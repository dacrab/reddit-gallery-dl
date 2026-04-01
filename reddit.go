package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	userAgent      = "golang:reddit-gallery-dl:v1.0.0 (by /u/reddit-gallery-dl)"
	defaultTimeout = 30 * time.Second
)

var (
	ErrInvalidURL   = errors.New("invalid reddit url")
	ErrPostNotFound = errors.New("post not found or deleted")
	ErrNoImages     = errors.New("no images found in post")
	ErrRateLimited  = errors.New("reddit is rate limiting requests")
)

// ── JSON types ────────────────────────────────────────────────────────────────

// redditResponse is the top-level array returned by the Reddit post JSON API.
type redditResponse []struct {
	Data struct {
		Children []struct {
			Data redditPost `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type redditPost struct {
	Title     string `json:"title"`
	IsGallery bool   `json:"is_gallery"`
	IsVideo   bool   `json:"is_video"`
	URL       string `json:"url_overridden_by_dest"`

	// GalleryData lists media IDs in display order for multi-image posts.
	GalleryData *struct {
		Items []struct {
			MediaID string `json:"media_id"`
		} `json:"items"`
	} `json:"gallery_data"`

	// MediaMetadata is keyed by media ID. E is the media type
	// ("Image" or "AnimatedImage"); S holds the full-resolution source URLs.
	MediaMetadata map[string]struct {
		E string `json:"e"`
		S struct {
			U   string `json:"u"`   // static image
			Gif string `json:"gif"` // direct GIF (i.redd.it)
			Mp4 string `json:"mp4"` // may be a ?format=mp4 query trick, not a real .mp4 path
		} `json:"s"`
	} `json:"media_metadata"`

	Media *struct {
		RedditVideo *struct {
			FallbackURL string `json:"fallback_url"`
		} `json:"reddit_video"`
	} `json:"media"`

	Preview *struct {
		RedditVideoPreview *struct {
			FallbackURL string `json:"fallback_url"`
		} `json:"reddit_video_preview"`
		Images []struct {
			Source   struct{ URL string `json:"url"` } `json:"source"`
			Variants struct {
				GIF *struct {
					Source struct{ URL string `json:"url"` } `json:"source"`
				} `json:"gif"`
				MP4 *struct {
					Source struct{ URL string `json:"url"` } `json:"source"`
				} `json:"mp4"`
			} `json:"variants"`
		} `json:"images"`
	} `json:"preview"`
}

// ── HTTP client ───────────────────────────────────────────────────────────────

type RedditClient struct {
	client           *http.Client
	noRedirectClient *http.Client // used only for resolving share links (single hop)
	rl               rateLimiter
}

func NewRedditClient() *RedditClient {
	// HTTP/2 is disabled: Reddit's CDN rate-limits non-browser HTTP/2 clients.
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13},
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     60 * time.Second,
	}
	return &RedditClient{
		client: &http.Client{Timeout: defaultTimeout, Transport: tr},
		noRedirectClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// request builds a GET request with the Reddit user-agent and over-18 cookie.
// Set acceptJSON to true for API endpoints to ensure a JSON response and prevent
// Reddit from redirecting to a localised domain.
func (r *RedditClient) request(ctx context.Context, rawURL string, acceptJSON bool) (*http.Request, error) {
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

// ── Rate limiter ──────────────────────────────────────────────────────────────

// rateLimiter serialises outgoing Reddit API calls using PRAW's algorithm.
// Holding the mutex across the sleep ensures concurrent callers queue rather
// than burst.
type rateLimiter struct{ mu sync.Mutex }

func (rl *rateLimiter) wait(ctx context.Context, h http.Header) error {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	remaining := h.Get("X-Ratelimit-Remaining")
	if remaining == "" {
		return nil
	}
	rem, _ := strconv.ParseFloat(remaining, 64)
	used, _ := strconv.ParseFloat(h.Get("X-Ratelimit-Used"), 64)
	reset, _ := strconv.ParseFloat(h.Get("X-Ratelimit-Reset"), 64)

	var sleep float64
	if rem <= 0 {
		sleep = min(max(reset, 1), 2)
		slog.Warn("Reddit rate limit exhausted, waiting", "wait_s", sleep)
	} else {
		// PRAW algorithm: spread remaining quota evenly across the reset window.
		ideal := reset - reset*(1-used/(rem+used))
		sleep = min(max(ideal, 0), 2.0)
	}
	if sleep <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(sleep * float64(time.Second))):
		return nil
	}
}

// ── API calls ─────────────────────────────────────────────────────────────────

// do executes a Reddit JSON API request, applies rate limiting, and retries
// once on HTTP 429.
func (r *RedditClient) do(ctx context.Context, rawURL string) (*http.Response, error) {
	for attempt := range 2 {
		req, err := r.request(ctx, rawURL, true)
		if err != nil {
			return nil, err
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			if err := r.rl.wait(ctx, resp.Header); err != nil {
				resp.Body.Close()
				return nil, err
			}
			return resp, nil
		}
		resp.Body.Close()
		if attempt == 1 {
			break
		}
		wait := 2 * time.Second
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 && secs <= 10 {
				wait = time.Duration(secs) * time.Second
			}
		}
		slog.Warn("Reddit rate limited, retrying", "wait", wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, ErrRateLimited
}

// Gallery is the result returned by FetchGallery.
type Gallery struct {
	Title  string
	Images []string
}

// FetchGallery resolves the URL, fetches the post JSON, and returns the ordered
// list of media URLs ready for display and download.
func (r *RedditClient) FetchGallery(ctx context.Context, postURL string) (*Gallery, error) {
	resolved, err := r.resolveURL(ctx, postURL)
	if err != nil {
		return nil, err
	}

	resp, err := r.do(ctx, strings.TrimRight(resolved, "/")+".json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusForbidden:
		return nil, ErrPostNotFound
	default:
		return nil, fmt.Errorf("reddit api status: %d", resp.StatusCode)
	}

	var data redditResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || len(data) == 0 || len(data[0].Data.Children) == 0 {
		return nil, ErrPostNotFound
	}

	post := data[0].Data.Children[0].Data
	images := extractImages(post)
	if len(images) == 0 {
		return nil, ErrNoImages
	}
	return &Gallery{Title: post.Title, Images: images}, nil
}

// StreamImage fetches a media URL and returns the response body, inferred file
// extension, and any error. The caller must close the body.
func (r *RedditClient) StreamImage(ctx context.Context, rawURL string) (io.ReadCloser, string, error) {
	req, err := r.request(ctx, rawURL, false)
	if err != nil {
		return nil, "", err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.Body, detectExtension(rawURL, resp.Header.Get("Content-Type")), nil
}

// ── URL resolution ────────────────────────────────────────────────────────────

// resolveURL normalises and validates the input, following share-link redirects
// when needed, and returns a canonical reddit.com post URL.
func (r *RedditClient) resolveURL(ctx context.Context, inputURL string) (string, error) {
	inputURL = strings.TrimSpace(inputURL)
	if !strings.HasPrefix(inputURL, "http") {
		inputURL = "https://" + inputURL
	}
	u, err := url.Parse(inputURL)
	if err != nil || u.Host == "" || !strings.Contains(u.Host, "reddit.com") {
		return "", ErrInvalidURL
	}
	if isShareLink(u.Path) {
		u, err = r.resolveShareLink(ctx, inputURL)
		if err != nil {
			return "", err
		}
	}
	if !isPostPath(u.Path) {
		return "", ErrInvalidURL
	}
	return "https://www.reddit.com" + u.Path, nil
}

// isShareLink matches short share URLs of the form /r/{sub}/s/{id}.
func isShareLink(p string) bool {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	return len(parts) == 4 && parts[0] == "r" && parts[2] == "s"
}

// isPostPath matches full post URLs of the form /r/{sub}/comments/{id}/...
func isPostPath(p string) bool {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	return len(parts) >= 4 && parts[0] == "r" && parts[2] == "comments"
}

// resolveShareLink follows the single redirect issued by a Reddit share link
// and returns the resolved URL.
func (r *RedditClient) resolveShareLink(ctx context.Context, shareURL string) (*url.URL, error) {
	req, err := r.request(ctx, shareURL, false)
	if err != nil {
		return nil, err
	}
	resp, err := r.noRedirectClient.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil, ErrInvalidURL
	}
	u, err := url.Parse(loc)
	if err != nil || !strings.Contains(u.Host, "reddit.com") {
		return nil, ErrInvalidURL
	}
	return u, nil
}

// ── Image extraction ──────────────────────────────────────────────────────────

// extractImages pulls ordered media URLs from a post, trying each source in
// priority order: gallery → hosted video → preview → direct URL fallback.
func extractImages(post redditPost) []string {
	// 1. Multi-image gallery — GalleryData preserves the author's ordering.
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
			// Prefer the direct i.redd.it GIF: clean .gif path, plays in <img>.
			// The mp4 field for AnimatedImage is a ?format=mp4 query trick whose
			// URL path still ends in .gif — serving it in <video> would break.
			// Only accept mp4 when the path genuinely ends in .mp4.
			case gif != "":
				urls = append(urls, gif)
			case mp4 != "" && urlExt(mp4) == ".mp4":
				urls = append(urls, mp4)
			case static != "":
				urls = append(urls, static)
			}
		}
		if len(urls) > 0 {
			return urls
		}
	}

	// 2. Reddit-hosted video (v.redd.it). Strip query params for a clean URL.
	if post.IsVideo && post.Media != nil && post.Media.RedditVideo != nil {
		if u := stripQuery(post.Media.RedditVideo.FallbackURL); u != "" {
			return []string{u}
		}
	}

	// 3. Preview block: video preview → animated variant → static source.
	if post.Preview != nil {
		if rvp := post.Preview.RedditVideoPreview; rvp != nil && rvp.FallbackURL != "" {
			return []string{stripQuery(rvp.FallbackURL)}
		}
		var urls []string
		for _, img := range post.Preview.Images {
			switch {
			// preview.redd.it variant URLs carry reliable path extensions, so
			// MP4 is preferred here for smooth playback.
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

	// 4. Direct URL fallback for single-image or external-link posts.
	if post.URL != "" {
		return []string{html.UnescapeString(post.URL)}
	}
	return nil
}

// stripQuery removes the query string from a URL, returning the original string
// unchanged if parsing fails.
func stripQuery(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		u.RawQuery = ""
		return u.String()
	}
	return raw
}
