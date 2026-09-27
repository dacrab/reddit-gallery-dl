package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	userAgent    = "golang:reddit-gallery-dl:v1.0.0 (by /u/reddit-gallery-dl)"
	maxJSONBytes = 2 * 1024 * 1024
	maxImageSize = 50 * 1024 * 1024
)

var (
	ErrInvalidURL    = errors.New("invalid reddit url")
	ErrPostNotFound  = errors.New("post not found or deleted")
	ErrNoImages      = errors.New("no images found in post")
	ErrRateLimited   = errors.New("reddit is rate limiting requests")
	ErrImageTooLarge = errors.New("image exceeds maximum allowed size")
	ErrBlocked       = errors.New("reddit and all public fallbacks blocked or unavailable")
)

// httpClient, noRedirectClient, and dlSem are package-level to keep the code
// simple. Tests temporarily replace httpClient with a local test server's
// client; those overrides are not safe for parallel test execution.
var (
	httpClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 5,
			MaxConnsPerHost:     10,
			IdleConnTimeout:     30 * time.Second,
		},
	}

	noRedirectClient = &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	dlSem = make(chan struct{}, 10)
)

type redditChild struct {
	Data redditPost `json:"data"`
}

type redditListing struct {
	Children []redditChild `json:"children"`
}

type redditResponse []struct {
	Data redditListing `json:"data"`
}

type redditVideo struct {
	FallbackURL string `json:"fallback_url"`
}

type redditMedia struct {
	RedditVideo *redditVideo `json:"reddit_video"`
}

type previewSource struct {
	URL string `json:"url"`
}

type previewVariant struct {
	Source previewSource `json:"source"`
}

type previewImage struct {
	Source   previewSource   `json:"source"`
	Variants previewVariants `json:"variants"`
}

type previewVariants struct {
	GIF *previewVariant `json:"gif"`
	MP4 *previewVariant `json:"mp4"`
}

type redditPreview struct {
	RedditVideoPreview *redditVideo   `json:"reddit_video_preview"`
	Images             []previewImage `json:"images"`
}

type galleryData struct {
	Items []galleryItem `json:"items"`
}

type galleryItem struct {
	MediaID string `json:"media_id"`
}

type mediaSource struct {
	U   string `json:"u"`
	Gif string `json:"gif"`
	Mp4 string `json:"mp4"`
}

type mediaMetadata struct {
	S mediaSource `json:"s"`
}

type redditPost struct {
	Title     string `json:"title"`
	IsGallery bool   `json:"is_gallery"`
	IsVideo   bool   `json:"is_video"`
	URL       string `json:"url_overridden_by_dest"`

	GalleryData   *galleryData             `json:"gallery_data"`
	MediaMetadata map[string]mediaMetadata `json:"media_metadata"`
	Media         *redditMedia             `json:"media"`
	Preview       *redditPreview           `json:"preview"`
}

type archivedResponse struct {
	Data []redditPost `json:"data"`
}

type gallery struct {
	Title  string
	Images []string
}

func redditRequest(ctx context.Context, rawURL string, acceptJSON bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.AddCookie(&http.Cookie{Name: "over18", Value: "1"})
	if acceptJSON {
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	}
	return req, nil
}

