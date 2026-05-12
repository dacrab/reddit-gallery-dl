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
	"strconv"
	"strings"
	"time"
)

const userAgent = "golang:reddit-gallery-dl:v1.0.0 (by /u/reddit-gallery-dl)"

var (
	ErrInvalidURL   = errors.New("invalid reddit url")
	ErrPostNotFound = errors.New("post not found or deleted")
	ErrNoImages     = errors.New("no images found in post")
	ErrRateLimited  = errors.New("reddit is rate limiting requests")

	httpClient = &http.Client{Timeout: 30 * time.Second}
)

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

	GalleryData *struct {
		Items []struct {
			MediaID string `json:"media_id"`
		} `json:"items"`
	} `json:"gallery_data"`

	MediaMetadata map[string]struct {
		E string `json:"e"`
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

type Gallery struct {
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

// doReddit executes a request with one retry on 429.
func doReddit(ctx context.Context, rawURL string) (*http.Response, error) {
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
	resp.Body.Close()

	// One retry after waiting Retry-After (or 2s default).
	wait := 2 * time.Second
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 && secs <= 10 {
			wait = time.Duration(secs) * time.Second
		}
	}
	log.Printf("Rate limited, retrying in %v", wait)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(wait):
	}

	req, err = redditRequest(ctx, rawURL, true)
	if err != nil {
		return nil, err
	}
	resp, err = httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		return nil, ErrRateLimited
	}
	return resp, nil
}

func fetchGallery(ctx context.Context, postURL string) (*Gallery, error) {
	resolved, err := resolveURL(ctx, postURL)
	if err != nil {
		return nil, err
	}
	resp, err := doReddit(ctx, strings.TrimRight(resolved, "/")+".json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return nil, ErrPostNotFound
	}
	if resp.StatusCode != http.StatusOK {
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
		resp.Body.Close()
		return nil, "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return resp.Body, detectExtension(rawURL, resp.Header.Get("Content-Type")), nil
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
	noRedirect := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := redditRequest(ctx, shareURL, false)
	if err != nil {
		return nil, err
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
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

// urlExt returns the lowercased extension from a URL path.
func urlExt(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return strings.ToLower(path.Ext(u.Path))
	}
	return strings.ToLower(path.Ext(rawURL))
}

// detectExtension returns a file extension from URL path or Content-Type.
func detectExtension(urlStr, contentType string) string {
	if u, err := url.Parse(urlStr); err == nil {
		switch ext := strings.ToLower(path.Ext(u.Path)); ext {
		case ".png", ".gif", ".gifv", ".jpg", ".jpeg", ".webp", ".mp4", ".webm", ".mov":
			return ext
		}
	}
	ct, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(ct)) {
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
