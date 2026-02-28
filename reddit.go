package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// userAgent follows Reddit's recommended format: <platform>:<app>:<version>.
	userAgent      = "golang:reddit-gallery-dl:v1.0.0 (by /u/reddit-gallery-dl)"
	defaultTimeout = 120 * time.Second
)

var (
	ErrInvalidURL   = errors.New("invalid reddit url")
	ErrPostNotFound = errors.New("post not found or deleted")
	ErrNoImages     = errors.New("no images found in post")
)

// newTransport returns an HTTP/1.1 transport. Reddit's CDN applies stricter
// rate limiting to HTTP/2 connections from non-browser clients.
func newTransport() *http.Transport {
	return &http.Transport{
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: false,
	}
}

type RedditClient struct {
	client           *http.Client
	noRedirectClient *http.Client
}

func NewRedditClient() *RedditClient {
	return &RedditClient{
		client: &http.Client{
			Timeout:   defaultTimeout,
			Transport: newTransport(),
		},
		// noRedirectClient is used for resolving share links (/s/...).
		// By not following redirects we avoid hitting the destination page,
		// which can return 429 for restricted subreddits without a session.
		noRedirectClient: &http.Client{
			Timeout:   defaultTimeout,
			Transport: newTransport(),
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type Gallery struct {
	Title  string
	Images []string
	URL    string
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
	URL         string `json:"url_overridden_by_dest"`
	GalleryData *struct {
		Items []struct {
			MediaID string `json:"media_id"`
		} `json:"items"`
	} `json:"gallery_data"`
	MediaMetadata map[string]struct {
		S struct{ U, Gif string } `json:"s"`
	} `json:"media_metadata"`
	Preview *struct {
		Images []struct {
			Variants struct {
				Gif *struct {
					Source struct {
						URL string `json:"url"`
					} `json:"source"`
				} `json:"gif"`
			} `json:"variants"`
		} `json:"images"`
	} `json:"preview"`
}

// newRequest builds an authenticated-style request with the standard headers
// Reddit expects from API clients.
func newRequest(ctx context.Context, method, targetURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("request creation failed: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.AddCookie(&http.Cookie{Name: "over18", Value: "1"})
	return req, nil
}

// FetchGallery resolves a Reddit post URL and returns its gallery images.
func (r *RedditClient) FetchGallery(ctx context.Context, postURL string) (*Gallery, error) {
	resolvedURL, err := r.resolveURL(ctx, postURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("%s.json", strings.TrimRight(resolvedURL, "/"))
	req, err := newRequest(ctx, http.MethodGet, apiURL)
	if err != nil {
		return nil, err
	}

	resp, err := r.client.Do(req)
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

	return &Gallery{Title: post.Title, Images: images, URL: postURL}, nil
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
	req, err := newRequest(ctx, http.MethodGet, shareURL)
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

// StreamImage fetches an image URL and returns its body and detected extension.
func (r *RedditClient) StreamImage(ctx context.Context, urlStr string) (io.ReadCloser, string, error) {
	req, err := newRequest(ctx, http.MethodGet, urlStr)
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
			if media, ok := post.MediaMetadata[item.MediaID]; ok {
				// Prefer animated GIF source, fall back to static image.
				raw := media.S.Gif
				if raw == "" {
					raw = media.S.U
				}
				if raw != "" {
					images = append(images, html.UnescapeString(raw))
				}
			}
		}
	}

	if len(images) == 0 && post.Preview != nil {
		for _, img := range post.Preview.Images {
			if img.Variants.Gif != nil {
				images = append(images, html.UnescapeString(img.Variants.Gif.Source.URL))
			}
		}
	}

	if len(images) == 0 && post.URL != "" {
		images = append(images, html.UnescapeString(post.URL))
	}

	return images
}
