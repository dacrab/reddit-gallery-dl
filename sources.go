package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxJSONBytes = 2 * 1024 * 1024
	maxPageBytes = 8 * 1024 * 1024
	maxImageSize = 50 * 1024 * 1024
	userAgent    = "golang:reddit-gallery-dl:v1.0.0 (by /u/reddit-gallery-dl)"
)

// Accept headers are per-request-type and not interchangeable. Asking for
// text/html when fetching an image makes Reddit 307 the request to
// www.reddit.com/media instead of serving the file from its CDN, so media
// requests have to advertise that they want an image.
const (
	acceptJSON  = "application/json"
	acceptHTML  = "text/html,application/xhtml+xml,image/*;q=0.8,*/*;q=0.5"
	acceptMedia = "image/*,video/*,*/*;q=0.8"
)

var (
	ErrPostNotFound  = errors.New("post not found or deleted")
	ErrNoMedia       = errors.New("post has no downloadable media")
	ErrUnavailable   = errors.New("no source could load this post")
	ErrImageTooLarge = errors.New("image exceeds maximum allowed size")
)

// shareHosts are tried in turn to resolve a /r/<sub>/s/<code> link. Reddit's
// per-host defences differ, so one host refusing a share link says little about
// the others.
var shareHosts = []string{"www.reddit.com", "old.reddit.com", "embed.reddit.com"}

// Fetcher loads posts and media. It owns its HTTP clients so that tests can
// inject transports instead of mutating package-level state, which is what
// previously made every test in this repository unparallelisable.
type Fetcher struct {
	client      *http.Client
	noRedirect  *http.Client
	mediaClient *http.Client

	// downloadSem caps image downloads across the whole process, so a handful of
	// concurrent archive requests cannot exhaust file descriptors or upstream
	// rate limits.
	downloadSem chan struct{}
}

func NewFetcher() *Fetcher {
	transport := func() *http.Transport {
		return &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   8,
			MaxConnsPerHost:       16,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	return &Fetcher{
		client:     &http.Client{Timeout: 30 * time.Second, Transport: transport(), CheckRedirect: guardRedirect(isRedditHost)},
		noRedirect: &http.Client{Timeout: 10 * time.Second, Transport: transport(), CheckRedirect: refuseRedirect},
		// The first hop must be a media host, but a redirect may legitimately
		// land on www.reddit.com, which proxies media through /media. The
		// property that matters is that the chain never leaves Reddit.
		mediaClient: &http.Client{Timeout: 60 * time.Second, Transport: transport(), CheckRedirect: guardRedirect(isRedditHost)},
		downloadSem: make(chan struct{}, maxParallelDownloads),
	}
}

// guardRedirect returns a CheckRedirect that re-checks every hop against a host
// predicate. Redirects are followed blindly otherwise, which is enough to walk
// a request straight back out to an internal address.
func guardRedirect(allow func(string) bool) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" || !allow(req.URL.Host) {
			return fmt.Errorf("redirect to disallowed host %q", req.URL.Host)
		}
		return nil
	}
}

func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func (f *Fetcher) newRequest(ctx context.Context, rawURL, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept", accept)
	return req, nil
}

// source is one way of loading a post. Every source returns a gallery, a
// definitive ErrNoMedia when it positively read the post and found nothing
// downloadable, or an inconclusive error.
type source struct {
	name  string
	fetch func(context.Context, *Fetcher, postRef) (*gallery, error)
}

// sources is the fallback ladder, ordered by how each source has actually
// behaved against anonymous traffic rather than by how official it sounds.
//
// The archives come first because they return the same JSON shape Reddit does
// and do not depend on page markup, so they cannot be broken by a front-end
// redesign. The embed page is next. Reddit's own endpoint is last because it
// currently answers anonymous requests from datacentre IPs with 403, and
// because old.reddit.com answers with a redirect to a login page — which is
// exactly the trap this ladder previously fell into.
var sources = []source{
	{"arctic-shift", fetchFromArcticShift},
	{"embed", fetchFromEmbed},
	{"pullpush", fetchFromPullPush},
	{"reddit", fetchFromRedditJSON},
}

// Gallery loads a post's media, trying each source in turn.
//
// Nothing short-circuits on "not found" except a source that actually read the
// post. A blocked request, a login wall and a deleted post are indistinguishable
// from the outside, and treating a block as proof of deletion is what made this
// tool tell users their live posts were missing.
func (f *Fetcher) Gallery(ctx context.Context, input string) (*gallery, error) {
	ref, err := f.resolve(ctx, input)
	if err != nil {
		return nil, err
	}

	var errs []error
	for _, s := range sources {
		gallery, err := s.fetch(ctx, f, ref)
		if err == nil {
			return gallery, nil
		}
		// Only a source that read the post may declare it media-less.
		if errors.Is(err, ErrNoMedia) {
			return nil, err
		}
		errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
	}

	joined := errors.Join(errs...)
	if allMissing(errs) {
		return nil, fmt.Errorf("%w: %w", ErrPostNotFound, joined)
	}
	return nil, fmt.Errorf("%w: %w", ErrUnavailable, joined)
}

