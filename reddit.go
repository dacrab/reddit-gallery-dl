package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type RedditClient struct {
	client *http.Client
}

func NewRedditClient() *RedditClient {
	return &RedditClient{
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// Gallery is the Clean Domain Object
type Gallery struct {
	Title  string
	Images []string
	URL    string // Original Input URL
}

// Centralized Request Handler (The Fix for Duplication)
func (r *RedditClient) makeRequest(method, url string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	// Always send the cookie, it doesn't hurt non-NSFW requests
	req.AddCookie(&http.Cookie{Name: "over18", Value: "1"})

	return r.client.Do(req)
}

func (r *RedditClient) FetchGallery(postURL string) (*Gallery, error) {
	// 1. Resolve shortened URLs
	resolvedURL, err := r.resolveURL(postURL)
	if err != nil {
		return nil, err
	}

	// 2. Fetch JSON
	apiURL := fmt.Sprintf("%s.json", strings.TrimRight(resolvedURL, "/"))
	resp, err := r.makeRequest("GET", apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("reddit API status: %d", resp.StatusCode)
	}

	// 3. Parse (Using anonymous structs to avoid global clutter)
	var data []struct {
		Data struct {
			Children []struct {
				Data struct {
					Title         string `json:"title"`
					IsGallery     bool   `json:"is_gallery"`
					URL           string `json:"url_overridden_by_dest"`
					GalleryData   *struct {
						Items []struct{ MediaID string `json:"media_id"` } `json:"items"`
					} `json:"gallery_data"`
					MediaMetadata map[string]struct {
						S struct{ U, Gif string } `json:"s"`
					} `json:"media_metadata"`
				} `json:"data"`
			} `json:"children"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode JSON")
	}

	if len(data) == 0 || len(data[0].Data.Children) == 0 {
		return nil, fmt.Errorf("no post data found")
	}

	post := data[0].Data.Children[0].Data

	// Extract Images
	var images []string
	if post.IsGallery && post.GalleryData != nil {
		for _, item := range post.GalleryData.Items {
			if media, ok := post.MediaMetadata[item.MediaID]; ok {
				raw := media.S.U
				if raw == "" { raw = media.S.Gif }
				if raw != "" {
					images = append(images, strings.ReplaceAll(raw, "&amp;", "&"))
				}
			}
		}
	} else if post.URL != "" {
		images = append(images, post.URL)
	}

	return &Gallery{
		Title:  post.Title,
		Images: images,
		URL:    postURL,
	}, nil
}

func (r *RedditClient) resolveURL(inputURL string) (string, error) {
	// 1. Sanitize Input: Ensure scheme exists
	inputURL = strings.TrimSpace(inputURL)
	if !strings.HasPrefix(inputURL, "http://") && !strings.HasPrefix(inputURL, "https://") {
		inputURL = "https://" + inputURL
	}

	// 2. Basic Validation
	u, err := url.Parse(inputURL)
	if err != nil || u.Host == "" || !strings.Contains(u.Host, ".") {
		return "", fmt.Errorf("invalid URL format")
	}

	resp, err := r.makeRequest("HEAD", inputURL) // Use HEAD to just check headers/redirects
	if err != nil {
		// Fallback to GET if HEAD fails (some servers block HEAD)
		resp, err = r.makeRequest("GET", inputURL)
		if err != nil {
			return "", err
		}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	final := resp.Request.URL
	if !strings.Contains(final.Host, "reddit.com") {
		return "", fmt.Errorf("invalid reddit url")
	}
	return fmt.Sprintf("%s://%s%s", final.Scheme, final.Host, final.Path), nil
}

func (r *RedditClient) StreamImage(url string) (io.ReadCloser, string, error) {
	resp, err := r.makeRequest("GET", url)
	if err != nil {
		return nil, "", err
	}

	ext := ".jpg"
	if strings.Contains(url, ".png") { ext = ".png" }
	if strings.Contains(url, ".gif") { ext = ".gif" }

	return resp.Body, ext, nil
}
