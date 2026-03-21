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
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	// userAgent follows Reddit's recommended format: <platform>:<app>:<version>.
	userAgent      = "golang:reddit-gallery-dl:v1.0.0 (by /u/reddit-gallery-dl)"
	defaultTimeout = 120 * time.Second
	maxRetries     = 3
	baseBackoff    = 2 * time.Second
	cacheTTL       = 5 * time.Minute
	cooldownPeriod = 30 * time.Second // back off proactively after a 429

	// Reddit OAuth2 endpoints.
	tokenURL    = "https://www.reddit.com/api/v1/access_token"
	oauthAPIURL = "https://oauth.reddit.com"
	anonAPIURL  = "https://www.reddit.com"
)

// cacheEntry holds a cached gallery result with an expiry time.
type cacheEntry struct {
	gallery *Gallery
	expiry  time.Time
}

// rateLimiter tracks Reddit's x-ratelimit-* headers and calculates the optimal
// delay before each request to avoid hitting the limit. Algorithm ported from
// PRAW (prawcore/rate_limit.py) — the battle-tested Python Reddit library.
type rateLimiter struct {
	mu          sync.Mutex
	remaining   float64
	used        float64
	nextRequest time.Time // earliest time the next request should be made
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

func (rl *rateLimiter) update(headers http.Header) {
	remaining := headers.Get("X-Ratelimit-Remaining")
	used := headers.Get("X-Ratelimit-Used")
	reset := headers.Get("X-Ratelimit-Reset")
	if remaining == "" {
		// No headers — conservatively decrement.
		rl.mu.Lock()
		if rl.remaining > 0 {
			rl.remaining--
			rl.used++
		}
		rl.mu.Unlock()
		return
	}

	r, _ := strconv.ParseFloat(remaining, 64)
	u, _ := strconv.ParseFloat(used, 64)
	s, _ := strconv.ParseFloat(reset, 64)

	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.remaining = r
	rl.used = u

	if r <= 0 {
		// Exhausted — wait until the window resets.
		wait := time.Duration(max(s, 1)) * time.Second
		rl.nextRequest = time.Now().Add(wait)
		slog.Warn("Reddit rate limit exhausted, waiting for reset", "wait", wait)
		return
	}

	// Spread remaining requests evenly across the reset window.
	// Formula from PRAW: sleep = min(reset, max(reset - window*(1 - used/(remaining+used)), 0), 10)
	windowSize := s
	idealSleep := windowSize - windowSize*(1-u/(r+u))
	sleep := min(max(idealSleep, 0), min(s, 10))
	rl.nextRequest = time.Now().Add(time.Duration(sleep * float64(time.Second)))
}

var (
	ErrInvalidURL   = errors.New("invalid reddit url")
	ErrPostNotFound = errors.New("post not found or deleted")
	ErrNoImages     = errors.New("no images found in post")
	ErrRateLimited  = errors.New("reddit is rate limiting requests")
)

// token holds a Reddit OAuth2 bearer token and its expiry.
type token struct {
	value   string
	expiry  time.Time
}

func (t *token) valid() bool {
	return t.value != "" && time.Now().Before(t.expiry.Add(-30*time.Second))
}

type RedditClient struct {
	client           *http.Client
	noRedirectClient *http.Client

	// OAuth2 credentials — optional, read from env at startup.
	clientID     string
	clientSecret string
	tokenMu      sync.Mutex
	tok          token

	rl rateLimiter // PRAW-style rate limiter shared across all API requests

	// cache deduplicates Reddit API calls across requests.
	cacheMu  sync.Mutex
	cache    map[string]cacheEntry
	group    singleflight.Group   // coalesces concurrent fetches for the same URL
	cooldown time.Time            // proactive back-off: don't hit Reddit until after this
}

// startCacheEviction runs a background goroutine that removes expired cache
// entries every cacheTTL to prevent unbounded memory growth.
func (r *RedditClient) startCacheEviction() {
	go func() {
		t := time.NewTicker(cacheTTL)
		defer t.Stop()
		for range t.C {
			now := time.Now()
			r.cacheMu.Lock()
			for k, e := range r.cache {
				if now.After(e.expiry) {
					delete(r.cache, k)
				}
			}
			r.cacheMu.Unlock()
		}
	}()
}

func NewRedditClient() *RedditClient {
	// Single shared transport — both clients talk to the same Reddit hosts
	// so sharing the connection pool avoids redundant TLS handshakes.
	// HTTP/2 is disabled: Reddit's CDN rate-limits non-browser HTTP/2 clients.
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 5,
		IdleConnTimeout:     60 * time.Second,
	}
	clientID     := os.Getenv("REDDIT_CLIENT_ID")
	clientSecret := os.Getenv("REDDIT_CLIENT_SECRET")
	if clientID != "" && clientSecret != "" {
		slog.Info("Reddit OAuth2 credentials found — using authenticated API (100 req/min)")
	} else {
		slog.Warn("No Reddit OAuth2 credentials — using unauthenticated API (rate limits apply). Set REDDIT_CLIENT_ID and REDDIT_CLIENT_SECRET.")
	}

	rc := &RedditClient{
		client: &http.Client{
			Timeout:   defaultTimeout,
			Transport: transport,
		},
		// noRedirectClient is used for share links (/r/<sub>/s/<id>).
		// We only need the Location header — not the destination page.
		noRedirectClient: &http.Client{
			Timeout:   defaultTimeout,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		clientID:     clientID,
		clientSecret: clientSecret,
		cache:        make(map[string]cacheEntry),
	}
	rc.startCacheEviction()
	return rc
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
		E string `json:"e"` // "Image", "AnimatedImage", "RedditVideo"
		S struct {
			U   string `json:"u"`   // static image URL
			Gif string `json:"gif"` // animated GIF URL
			Mp4 string `json:"mp4"` // video URL
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

// doWithRetry executes req using client with PRAW-style rate limiting.
// It sleeps before each request to stay within Reddit's rate limit window,
// and retries up to maxRetries times on HTTP 429 responses.
// The caller is responsible for closing the response body on success.
func (r *RedditClient) doWithRetry(ctx context.Context, req *http.Request) (*http.Response, error) {
	backoff := baseBackoff
	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Sleep if needed to respect the rate limit window.
		if err := r.rl.delay(ctx); err != nil {
			return nil, err
		}

		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}

		// Always update the rate limiter from response headers.
		r.rl.update(resp.Header)

		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		resp.Body.Close()

		if attempt == maxRetries {
			break
		}

		// Respect Retry-After if Reddit tells us how long to wait.
		wait := backoff
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
				wait = time.Duration(secs) * time.Second
			}
		}
		backoff *= 2

		// Add ±25% jitter to prevent thundering herd under concurrent load.
		jitter := time.Duration(rand.Int64N(int64(wait / 2))) - wait/4
		wait += jitter

		slog.Warn("Reddit rate limited, retrying", "attempt", attempt+1, "wait", wait.Round(time.Millisecond))

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	slog.Warn("Reddit rate limited, all retries exhausted")
	return nil, ErrRateLimited
}