// allMissing reports whether every source positively said the post does not
// exist, which is the only evidence strong enough to tell a user it is gone.
func allMissing(errs []error) bool {
	if len(errs) == 0 {
		return false
	}
	for _, err := range errs {
		if !errors.Is(err, ErrPostNotFound) {
			return false
		}
	}
	return true
}

func (f *Fetcher) resolve(ctx context.Context, input string) (postRef, error) {
	l, err := parseLink(input)
	if err != nil {
		return postRef{}, err
	}
	if l.ref.postID != "" {
		return l.ref, nil
	}
	return f.resolveShare(ctx, l.share)
}

// resolveShare follows a /r/<sub>/s/<code> share link to the post behind it.
// The resolved post is returned in full, subreddit included, so a share link
// behaves exactly like the equivalent pasted URL.
func (f *Fetcher) resolveShare(ctx context.Context, share string) (postRef, error) {
	var errs []error
	for _, host := range shareHosts {
		req, err := f.newRequest(ctx, "https://"+host+share+"/", acceptHTML)
		if err != nil {
			return postRef{}, err
		}
		resp, err := f.noRedirect.Do(req)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", host, err))
			continue
		}
		location := resp.Header.Get("Location")
		_ = resp.Body.Close()
		if location == "" {
			errs = append(errs, fmt.Errorf("%s: no redirect (status %d)", host, resp.StatusCode))
			continue
		}
		absolute, err := req.URL.Parse(location)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", host, err))
			continue
		}
		resolved, err := parseLink(absolute.String())
		if err != nil || resolved.ref.postID == "" {
			errs = append(errs, fmt.Errorf("%s: share link did not resolve to a post", host))
			continue
		}
		return resolved.ref, nil
	}
	return postRef{}, fmt.Errorf("%w: could not resolve share link, paste the full post URL instead: %w",
		ErrUnavailable, errors.Join(errs...))
}

// --- sources -------------------------------------------------------------

func fetchFromArcticShift(ctx context.Context, f *Fetcher, ref postRef) (*gallery, error) {
	return f.archivePost(ctx, "https://arctic-shift.photon-reddit.com/api/posts/ids?ids=", ref.postID)
}

func fetchFromPullPush(ctx context.Context, f *Fetcher, ref postRef) (*gallery, error) {
	return f.archivePost(ctx, "https://api.pullpush.io/reddit/search/submission/?ids=", ref.postID)
}

func (f *Fetcher) archivePost(ctx context.Context, endpointBase, postID string) (*gallery, error) {
	var payload struct {
		Data []redditPost `json:"data"`
	}
	endpoint := endpointBase + url.QueryEscape(postID)
	if err := f.getJSON(ctx, f.client, endpoint, &payload); err != nil {
		return nil, err
	}
	if len(payload.Data) == 0 {
		return nil, ErrPostNotFound
	}
	return galleryFromPost(payload.Data[0])
}

func fetchFromRedditJSON(ctx context.Context, f *Fetcher, ref postRef) (*gallery, error) {
	// raw_json=1 asks Reddit for unescaped media URLs. The endpoint is public
	// and needs no OAuth token, client id, or account.
	var payload []struct {
		Data struct {
			Children []struct {
				Data redditPost `json:"data"`
			} `json:"children"`
		} `json:"data"`
	}
	endpoint := "https://www.reddit.com/comments/" + url.PathEscape(ref.postID) + ".json?raw_json=1"
	if err := f.getJSON(ctx, f.client, endpoint, &payload); err != nil {
		return nil, err
	}
	if len(payload) == 0 || len(payload[0].Data.Children) == 0 {
		return nil, ErrPostNotFound
	}
	return galleryFromPost(payload[0].Data.Children[0].Data)
}

// fetchFromEmbed scrapes Reddit's embed page, which is reachable without an
// account even when the JSON endpoints are not.
//
// The subreddit has to be in the request path: embed.reddit.com/comments/<id>/
// answers HTTP 200 with a "Page not found" body and no media, so a slug-free
// request looks like success while returning nothing.
//
// It can only ever return an inconclusive error. Zero images means "this markup
// told us nothing", which might be a text post, a missing post, a block page, or
// a front-end redesign — never proof that a post has no media, and never a
// reason to stop trying the sources behind it.
func fetchFromEmbed(ctx context.Context, f *Fetcher, ref postRef) (*gallery, error) {
	if ref.subreddit == "" {
		return nil, errors.New("no subreddit in link, cannot build an embed URL")
	}
	embedURL := "https://embed.reddit.com/r/" + url.PathEscape(ref.subreddit) +
		"/comments/" + url.PathEscape(ref.postID) + "/"
	req, err := f.newRequest(ctx, embedURL, acceptHTML)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrPostNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed status: %d", resp.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return nil, err
	}
	gallery := galleryFromEmbedHTML(string(page))
	if gallery == nil || len(gallery.Images) == 0 {
		return nil, errors.New("embed page exposed no media (text post, missing post, block page, or markup change)")
	}
	return gallery, nil
}

