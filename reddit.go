package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	userAgent      = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	defaultTimeout = 120 * time.Second
)

var (
	ErrInvalidURL   = errors.New("invalid reddit url")
	ErrPostNotFound = errors.New("post not found or deleted")
	ErrNoImages     = errors.New("no images found in post")
)

type RedditClient struct {
	client *http.Client
}

func NewRedditClient() *RedditClient {
	return &RedditClient{
		client: &http.Client{Timeout: defaultTimeout},
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

func (r *RedditClient) makeRequest(ctx context.Context, method, targetURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("request creation failed: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.AddCookie(&http.Cookie{Name: "over18", Value: "1"})
	return r.client.Do(req)
}

func (r *RedditClient) FetchGallery(ctx context.Context, postURL string) (*Gallery, error) {
	resolvedURL, err := r.resolveURL(ctx, postURL)
	if err != nil {
		return nil, err
	}

	resp, err := r.makeRequest(ctx, "GET", fmt.Sprintf("%s.json", strings.TrimRight(resolvedURL, "/")))
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

func (r *RedditClient) resolveURL(ctx context.Context, inputURL string) (string, error) {
	inputURL = strings.TrimSpace(inputURL)
	if !strings.HasPrefix(inputURL, "http") {
		inputURL = "https://" + inputURL
	}

	u, err := url.Parse(inputURL)
	if err != nil || u.Host == "" || !strings.Contains(u.Host, ".") {
		return "", ErrInvalidURL
	}

	resp, err := r.makeRequest(ctx, "HEAD", inputURL)
	if err != nil {
		resp, err = r.makeRequest(ctx, "GET", inputURL)
		if err != nil {
			return "", err
		}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	final := resp.Request.URL
	if !strings.Contains(final.Host, "reddit.com") {
		return "", ErrInvalidURL
	}
	return fmt.Sprintf("%s://%s%s", final.Scheme, final.Host, final.Path), nil
}

func (r *RedditClient) StreamImage(ctx context.Context, urlStr string) (io.ReadCloser, string, error) {
	resp, err := r.makeRequest(ctx, "GET", urlStr)
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
				raw := media.S.Gif
				if raw == "" {
					raw = media.S.U
				}
				if raw != "" {
					images = append(images, strings.ReplaceAll(raw, "&amp;", "&"))
				}
			}
		}
	}

	if len(images) == 0 && post.Preview != nil {
		for _, img := range post.Preview.Images {
			if img.Variants.Gif != nil {
				images = append(images, strings.ReplaceAll(img.Variants.Gif.Source.URL, "&amp;", "&"))
			}
		}
	}

	if len(images) == 0 && post.URL != "" {
		images = append(images, strings.ReplaceAll(post.URL, "&amp;", "&"))
	}

	return images
}

func detectExtension(urlStr, contentType string) string {
	if contentType != "" {
		exts, _ := mime.ExtensionsByType(contentType)
		if len(exts) > 0 {
			return exts[0]
		}
	}
	if u, err := url.Parse(urlStr); err == nil {
		ext := strings.ToLower(path.Ext(u.Path))
		if ext == ".png" || ext == ".gif" || ext == ".jpg" || ext == ".jpeg" {
			return ext
		}
	}
	return ".jpg"
}