// bearerToken returns a valid OAuth2 bearer token, fetching a new one if needed.
// It is safe for concurrent use — only one goroutine fetches at a time.
func (r *RedditClient) bearerToken(ctx context.Context) (string, error) {
	r.tokenMu.Lock()
	defer r.tokenMu.Unlock()

	if r.tok.valid() {
		return r.tok.value, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(r.clientID, r.clientSecret)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token fetch status: %d", resp.StatusCode)
	}

	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("token decode: %w", err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("token error: %s", result.Error)
	}

	r.tok = token{
		value:  result.AccessToken,
		expiry: time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}
	slog.Info("Reddit OAuth2 token refreshed", "expires_in", result.ExpiresIn)
	return r.tok.value, nil
}

// newRequest builds a request with standard Reddit headers.
// If OAuth2 credentials are configured, adds a Bearer token and targets
// oauth.reddit.com (100 req/min). Otherwise falls back to www.reddit.com.
func (r *RedditClient) newRequest(ctx context.Context, method, targetURL string) (*http.Request, error) {
	if r.clientID != "" {
		// Rewrite host to oauth.reddit.com for authenticated requests.
		if u, err := url.Parse(targetURL); err == nil &&
			(u.Host == "www.reddit.com" || u.Host == "reddit.com") {
			u.Host = "oauth.reddit.com"
			u.Scheme = "https"
			targetURL = u.String()
		}
		tok, err := r.bearerToken(ctx)
		if err != nil {
			slog.Warn("OAuth2 token unavailable, falling back to unauthenticated", "error", err)
		} else {
			req, err := http.NewRequestWithContext(ctx, method, targetURL, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("User-Agent", userAgent)
			req.Header.Set("Authorization", "Bearer "+tok)
			return req, nil
		}
	}

	// Unauthenticated fallback.
	req, err := http.NewRequestWithContext(ctx, method, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.AddCookie(&http.Cookie{Name: "over18", Value: "1"})
	return req, nil
}

// FetchGallery resolves a Reddit post URL and returns its gallery images.
// Results are cached for cacheTTL. Concurrent requests for the same URL are
// coalesced via singleflight. A proactive cooldown is applied after a 429.
func (r *RedditClient) FetchGallery(ctx context.Context, postURL string) (*Gallery, error) {
	resolvedURL, err := r.resolveURL(ctx, postURL)
	if err != nil {
		return nil, err
	}

	// Cache lookup.
	r.cacheMu.Lock()
	if e, ok := r.cache[resolvedURL]; ok && time.Now().Before(e.expiry) {
		r.cacheMu.Unlock()
		slog.Debug("Gallery cache hit", "url", resolvedURL)
		return e.gallery, nil
	}
	r.cacheMu.Unlock()

	// Proactive cooldown — if we recently got rate-limited, fail fast.
	r.cacheMu.Lock()
	cooldown := r.cooldown
	r.cacheMu.Unlock()
	if wait := time.Until(cooldown); wait > 0 {
		slog.Warn("Proactive rate limit cooldown", "wait", wait.Round(time.Second))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}

	// Singleflight — deduplicate concurrent fetches for the same URL.
	// Use a detached context with a timeout so one cancelling caller does
	// not abort the in-flight request that other callers are sharing.
	fetchCtx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	v, err, _ := r.group.Do(resolvedURL, func() (any, error) {
		return r.fetchGallery(fetchCtx, resolvedURL)
	})
	if err != nil {
		if errors.Is(err, ErrRateLimited) {
			r.cacheMu.Lock()
			r.cooldown = time.Now().Add(cooldownPeriod)
			r.cacheMu.Unlock()
		}
		return nil, err
	}

	gallery := v.(*Gallery)

	// Store in cache.
	r.cacheMu.Lock()
	r.cache[resolvedURL] = cacheEntry{gallery: gallery, expiry: time.Now().Add(cacheTTL)}
	r.cacheMu.Unlock()

	return gallery, nil
}

// fetchGallery performs the actual Reddit API call. Called only via singleflight.
func (r *RedditClient) fetchGallery(ctx context.Context, resolvedURL string) (*Gallery, error) {
	apiURL := fmt.Sprintf("%s.json", strings.TrimRight(resolvedURL, "/"))
	req, err := r.newRequest(ctx, http.MethodGet, apiURL)
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

// resolveURL validates and normalises a Reddit post URL.
// Share links (/s/...) are resolved by inspecting the redirect Location header
// without following it, to avoid rate limiting on restricted subreddits.
func (r *RedditClient) resolveURL(ctx context.Context, inputURL string) (string, error) {
	inputURL = strings.TrimSpace(inputURL)
	if !strings.HasPrefix(inputURL, "http") {
		inputURL = "https://" + inputURL
	}

	u, err := url.Parse(inputURL)
	if err != nil || u.Host == "" || !strings.Contains(u.Host, "reddit.com") {
		return "", ErrInvalidURL
	}

	// Share links use the path format /r/<sub>/s/<id> and redirect to the real post.
	if isShareLink(u.Path) {
		u, err = r.resolveShareLink(ctx, inputURL)
		if err != nil {
			return "", err
		}
	}

	// Normalise to https://www.reddit.com/<path>, stripping query params and fragments.
	return fmt.Sprintf("https://www.reddit.com%s", u.Path), nil
}

// isShareLink reports whether path is a Reddit share link (/r/<sub>/s/<id>).
func isShareLink(path string) bool {
	// e.g. /r/GreekCelebs/s/x1K7r5KnaM → parts: [r, GreekCelebs, s, x1K7r5KnaM]
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 4 && parts[0] == "r" && parts[2] == "s" && parts[3] != ""
}

// resolveShareLink fetches the share link without following the redirect and
// returns the parsed URL from the Location header.
func (r *RedditClient) resolveShareLink(ctx context.Context, shareURL string) (*url.URL, error) {
	req, err := r.newRequest(ctx, http.MethodGet, shareURL)
	if err != nil {
		return nil, err
	}

	// Share link resolution is a single redirect — no retry needed here.
	// Reddit does not rate-limit the share link endpoint itself.
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

// StreamImage fetches an image URL and returns its body and detected extension.
func (r *RedditClient) StreamImage(ctx context.Context, urlStr string) (io.ReadCloser, string, error) {
	req, err := r.newRequest(ctx, http.MethodGet, urlStr)
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

	// Multi-image gallery — items are ordered by GalleryData.Items.
	if post.IsGallery && post.GalleryData != nil {
		for _, item := range post.GalleryData.Items {
			media, ok := post.MediaMetadata[item.MediaID]
			if !ok {
				continue
			}
			// Prefer mp4 > gif > static image, in that order.
			raw := media.S.Mp4
			if raw == "" {
				raw = media.S.Gif
			}
			if raw == "" {
				raw = media.S.U
			}
			if raw != "" {
				images = append(images, html.UnescapeString(raw))
			}
		}
		if len(images) > 0 {
			return images
		}
	}

	// Single Reddit-hosted video (v.redd.it).
	if post.IsVideo && post.Media != nil && post.Media.RedditVideo != nil {
		if u := post.Media.RedditVideo.FallbackURL; u != "" {
			// Strip query params — they add tracking and break extension detection.
			if parsed, err := url.Parse(u); err == nil {
				parsed.RawQuery = ""
				return []string{parsed.String()}
			}
			return []string{u}
		}
	}

	// Linked GIF/video via preview variants.
	if post.Preview != nil {
		// Reddit video preview (crossposted or linked video).
		if post.Preview.RedditVideoPreview != nil {
			if u := post.Preview.RedditVideoPreview.FallbackURL; u != "" {
				if parsed, err := url.Parse(u); err == nil {
					parsed.RawQuery = ""
					return []string{parsed.String()}
				}
				return []string{u}
			}
		}
		for _, img := range post.Preview.Images {
			// Prefer mp4 preview > gif preview > static source.
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

	// Plain image/gif/video link (e.g. i.redd.it, imgur, etc.).
	if post.URL != "" {
		images = append(images, html.UnescapeString(post.URL))
	}

	return images
}
