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

// rateLimiter serialises outgoing Reddit API requests using PRAW's algorithm.
// The mutex is held across the sleep so concurrent callers queue up rather than
// all firing at once.
type rateLimiter struct {
	mu sync.Mutex
}

func (rl *rateLimiter) wait(ctx context.Context, h http.Header) error {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	remaining := h.Get("X-Ratelimit-Remaining")
	if remaining == "" {
		return nil
	}
	r, _ := strconv.ParseFloat(remaining, 64)
	u, _ := strconv.ParseFloat(h.Get("X-Ratelimit-Used"), 64)
	s, _ := strconv.ParseFloat(h.Get("X-Ratelimit-Reset"), 64)

	var sleep float64
	if r <= 0 {
		sleep = min(max(s, 1), 2)
		slog.Warn("Reddit rate limit exhausted, waiting", "wait_s", sleep)
	} else {
		// PRAW algorithm: spread remaining requests across the reset window.
		ideal := s - s*(1-u/(r+u))
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

type RedditClient struct {
	client           *http.Client
	noRedirectClient *http.Client
	rl               rateLimiter
}

func NewRedditClient() *RedditClient {
	// HTTP/2 disabled — Reddit CDN rate-limits non-browser HTTP/2 clients.
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13},
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     60 * time.Second,
	}
	return &RedditClient{
		client: &http.Client{Timeout: defaultTimeout, Transport: transport},
		noRedirectClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type Gallery struct {
	Title  string
	Images []string
}

// redditResponse is the top-level JSON structure returned by the Reddit post API.
type redditResponse []struct {
	Data struct {
		Children []struct {
			Data redditPost `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type redditPost struct {
	Title         string `json:"title"`
	IsGallery     bool   `json:"is_gallery"`
	IsVideo       bool   `json:"is_video"`
	URL           string `json:"url_overridden_by_dest"`
	GalleryData   *struct {
		Items []struct {
			MediaID string `json:"media_id"`
		} `json:"items"`
	} `json:"gallery_data"`
	MediaMetadata map[string]struct {
		S struct {
			U   string `json:"u"`
			Gif string `json:"gif"`
			Mp4 string `json:"mp4"`
		} `json:"s"`
	} `json:"media_metadata"`
	Media *struct {
		RedditVideo *struct {
			FallbackURL string `json:"fallback_url"`
		} `json:"reddit_video"`
	} `json:"media"`
	Preview *struct {
		Images []struct {
			Variants struct {
				Gif *struct {
					Source struct{ URL string `json:"url"` } `json:"source"`
				} `json:"gif"`
				Mp4 *struct {
					Source struct{ URL string `json:"url"` } `json:"source"`
				} `json:"mp4"`
			} `json:"variants"`
			Source struct{ URL string `json:"url"` } `json:"source"`
		} `json:"images"`
		RedditVideoPreview *struct {
			FallbackURL string `json:"fallback_url"`
		} `json:"reddit_video_preview"`
	} `json:"preview"`
}

func newRequest(ctx context.Context, targetURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.AddCookie(&http.Cookie{Name: "over18", Value: "1"})
	return req, nil
}

// apiRequest builds a request for Reddit's JSON API endpoints.
func apiRequest(ctx context.Context, targetURL string) (*http.Request, error) {
	req, err := newRequest(ctx, targetURL)
	if err != nil {
		return nil, err
	}
	// Accept ensures we always get JSON, never an HTML fallback.
	// Accept-Language prevents Reddit redirecting to localised domains.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	return req, nil
}

// do executes an API request with PRAW-style rate limiting and one retry on 429.
// Rate limit headers are consumed *after* a successful response so the mutex
// serialises concurrent callers and prevents burst requests.
func (r *RedditClient) do(ctx context.Context, targetURL string) (*http.Response, error) {
	for attempt := range 2 {
		req, err := apiRequest(ctx, targetURL)
		if err != nil {
			return nil, err
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			// Good response — apply rate limit delay before returning so the
			// next caller is held off appropriately.
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

func (r *RedditClient) FetchGallery(ctx context.Context, postURL string) (*Gallery, error) {
	resolvedURL, err := r.resolveURL(ctx, postURL)
	if err != nil {
		return nil, err
	}

	resp, err := r.do(ctx, strings.TrimRight(resolvedURL, "/")+".json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// ok
	case http.StatusNotFound, http.StatusForbidden:
		return nil, ErrPostNotFound
	default:
		return nil, fmt.Errorf("reddit api status: %d", resp.StatusCode)
	}

	var data redditResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, ErrPostNotFound // malformed response treated as not found
	}
	if len(data) == 0 || len(data[0].Data.Children) == 0 {
		return nil, ErrPostNotFound
	}

	post := data[0].Data.Children[0].Data
	images := extractImages(post)
	if len(images) == 0 {
		return nil, ErrNoImages
	}
	return &Gallery{Title: post.Title, Images: images}, nil
}

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

// isShareLink matches short share URLs: /r/{sub}/s/{id}
func isShareLink(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 4 && parts[0] == "r" && parts[2] == "s"
}

// isPostPath matches full post URLs: /r/{sub}/comments/{id}/...
func isPostPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) >= 4 && parts[0] == "r" && parts[2] == "comments"
}

func (r *RedditClient) resolveShareLink(ctx context.Context, shareURL string) (*url.URL, error) {
	req, err := newRequest(ctx, shareURL)
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

func (r *RedditClient) StreamImage(ctx context.Context, urlStr string) (io.ReadCloser, string, error) {
	req, err := newRequest(ctx, urlStr)
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
	return resp.Body, detectExtension(urlStr, resp.Header.Get("Content-Type")), nil
}

// extractImages pulls media URLs from a post, trying each source in priority order:
// gallery > hosted video > video preview > image preview > direct URL.
// Reddit caps gallery size at 20 images.
func extractImages(post redditPost) []string {
	// 1. Multi-image gallery
	if post.IsGallery && post.GalleryData != nil {
		var images []string
		for _, item := range post.GalleryData.Items {
			meta, ok := post.MediaMetadata[item.MediaID]
			if !ok {
				continue
			}
			// Prefer mp4 > gif > static image
			for _, u := range []string{meta.S.Mp4, meta.S.Gif, meta.S.U} {
				if u != "" {
					images = append(images, html.UnescapeString(u))
					break
				}
			}
		}
		if len(images) > 0 {
			return images
		}
	}

	// 2. Reddit-hosted video
	if post.IsVideo && post.Media != nil && post.Media.RedditVideo != nil {
		if u := post.Media.RedditVideo.FallbackURL; u != "" {
			return []string{stripQuery(u)}
		}
	}

	// 3. Preview (video preview, animated gif/mp4, or static image)
	if post.Preview != nil {
		if rvp := post.Preview.RedditVideoPreview; rvp != nil && rvp.FallbackURL != "" {
			return []string{stripQuery(rvp.FallbackURL)}
		}
		var images []string
		for _, img := range post.Preview.Images {
			switch {
			case img.Variants.Mp4 != nil:
				images = append(images, html.UnescapeString(img.Variants.Mp4.Source.URL))
			case img.Variants.Gif != nil:
				images = append(images, html.UnescapeString(img.Variants.Gif.Source.URL))
			case img.Source.URL != "":
				images = append(images, html.UnescapeString(img.Source.URL))
			}
		}
		if len(images) > 0 {
			return images
		}
	}

	// 4. Direct URL fallback
	if post.URL != "" {
		return []string{html.UnescapeString(post.URL)}
	}
	return nil
}

// stripQuery removes the query string from a URL string.
func stripQuery(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		u.RawQuery = ""
		return u.String()
	}
	return raw
}
