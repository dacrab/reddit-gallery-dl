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
	"time"
)

const (
	maxJSONBytes = 2 * 1024 * 1024
	maxPageBytes = 8 * 1024 * 1024
	userAgent    = "golang:reddit-gallery-dl:v1.0.0 (by /u/reddit-gallery-dl)"
)

// Asking Reddit for text/html when fetching an image makes it answer 307 to
// its /media proxy instead of serving the file from the CDN, so media requests
// must advertise the media content types.
const (
	acceptJSON  = "application/json"
	acceptHTML  = "text/html,application/xhtml+xml,image/*;q=0.8,*/*;q=0.5"
	acceptMedia = "image/*,video/*,*/*;q=0.8"
)

var (
	ErrPostNotFound = errors.New("post not found or deleted")
	ErrNoMedia      = errors.New("post has no downloadable media")
	ErrUnavailable  = errors.New("no source could load this post")
)

// shareHosts are tried in turn for /r/<sub>/s/<code> links; Reddit's per-host
// defences differ, so one host refusing says little about the others.
var shareHosts = []string{"www.reddit.com", "old.reddit.com", "embed.reddit.com"}

// Fetcher loads posts and media. It owns its HTTP clients so tests can inject
// transports instead of mutating package-level state.
type Fetcher struct {
	client      *http.Client
	noRedirect  *http.Client
	mediaClient *http.Client
}

func NewFetcher() *Fetcher {
	// One transport, so every lookup and download shares a single connection
	// pool; the clients differ only in timeout and redirect policy.
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		MaxConnsPerHost:       16,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &Fetcher{
		client:     &http.Client{Timeout: 30 * time.Second, Transport: transport, CheckRedirect: guardRedirect(isRedditHost)},
		noRedirect: &http.Client{Timeout: 10 * time.Second, Transport: transport, CheckRedirect: refuseRedirect},
		// Media may redirect to www.reddit.com/media, which proxies the file;
		// the property that matters is that the chain never leaves Reddit.
		mediaClient: &http.Client{Timeout: 60 * time.Second, Transport: transport, CheckRedirect: guardRedirect(isRedditHost)},
	}
}

// guardRedirect returns a CheckRedirect that re-checks every hop: redirects are
// otherwise followed blindly, which can walk a request to an internal address.
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

// source is one way of loading a post: it returns a gallery, a definitive
// ErrNoMedia when it positively read the post and found nothing downloadable,
// or an inconclusive error.
type source struct {
	name  string
	fetch func(context.Context, *Fetcher, postRef) (*gallery, error)
}

// sources is the fallback ladder. The archives come first because they return
// the same JSON shape Reddit does and do not depend on page markup; the embed
// page is scraped; Reddit's own endpoint is last because it currently answers
// anonymous requests from datacentre IPs with 403s.
var sources = []source{
	{"arctic-shift", fetchFromArcticShift},
	{"embed", fetchFromEmbed},
	{"pullpush", fetchFromPullPush},
	{"reddit", fetchFromRedditJSON},
}

// Gallery loads a post's media, trying each source in turn. A block is never
// proof of deletion: only a source that actually read the post may declare it
// missing, and "deleted" is only reported when every source agrees.
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

// resolveShare follows a /r/<sub>/s/<code> share link to the post behind it and
// returns the full post reference.
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
	if err := f.getJSON(ctx, endpoint, &payload); err != nil {
		return nil, err
	}
	if len(payload.Data) == 0 {
		return nil, ErrPostNotFound
	}
	return galleryFromPost(payload.Data[0])
}

func fetchFromRedditJSON(ctx context.Context, f *Fetcher, ref postRef) (*gallery, error) {
	// raw_json=1 asks Reddit for unescaped media URLs.
	var payload []struct {
		Data struct {
			Children []struct {
				Data redditPost `json:"data"`
			} `json:"children"`
		} `json:"data"`
	}
	endpoint := "https://www.reddit.com/comments/" + url.PathEscape(ref.postID) + ".json?raw_json=1"
	if err := f.getJSON(ctx, endpoint, &payload); err != nil {
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
// The subreddit must be in the path: a slug-free URL answers 200 with a
// "Page not found" body, looking like success while returning nothing.
//
// Zero images is never proof of a media-less post — the markup may be a text
// post, a block page or a redesign — so every failure here is inconclusive.
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
	if gallery == nil || len(gallery.Media) == 0 {
		return nil, errors.New("embed page exposed no media (text post, missing post, block page, or markup change)")
	}
	return gallery, nil
}

func galleryFromPost(post redditPost) (*gallery, error) {
	media := extractMedia(post)
	if len(media) == 0 {
		return nil, ErrNoMedia
	}
	return &gallery{Title: post.Title, Media: media}, nil
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
// Images are read from <img> tags rather than by scanning for redd.it URLs: a
// document-wide scan also matches stylesheets, preload links and social preview
// tags. Host filtering is left to redditMediaURLs.
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
	if media := redditMediaURLs(candidates); len(media) > 0 {
		return &gallery{Title: title, Media: media}
	}
	if media := redditMediaURLs(rxEmbedVideo.FindAllString(page, -1)); len(media) > 0 {
		return &gallery{Title: title, Media: media}
	}
	return nil
}

// --- http helpers --------------------------------------------------------

func (f *Fetcher) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := f.newRequest(ctx, endpoint, acceptJSON)
	if err != nil {
		return err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// A login wall means we were never actually authorised to an answer,
		// so it must not be reported as a missing post.
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

// streamImage opens a Reddit-hosted image for reading. The caller owns the
// returned body and must close it.
func (f *Fetcher) streamImage(ctx context.Context, rawURL string) (io.ReadCloser, string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", ErrInvalidURL
	}
	if parsed.Scheme != "https" || !isRedditMediaHost(parsed.Host) {
		return nil, "", fmt.Errorf("%w: refusing to fetch %q", ErrInvalidURL, parsed.Host)
	}

	req, err := f.newRequest(ctx, rawURL, acceptMedia)
	if err != nil {
		return nil, "", err
	}
	resp, err := f.mediaClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.Body, detectExtension(rawURL, resp.Header.Get("Content-Type")), nil
}
