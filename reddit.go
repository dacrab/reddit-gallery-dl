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

type redditResponse []struct {
	Data struct {
		Children []struct {
			Data redditPost `json:"data"`
		} `json:"children"`
	} `json:"data"`
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

	GalleryData *galleryData `json:"gallery_data"`

	MediaMetadata map[string]mediaMetadata `json:"media_metadata"`

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
			Source struct {
				URL string `json:"url"`
			} `json:"source"`
			Variants struct {
				GIF *struct {
					Source struct {
						URL string `json:"url"`
					} `json:"source"`
				} `json:"gif"`
				MP4 *struct {
					Source struct {
						URL string `json:"url"`
					} `json:"source"`
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
	_ = resp.Body.Close()

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
		_ = resp.Body.Close()
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
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var data redditResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(&data); err != nil || len(data) == 0 || len(data[0].Data.Children) == 0 {
			return nil, ErrPostNotFound
		}
		post := data[0].Data.Children[0].Data
		images := extractImages(post)
		if len(images) == 0 {
			return nil, ErrNoImages
		}
		return &Gallery{Title: post.Title, Images: images}, nil
	case http.StatusForbidden:
		log.Printf("JSON API returned 403, falling back to HTML scrape: %s", resolved)
		return fetchGalleryFromHTML(ctx, resolved)
	case http.StatusNotFound:
		return nil, ErrPostNotFound
	default:
		return nil, fmt.Errorf("reddit api status: %d", resp.StatusCode)
	}
}

var (
	rxTitle      = regexp.MustCompile(`<title>([^<]+)`)
	rxMediaIDs   = regexp.MustCompile(`data-media-ids="([^"]+)"`)
	rxPreviewExt = regexp.MustCompile(`preview\.redd\.it/([^\.?]+)\.([a-z0-9]+)`)
	rxDataURL    = regexp.MustCompile(`data-url="([^"]+)"`)
)

func fetchGalleryFromHTML(ctx context.Context, resolved string) (*Gallery, error) {
	u, err := url.Parse(resolved)
	if err != nil {
		return nil, err
	}
	htmlURL := "https://old.reddit.com" + strings.TrimRight(u.Path, "/") + "/"
	req, err := redditRequest(ctx, htmlURL, false)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return nil, ErrPostNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("old.reddit.com status: %d", resp.StatusCode)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes))
	if err != nil {
		return nil, err
	}
	page := string(b)

	title := ""
	if m := rxTitle.FindStringSubmatch(page); len(m) > 1 {
		title = m[1]
		if idx := strings.LastIndex(title, " : "); idx > 0 {
			title = title[:idx]
		}
	}

	var urls []string
	if strings.Contains(page, `data-is-gallery="true"`) {
		idsStr := ""
		if m := rxMediaIDs.FindStringSubmatch(page); len(m) > 1 {
			idsStr = m[1]
		}
		if idsStr != "" {
			ext := map[string]string{}
			for _, m := range rxPreviewExt.FindAllStringSubmatch(page, -1) {
				ext[m[1]] = "." + m[2]
			}
			for _, id := range strings.Split(idsStr, ",") {
				id = strings.TrimSpace(id)
				if e, ok := ext[id]; ok {
					urls = append(urls, "https://i.redd.it/"+id+e)
				}
			}
		}
	} else if m := rxDataURL.FindStringSubmatch(page); len(m) > 1 && m[1] != "" {
		urls = []string{html.UnescapeString(m[1])}
	}

	if len(urls) == 0 {
		return nil, ErrNoImages
	}
	return &Gallery{Title: title, Images: urls}, nil
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
