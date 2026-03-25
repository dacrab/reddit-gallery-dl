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
	maxRetries     = 3
	baseBackoff    = 2 * time.Second
)

var (
	ErrInvalidURL   = errors.New("invalid reddit url")
	ErrPostNotFound = errors.New("post not found or deleted")
	ErrNoImages     = errors.New("no images found in post")
	ErrRateLimited  = errors.New("reddit is rate limiting requests")
)

// rateLimiter tracks Reddit's x-ratelimit-* headers and calculates the optimal
// delay before each request to avoid hitting the limit.
type rateLimiter struct {
	mu          sync.Mutex
	nextRequest time.Time
}

func (rl *rateLimiter) delay(ctx context.Context) error {
	rl.mu.Lock()
	wait := time.Until(rl.nextRequest)
	rl.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	slog.Debug("Rate limiter sleeping", "wait", wait.Round(time.Millisecond))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

func (rl *rateLimiter) update(h http.Header) {
	remaining := h.Get("X-Ratelimit-Remaining")
	used := h.Get("X-Ratelimit-Used")
	reset := h.Get("X-Ratelimit-Reset")

	if remaining == "" {
		return
	}

	r, _ := strconv.ParseFloat(remaining, 64)
	u, _ := strconv.ParseFloat(used, 64)
	s, _ := strconv.ParseFloat(reset, 64)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	if r <= 0 {
		wait := time.Duration(max(s, 1)) * time.Second
		rl.nextRequest = time.Now().Add(wait)
		slog.Warn("Reddit rate limit exhausted, waiting for reset", "wait", wait)
		return
	}

	// Spread remaining requests evenly across the reset window.
	idealSleep := s - s*(1-u/(r+u))
	sleep := min(max(idealSleep, 0), min(s, 10))
	rl.nextRequest = time.Now().Add(time.Duration(sleep * float64(time.Second)))
}

type RedditClient struct {
	client           *http.Client
	noRedirectClient *http.Client
	rl               rateLimiter
}

func NewRedditClient() *RedditClient {
	// Shared transport; HTTP/2 disabled — Reddit CDN rate-limits non-browser HTTP/2 clients.
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS13},
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     60 * time.Second,
	}
	return &RedditClient{
		client: &http.Client{
			Timeout:   defaultTimeout,
			Transport: transport,
		},
		noRedirectClient: &http.Client{
			Timeout:   defaultTimeout,
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

type redditResponse []struct {
	Data struct {
		Children []struct {
			Data redditPost `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type redditPost struct {
	Title       string `json:"title"`
	IsGallery   bool   `json:"is_gallery"`
	IsVideo     bool   `json:"is_video"`
	URL         string `json:"url_overridden_by_dest"`
	GalleryData *struct {
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

// firstNonEmpty returns the first non-empty string from the arguments.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// stripQuery removes the query string from a URL, returning the clean URL string.
func stripQuery(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		u.RawQuery = ""
		return u.String()
	}
	return raw
}

// doWithRetry executes the request with PRAW-style rate limiting and retries
// up to maxRetries times on HTTP 429 with exponential backoff.
func (r *RedditClient) doWithRetry(ctx context.Context, req *http.Request) (*http.Response, error) {
	backoff := baseBackoff
	for attempt := range maxRetries + 1 {
		if err := r.rl.delay(ctx); err != nil {
			return nil, err
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}
		r.rl.update(resp.Header)
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		resp.Body.Close()
		if attempt == maxRetries {
			break
		}
		wait := backoff
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
				wait = time.Duration(secs) * time.Second
			}
		}
		backoff *= 2
		slog.Warn("Reddit rate limited, retrying", "attempt", attempt+1, "wait", wait)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	slog.Warn("Reddit rate limited, all retries exhausted")
	return nil, ErrRateLimited
}

func (r *RedditClient) FetchGallery(ctx context.Context, postURL string) (*Gallery, error) {
	resolvedURL, err := r.resolveURL(ctx, postURL)
	if err != nil {
		return nil, err
	}

	apiURL := strings.TrimRight(resolvedURL, "/") + ".json"
	req, err := newRequest(ctx, apiURL)
	if err != nil {
		return nil, err
	}

	resp, err := r.doWithRetry(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reddit api status: %d", resp.StatusCode)
	}

	var data redditResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("json decode: %w", err)
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
	return "https://www.reddit.com" + u.Path, nil
}

func isShareLink(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 4 && parts[0] == "r" && parts[2] == "s"
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

func extractImages(post redditPost) []string {
	var images []string

	if post.IsGallery && post.GalleryData != nil {
		for _, item := range post.GalleryData.Items {
			media, ok := post.MediaMetadata[item.MediaID]
			if !ok {
				continue
			}
			if raw := firstNonEmpty(media.S.Mp4, media.S.Gif, media.S.U); raw != "" {
				images = append(images, html.UnescapeString(raw))
			}
		}
		if len(images) > 0 {
			return images
		}
	}

	if post.IsVideo && post.Media != nil && post.Media.RedditVideo != nil {
		if u := post.Media.RedditVideo.FallbackURL; u != "" {
			return []string{stripQuery(u)}
		}
	}

	if post.Preview != nil {
		if post.Preview.RedditVideoPreview != nil {
			if u := post.Preview.RedditVideoPreview.FallbackURL; u != "" {
				return []string{stripQuery(u)}
			}
		}
		for _, img := range post.Preview.Images {
			if img.Variants.Mp4 != nil {
				images = append(images, html.UnescapeString(img.Variants.Mp4.Source.URL))
			} else if img.Variants.Gif != nil {
				images = append(images, html.UnescapeString(img.Variants.Gif.Source.URL))
			} else if img.Source.URL != "" {
				images = append(images, html.UnescapeString(img.Source.URL))
			}
		}
		if len(images) > 0 {
			return images
		}
	}

	if post.URL != "" {
		images = append(images, html.UnescapeString(post.URL))
	}
	return images
}