// doReddit performs a GET against Reddit's public, unauthenticated endpoints
// and transparently retries once when Reddit rate limits us with HTTP 429.
func doReddit(ctx context.Context, rawURL string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := redditRequest(ctx, rawURL, true)
		if err != nil {
			return nil, err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		_ = resp.Body.Close()
		if attempt > 0 {
			return nil, ErrRateLimited
		}
		wait := 2 * time.Second
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 && secs <= 10 {
			wait = time.Duration(secs) * time.Second
		}
		log.Printf("Rate limited, retrying in %v", wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// fetchGallery resolves a post URL and extracts its media without any Reddit
// account or app credentials: it first tries Reddit's public JSON endpoint and,
// whenever Reddit blocks or rate limits the anonymous request, falls back to
// other public sources that do not require authentication.
func fetchGallery(ctx context.Context, postURL string) (*gallery, error) {
	resolved, err := resolveURL(ctx, postURL)
	if err != nil {
		return nil, err
	}

	gallery, err := fetchGalleryFromJSON(ctx, resolved)
	if err == nil {
		return gallery, nil
	}
	// A deleted post or a media-less post will not show up in any fallback source.
	if errors.Is(err, ErrPostNotFound) || errors.Is(err, ErrNoImages) {
		return nil, err
	}

	log.Printf("reddit JSON unavailable (%v), trying no-auth fallbacks for %s", err, resolved)
	errs := []error{err}
	for _, fallback := range galleryFallbacks {
		gallery, fallbackErr := fallback.fetch(ctx, resolved)
		if fallbackErr == nil {
			return gallery, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", fallback.name, fallbackErr))
	}
	return nil, fmt.Errorf("%w: %w", ErrBlocked, errors.Join(errs...))
}

// redditJSONHosts all serve the same public, unauthenticated post JSON.
// Reddit's bot filters sometimes block one host but not the others, so every
// host is tried before giving up on Reddit itself.
var redditJSONHosts = []string{"www.reddit.com", "old.reddit.com", "api.reddit.com"}

func fetchGalleryFromJSON(ctx context.Context, resolved string) (*gallery, error) {
	u, err := url.Parse(resolved)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, host := range redditJSONHosts {
		u.Host = host
		gallery, err := fetchGalleryFromRedditHost(ctx, u.String())
		if err == nil {
			return gallery, nil
		}
		// A deleted post or a media-less post behaves identically on every host.
		if errors.Is(err, ErrPostNotFound) || errors.Is(err, ErrNoImages) {
			return nil, err
		}
		errs = append(errs, fmt.Errorf("%s: %w", host, err))
	}
	return nil, errors.Join(errs...)
}

func fetchGalleryFromRedditHost(ctx context.Context, resolved string) (*gallery, error) {
	// raw_json=1 asks Reddit for unescaped media URLs; the endpoint is public
	// and needs no OAuth token, client id, or account.
	endpoint := strings.TrimRight(resolved, "/") + ".json?raw_json=1"
	resp, err := doReddit(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var data redditResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(&data); err != nil {
			// A 200 response that is not JSON is Reddit's WAF serving a block
			// or interstitial page, so treat it like any other block.
			return nil, errors.New("got a non-JSON response (likely a block page)")
		}
		if len(data) == 0 || len(data[0].Data.Children) == 0 {
			return nil, ErrPostNotFound
		}
		return galleryFromPost(data[0].Data.Children[0].Data)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("reddit blocked anonymous access (status %d)", resp.StatusCode)
	case http.StatusNotFound:
		return nil, ErrPostNotFound
	default:
		return nil, fmt.Errorf("reddit api status: %d", resp.StatusCode)
	}
}

// galleryFallbacks are public, no-auth media sources tried in order when
// Reddit blocks the anonymous JSON API (rate limits, 401/403, outages).
var galleryFallbacks = []struct {
	name  string
	fetch func(context.Context, string) (*gallery, error)
}{
	{"embed.reddit.com", fetchGalleryFromEmbed},
	{"pullpush.io", fetchGalleryFromPullPush},
	{"arctic-shift", fetchGalleryFromArcticShift},
}

func galleryFromPost(post redditPost) (*gallery, error) {
	images := extractImages(post)
	if len(images) == 0 {
		return nil, ErrNoImages
	}
	return &gallery{Title: post.Title, Images: images}, nil
}

var (
	rxEmbedTitle     = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	rxHTMLTag        = regexp.MustCompile(`(?s)<[^>]+>`)
	rxEmbedVideo     = regexp.MustCompile(`https://v\.redd\.it/[a-z0-9._/-]+`)
	rxEmbedImage     = regexp.MustCompile(`https://(?:i|preview)\.redd\.it/[a-z0-9._-]+`)
	rxPreviewMediaID = regexp.MustCompile(`-v[0-9]+-([a-z0-9]+)(\.[a-z0-9]+)$`)
)

func fetchGalleryFromEmbed(ctx context.Context, resolved string) (*gallery, error) {
	u, err := url.Parse(resolved)
	if err != nil {
		return nil, err
	}
	embedURL := "https://embed.reddit.com" + strings.TrimRight(u.Path, "/") + "/"
	req, err := redditRequest(ctx, embedURL, false)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrPostNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed.reddit.com status: %d", resp.StatusCode)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, err
	}
	gallery := galleryFromEmbedHTML(string(b))
	if gallery == nil {
		return nil, ErrNoImages
	}
	return gallery, nil
}

func galleryFromEmbedHTML(page string) *gallery {
	title := ""
	if m := rxEmbedTitle.FindStringSubmatch(page); len(m) > 1 {
		title = strings.Join(strings.Fields(html.UnescapeString(rxHTMLTag.ReplaceAllString(m[1], " "))), " ")
	}

	var urls []string
	for _, videoURL := range rxEmbedVideo.FindAllString(page, -1) {
		urls = appendUniqueEmbedURL(urls, videoURL)
	}
	if len(urls) == 0 {
		for _, rawURL := range rxEmbedImage.FindAllString(page, -1) {
			if id := rxPreviewMediaID.FindStringSubmatch(rawURL); len(id) == 3 {
				rawURL = "https://i.redd.it/" + id[1] + id[2]
			}
			urls = appendUniqueEmbedURL(urls, rawURL)
		}
	}
	if len(urls) == 0 {
		return nil
	}
	return &gallery{Title: title, Images: urls}
}

func appendUniqueEmbedURL(urls []string, rawURL string) []string {
	for _, existing := range urls {
		if existing == rawURL {
			return urls
		}
	}
	return append(urls, rawURL)
}

func postIDFromURL(resolved string) (string, error) {
	u, err := url.Parse(resolved)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "comments" {
		return "", ErrInvalidURL
	}
	return parts[3], nil
}

func fetchGalleryFromPullPush(ctx context.Context, resolved string) (*gallery, error) {
	return fetchGalleryFromArchive(ctx, resolved, "https://api.pullpush.io/reddit/search/submission/?ids=")
}

func fetchGalleryFromArcticShift(ctx context.Context, resolved string) (*gallery, error) {
	return fetchGalleryFromArchive(ctx, resolved, "https://arctic-shift.photon-reddit.com/api/posts/ids?ids=")
}

func fetchGalleryFromArchive(ctx context.Context, resolved, endpointBase string) (*gallery, error) {
	postID, err := postIDFromURL(resolved)
	if err != nil {
		return nil, err
	}
	return fetchGalleryFromArchiveSource(ctx, endpointBase+url.QueryEscape(postID))
}

func fetchGalleryFromArchiveSource(ctx context.Context, endpoint string) (*gallery, error) {
	req, err := redditRequest(ctx, endpoint, true)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("post archive status: %d", resp.StatusCode)
	}
	var data archivedResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(&data); err != nil || len(data.Data) == 0 {
		return nil, ErrPostNotFound
	}
	return galleryFromPost(data.Data[0])
}

func streamImage(ctx context.Context, rawURL string) (io.ReadCloser, string, error) {
	req, err := redditRequest(ctx, rawURL, false)
	if err != nil {
		return nil, "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return &limitedImageBody{body: resp.Body, remaining: maxImageSize + 1}, detectExtension(rawURL, resp.Header.Get("Content-Type")), nil
}

// limitedImageBody caps an image stream at maxImageSize+1 bytes: reading past
// the cap returns ErrImageTooLarge instead of silently truncating, and Close
// drains the remainder so the connection returns to the keep-alive pool.
type limitedImageBody struct {
	body      io.ReadCloser
	remaining int64
}

func (b *limitedImageBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, ErrImageTooLarge
	}
	if int64(len(p)) > b.remaining {
		p = p[:int(b.remaining)]
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

func resolveURL(ctx context.Context, inputURL string) (string, error) {
	inputURL = strings.TrimSpace(inputURL)
	if !strings.HasPrefix(inputURL, "http") {
		inputURL = "https://" + inputURL
	}
	u, err := url.Parse(inputURL)
	if err != nil || u.Host == "" || !isRedditHost(u.Host) {
		return "", ErrInvalidURL
	}
	if isShareLink(u.Path) {
		u, err = resolveShareLink(ctx, inputURL)
		if err != nil {
			return "", err
		}
	}
	if !isPostPath(u.Path) {
		return "", ErrInvalidURL
	}
	return "https://www.reddit.com" + u.Path, nil
}

func isRedditHost(host string) bool {
	return host == "reddit.com" || strings.HasSuffix(host, ".reddit.com")
}

func isShareLink(p string) bool {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	return len(parts) == 4 && parts[0] == "r" && parts[2] == "s"
}

func isPostPath(p string) bool {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	return len(parts) >= 4 && parts[0] == "r" && parts[2] == "comments"
}

func resolveShareLink(ctx context.Context, shareURL string) (*url.URL, error) {
	req, err := redditRequest(ctx, shareURL, false)
	if err != nil {
		return nil, err
	}
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil, ErrInvalidURL
	}
	u, err := url.Parse(loc)
	if err != nil || !isRedditHost(u.Host) {
		return nil, ErrInvalidURL
	}
	return u, nil
}

func extractImages(post redditPost) []string {
	if post.IsGallery && post.GalleryData != nil {
		var urls []string
		for _, item := range post.GalleryData.Items {
			meta, ok := post.MediaMetadata[item.MediaID]
			if !ok {
				continue
			}
			gif := html.UnescapeString(meta.S.Gif)
			mp4 := html.UnescapeString(meta.S.Mp4)
			static := html.UnescapeString(meta.S.U)
			switch {
			case mp4 != "" && urlExt(mp4) == ".mp4":
				urls = append(urls, mp4)
			case gif != "":
				urls = append(urls, gif)
			case static != "":
				urls = append(urls, static)
			}
		}
		if len(urls) > 0 {
			return urls
		}
	}

	if post.IsVideo && post.Media != nil && post.Media.RedditVideo != nil {
		if u := stripQuery(post.Media.RedditVideo.FallbackURL); u != "" {
			return []string{u}
		}
	}

	if post.Preview != nil {
		if rvp := post.Preview.RedditVideoPreview; rvp != nil && rvp.FallbackURL != "" {
			return []string{stripQuery(rvp.FallbackURL)}
		}
		var urls []string
		for _, img := range post.Preview.Images {
			switch {
			case img.Variants.MP4 != nil:
				urls = append(urls, html.UnescapeString(img.Variants.MP4.Source.URL))
			case img.Variants.GIF != nil:
				urls = append(urls, html.UnescapeString(img.Variants.GIF.Source.URL))
			case img.Source.URL != "":
				urls = append(urls, html.UnescapeString(img.Source.URL))
			}
		}
		if len(urls) > 0 {
			return urls
		}
	}

	if post.URL != "" {
		return []string{html.UnescapeString(post.URL)}
	}
	return nil
}

func stripQuery(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		u.RawQuery = ""
		return u.String()
	}
	return raw
}

func urlExt(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return strings.ToLower(path.Ext(u.Path))
	}
	return strings.ToLower(path.Ext(rawURL))
}

var imageExts = map[string]bool{
	".png": true, ".gif": true, ".gifv": true, ".jpg": true,
	".jpeg": true, ".webp": true, ".mp4": true, ".webm": true, ".mov": true,
}

func detectExtension(urlStr, contentType string) string {
	if u, err := url.Parse(urlStr); err == nil {
		if ext := strings.ToLower(path.Ext(u.Path)); imageExts[ext] {
			return ext
		}
	}
	mediaType, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	}
	return ".jpg"
}
