package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsRedditHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"reddit.com", true},
		{"www.reddit.com", true},
		{"old.reddit.com", true},
		{"notreddit.com", false},
		{"evil-reddit.com", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isRedditHost(tt.host); got != tt.want {
			t.Errorf("isRedditHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestIsPostPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/r/pics/comments/abc123/my_post/", true},
		{"/r/pics/comments/abc123", true},
		{"/r/pics/s/abc123def456", false},
		{"/r/pics/", false},
		{"/", false},
	}
	for _, tt := range tests {
		if got := isPostPath(tt.path); got != tt.want {
			t.Errorf("isPostPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestIsShareLink(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/r/pics/s/abc123def456", true},
		{"/r/pics/comments/abc123/title", false},
		{"/r/pics/", false},
	}
	for _, tt := range tests {
		if got := isShareLink(tt.path); got != tt.want {
			t.Errorf("isShareLink(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestResolveURL(t *testing.T) {
	tests := []struct {
		input   string
		wantErr error
	}{
		{"https://www.reddit.com/r/pics/comments/abc/title/", nil},
		{"reddit.com/r/pics/comments/abc/title/", nil},
		{"https://www.reddit.com/r/pics/", ErrInvalidURL},
		{"https://example.com/something", ErrInvalidURL},
		{"not a url at all !!!", ErrInvalidURL},
	}
	for _, tt := range tests {
		_, err := resolveURL(context.Background(), tt.input)
		if tt.wantErr != nil && err != tt.wantErr {
			t.Errorf("resolveURL(%q) err = %v, want %v", tt.input, err, tt.wantErr)
		}
		if tt.wantErr == nil && err != nil {
			t.Errorf("resolveURL(%q) unexpected err: %v", tt.input, err)
		}
	}
}

func TestResolveShareLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://www.reddit.com/r/pics/comments/abc123/title/")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	u, err := resolveShareLink(context.Background(), srv.URL+"/r/pics/s/xyz")
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/r/pics/comments/abc123/title/" {
		t.Errorf("got path %q", u.Path)
	}
}

func TestDetectExtension(t *testing.T) {
	tests := []struct {
		url, ct, want string
	}{
		{"https://i.redd.it/abc.png", "", ".png"},
		{"https://i.redd.it/abc.jpg?width=640", "", ".jpg"},
		{"https://v.redd.it/abc/DASH_720.mp4", "", ".mp4"},
		{"https://i.redd.it/abc.gif", "", ".gif"},
		{"https://i.redd.it/abc", "image/png", ".png"},
		{"https://i.redd.it/abc", "image/jpeg; charset=utf-8", ".jpg"},
		{"https://i.redd.it/abc", "video/mp4", ".mp4"},
		{"https://i.redd.it/abc", "", ".jpg"}, // fallback
	}
	for _, tt := range tests {
		if got := detectExtension(tt.url, tt.ct); got != tt.want {
			t.Errorf("detectExtension(%q, %q) = %q, want %q", tt.url, tt.ct, got, tt.want)
		}
	}
}

func TestUrlExt(t *testing.T) {
	tests := []struct {
		url, want string
	}{
		{"https://i.redd.it/photo.png?w=640", ".png"},
		{"https://v.redd.it/video.mp4", ".mp4"},
		{"https://i.redd.it/noext", ""},
	}
	for _, tt := range tests {
		if got := urlExt(tt.url); got != tt.want {
			t.Errorf("urlExt(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestExtractImages_Gallery(t *testing.T) {
	post := redditPost{
		IsGallery: true,
		GalleryData: &struct {
			Items []struct {
				MediaID string `json:"media_id"`
			} `json:"items"`
		}{
			Items: []struct {
				MediaID string `json:"media_id"`
			}{
				{MediaID: "img1"},
				{MediaID: "img2"},
			},
		},
		MediaMetadata: map[string]struct {
			E string `json:"e"`
			S struct {
				U   string `json:"u"`
				Gif string `json:"gif"`
				Mp4 string `json:"mp4"`
			} `json:"s"`
		}{
			"img1": {S: struct {
				U   string `json:"u"`
				Gif string `json:"gif"`
				Mp4 string `json:"mp4"`
			}{U: "https://i.redd.it/img1.jpg"}},
			"img2": {S: struct {
				U   string `json:"u"`
				Gif string `json:"gif"`
				Mp4 string `json:"mp4"`
			}{Gif: "https://i.redd.it/img2.gif"}},
		},
	}
	imgs := extractImages(post)
	if len(imgs) != 2 {
		t.Fatalf("got %d images, want 2", len(imgs))
	}
	if imgs[0] != "https://i.redd.it/img1.jpg" {
		t.Errorf("imgs[0] = %q", imgs[0])
	}
	if imgs[1] != "https://i.redd.it/img2.gif" {
		t.Errorf("imgs[1] = %q", imgs[1])
	}
}

func TestExtractImages_Video(t *testing.T) {
	post := redditPost{
		IsVideo: true,
		Media: &struct {
			RedditVideo *struct {
				FallbackURL string `json:"fallback_url"`
			} `json:"reddit_video"`
		}{
			RedditVideo: &struct {
				FallbackURL string `json:"fallback_url"`
			}{FallbackURL: "https://v.redd.it/abc/DASH_720.mp4?source=fallback"},
		},
	}
	imgs := extractImages(post)
	if len(imgs) != 1 {
		t.Fatalf("got %d images, want 1", len(imgs))
	}
	if imgs[0] != "https://v.redd.it/abc/DASH_720.mp4" {
		t.Errorf("got %q", imgs[0])
	}
}

func TestExtractImages_SingleURL(t *testing.T) {
	post := redditPost{URL: "https://i.redd.it/single.jpg"}
	imgs := extractImages(post)
	if len(imgs) != 1 || imgs[0] != "https://i.redd.it/single.jpg" {
		t.Errorf("got %v", imgs)
	}
}

func TestExtractImages_Empty(t *testing.T) {
	post := redditPost{}
	if imgs := extractImages(post); imgs != nil {
		t.Errorf("expected nil, got %v", imgs)
	}
}

func TestFetchGallery(t *testing.T) {
	resp := redditResponse{
		{Data: struct {
			Children []struct {
				Data redditPost `json:"data"`
			} `json:"children"`
		}{
			Children: []struct {
				Data redditPost `json:"data"`
			}{
				{Data: redditPost{
					Title: "Test Post",
					URL:   "https://i.redd.it/test.jpg",
				}},
			},
		}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// Override httpClient for test
	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	// We can't easily test fetchGallery without also mocking resolveURL,
	// so test doReddit directly against our mock server.
	ctx := context.Background()
	r, err := doReddit(ctx, srv.URL+"/r/test/comments/abc/title.json")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Errorf("status = %d", r.StatusCode)
	}
}

func TestDoReddit_RateLimitRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	resp, err := doReddit(context.Background(), srv.URL+"/test")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
}

func TestStripQuery(t *testing.T) {
	got := stripQuery("https://v.redd.it/abc/DASH_720.mp4?source=fallback&extra=1")
	want := "https://v.redd.it/abc/DASH_720.mp4"
	if got != want {
		t.Errorf("stripQuery = %q, want %q", got, want)
	}
}