func galleryFromPost(post redditPost) (*gallery, error) {
	images := extractImages(post)
	if len(images) == 0 {
		return nil, ErrNoMedia
	}
	return &gallery{Title: post.Title, Images: images}, nil
}

// --- embed page parsing --------------------------------------------------

var (
	rxEmbedTitle  = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	rxHTMLTag     = regexp.MustCompile(`(?s)<[^>]+>`)
	rxHTMLComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	rxEmbedImg    = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	rxAttrSrc     = regexp.MustCompile(`(?is)\bsrc\s*=\s*["']([^"']+)["']`)
	rxEmbedVideo  = regexp.MustCompile(`https://v\.redd\.it/[A-Za-z0-9._/-]+`)
)

// galleryFromEmbedHTML pulls the title and media out of an embed page.
//
// Images are read from <img> tags rather than by scanning the whole document
// for anything that looks like a redd.it URL, because a document-wide scan also
// matches stylesheets, preload links and social preview tags, and silently
// returns those instead of the post's actual media. redditMediaURLs then does
// the host filtering and deduplication.
func galleryFromEmbedHTML(page string) *gallery {
	// Comments go first: a page that documents its own markup in a comment
	// would otherwise feed the tag regexes text that is not part of the DOM.
	page = rxHTMLComment.ReplaceAllString(page, "")

	title := ""
	if m := rxEmbedTitle.FindStringSubmatch(page); len(m) > 1 {
		title = strings.Join(strings.Fields(html.UnescapeString(rxHTMLTag.ReplaceAllString(m[1], " "))), " ")
	}

	var candidates []string
	for _, tag := range rxEmbedImg.FindAllString(page, -1) {
		if m := rxAttrSrc.FindStringSubmatch(tag); len(m) > 1 {
			candidates = append(candidates, m[1])
		}
	}
	if images := redditMediaURLs(candidates); len(images) > 0 {
		return &gallery{Title: title, Images: images}
	}
	if videos := redditMediaURLs(rxEmbedVideo.FindAllString(page, -1)); len(videos) > 0 {
		return &gallery{Title: title, Images: videos}
	}
	return nil
}

// --- http helpers --------------------------------------------------------

func (f *Fetcher) getJSON(ctx context.Context, client *http.Client, endpoint string, out any) error {
	req, err := f.newRequest(ctx, endpoint, acceptJSON)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// A login wall means we were never actually authorised to an answer, so
		// it must not be reported as a missing post.
		if redirectedToLogin(resp) {
			return errors.New("reddit requires a login for anonymous access")
		}
		return fmt.Errorf("status: %d", resp.StatusCode)
	}
	// A 200 that is not JSON is a WAF block page or an interstitial.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(out); err != nil {
		return fmt.Errorf("response was not json (likely a block page): %w", err)
	}
	return nil
}

func redirectedToLogin(resp *http.Response) bool {
	return resp.Request != nil && strings.Contains(resp.Request.URL.Path, "/login")
}

// streamImage opens a Reddit-hosted image for reading.
func (f *Fetcher) streamImage(ctx context.Context, rawURL string) (io.ReadCloser, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", ErrInvalidURL
	}
	if parsed.Scheme != "https" || !isRedditMediaHost(parsed.Host) {
		return nil, "", fmt.Errorf("%w: refusing to fetch %q", ErrInvalidURL, parsed.Host)
	}

	select {
	case f.downloadSem <- struct{}{}:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	release := func() { <-f.downloadSem }

	req, err := f.newRequest(ctx, rawURL, acceptMedia)
	if err != nil {
		release()
		return nil, "", err
	}
	resp, err := f.mediaClient.Do(req)
	if err != nil {
		release()
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		release()
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	// maxImageSize+1 so the reader can detect an oversized body rather than
	// truncating it silently.
	body := &limitedImageBody{body: resp.Body, remaining: maxImageSize + 1}
	stream := &downloadBody{ReadCloser: body, release: release}
	return stream, detectExtension(rawURL, resp.Header.Get("Content-Type")), nil
}

// downloadBody holds a download slot for the lifetime of the stream.
//
// Releasing the slot when the response headers arrive would cap nothing: every
// request would take a slot, drop it immediately, and then sit on an open
// connection reading a multi-megabyte image. The slot has to be given back when
// the body is closed.
type downloadBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *downloadBody) Close() error {
	b.once.Do(b.release)
	return b.ReadCloser.Close()
}

// limitedImageBody caps an image stream at maxImageSize. Reading past the cap
// returns ErrImageTooLarge instead of silently truncating, and Close drains the
// remainder so the connection returns to the keep-alive pool.
type limitedImageBody struct {
	body      io.ReadCloser
	remaining int64
}

func (b *limitedImageBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, ErrImageTooLarge
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.body.Read(p)
	b.remaining -= int64(n)
	if n > 0 && b.remaining == 0 {
		err = ErrImageTooLarge
	}
	return n, err
}

func (b *limitedImageBody) Close() error {
	_, _ = io.Copy(io.Discard, b.body)
	return b.body.Close()
}
