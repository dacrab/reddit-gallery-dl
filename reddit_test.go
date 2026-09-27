package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func testResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

const embedGalleryFixture = `<h1>Gallery &amp; title</h1>
<img src="https://preview.redd.it/example-title-v0-p4223x8lmp9c1.png?width=640&amp;auto=webp">
<img srcset="https://preview.redd.it/example-title-v0-p4223x8lmp9c1.png?width=320 320w" src="https://preview.redd.it/example-title-v0-p4223x8lmp9c1.png?width=640">
<img src="https://preview.redd.it/example-title-v0-ghlt58aqmp9c1.jpg?width=640">`

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
		GalleryData: &galleryData{
			Items: []galleryItem{
				{MediaID: "img1"},
				{MediaID: "img2"},
			},
		},
		MediaMetadata: map[string]mediaMetadata{
			"img1": {S: mediaSource{U: "https://i.redd.it/img1.jpg"}},
			"img2": {S: mediaSource{Gif: "https://i.redd.it/img2.gif"}},
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
		Media: &redditMedia{
			RedditVideo: &redditVideo{FallbackURL: "https://v.redd.it/abc/DASH_720.mp4?source=fallback"},
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

func TestDoReddit_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	origClient := httpClient
	httpClient = srv.Client()
	defer func() { httpClient = origClient }()

	resp, err := doReddit(context.Background(), srv.URL+"/r/test/comments/abc/title.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
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
func TestGalleryFromEmbedHTML(t *testing.T) {
	got := galleryFromEmbedHTML(embedGalleryFixture)
	if got == nil {
		t.Fatal("galleryFromEmbedHTML returned nil")
	}
	if got.Title != "Gallery & title" {
		t.Errorf("title = %q", got.Title)
	}
	if len(got.Images) != 2 {
		t.Fatalf("got %d images, want 2: %v", len(got.Images), got.Images)
	}
	if got.Images[0] != "https://i.redd.it/p4223x8lmp9c1.png" {
		t.Errorf("first image = %q", got.Images[0])
	}
	if got.Images[1] != "https://i.redd.it/ghlt58aqmp9c1.jpg" {
		t.Errorf("second image = %q", got.Images[1])
	}
}

func TestFetchGallery_UsesEmbedWhenJSONForbidden(t *testing.T) {
	origClient := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "www.reddit.com", "old.reddit.com", "api.reddit.com":
			return testResponse(r, http.StatusForbidden, ""), nil
		case "embed.reddit.com":
			return testResponse(r, http.StatusOK, embedGalleryFixture), nil
		default:
			t.Fatalf("unexpected fallback request to %s", r.URL.Host)
			return nil, nil
		}
	})}
	defer func() { httpClient = origClient }()

	got, err := fetchGallery(context.Background(), "https://www.reddit.com/r/pics/comments/abc123/title/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Images) != 2 {
		t.Fatalf("got %d images, want 2", len(got.Images))
	}
}

func TestFetchGallery_UsesAlternateArchive(t *testing.T) {
	origClient := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "www.reddit.com", "old.reddit.com", "api.reddit.com":
			return testResponse(r, http.StatusForbidden, ""), nil
		case "embed.reddit.com":
			return testResponse(r, http.StatusOK, "<h1>No media</h1>"), nil
		case "api.pullpush.io":
			return testResponse(r, http.StatusServiceUnavailable, ""), nil
		case "arctic-shift.photon-reddit.com":
			return testResponse(r, http.StatusOK, `{"data":[{"title":"Archived gallery","is_gallery":true,"gallery_data":{"items":[{"media_id":"abc"}]},"media_metadata":{"abc":{"s":{"u":"https://i.redd.it/abc.jpg"}}}}]}`), nil
		default:
			t.Fatalf("unexpected fallback request to %s", r.URL.Host)
			return nil, nil
		}
	})}
	defer func() { httpClient = origClient }()

	got, err := fetchGallery(context.Background(), "https://www.reddit.com/r/pics/comments/abc123/title/")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Archived gallery" || len(got.Images) != 1 || got.Images[0] != "https://i.redd.it/abc.jpg" {
		t.Fatalf("unexpected gallery: %+v", got)
	}
}

func TestStripQuery(t *testing.T) {
	got := stripQuery("https://v.redd.it/abc/DASH_720.mp4?source=fallback&extra=1")
	want := "https://v.redd.it/abc/DASH_720.mp4"
	if got != want {
		t.Errorf("stripQuery = %q, want %q", got, want)
	}
}

func TestFetchGallery_FallsBackWhenRateLimited(t *testing.T) {
	origClient := httpClient
	calls := 0
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "www.reddit.com":
			calls++
			if r.URL.Query().Get("raw_json") != "1" {
				t.Errorf("expected raw_json=1 on public JSON request, got %q", r.URL.RawQuery)
			}
			resp := testResponse(r, http.StatusTooManyRequests, "")
			resp.Header.Set("Retry-After", "1")
			return resp, nil
		case "old.reddit.com", "api.reddit.com":
			return testResponse(r, http.StatusForbidden, ""), nil
		case "embed.reddit.com":
			return testResponse(r, http.StatusOK, embedGalleryFixture), nil
		default:
			t.Fatalf("unexpected fallback request to %s", r.URL.Host)
			return nil, nil
		}
	})}
	defer func() { httpClient = origClient }()

	got, err := fetchGallery(context.Background(), "https://www.reddit.com/r/pics/comments/abc123/title/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Images) != 2 {
		t.Fatalf("got %d images, want 2", len(got.Images))
	}
	if calls != 2 {
		t.Errorf("expected 2 rate-limited JSON calls on www, got %d", calls)
	}
}

func TestFetchGallery_BlockPageHTMLTriggersFallback(t *testing.T) {
	origClient := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "www.reddit.com", "old.reddit.com", "api.reddit.com":
			// Reddit's WAF answers with an HTML block page under a 200 status.
			return testResponse(r, http.StatusOK, "<!DOCTYPE html><html><body>Blocked</body></html>"), nil
		case "embed.reddit.com":
			return testResponse(r, http.StatusOK, embedGalleryFixture), nil
		default:
			t.Fatalf("unexpected fallback request to %s", r.URL.Host)
			return nil, nil
		}
	})}
	defer func() { httpClient = origClient }()

	got, err := fetchGallery(context.Background(), "https://www.reddit.com/r/pics/comments/abc123/title/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Images) != 2 {
		t.Fatalf("got %d images, want 2", len(got.Images))
	}
}

func TestFetchGallery_AllSourcesBlockedReportsErrBlocked(t *testing.T) {
	origClient := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Host, "reddit.com") {
			return testResponse(r, http.StatusForbidden, ""), nil
		}
		return testResponse(r, http.StatusServiceUnavailable, ""), nil
	})}
	defer func() { httpClient = origClient }()

	_, err := fetchGallery(context.Background(), "https://www.reddit.com/r/pics/comments/abc123/title/")
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("err = %v, want ErrBlocked", err)
	}
}

func TestFetchGallery_NotFoundSkipsFallbacks(t *testing.T) {
	origClient := httpClient
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "www.reddit.com" {
			t.Fatalf("unexpected fallback request to %s", r.URL.Host)
		}
		return testResponse(r, http.StatusNotFound, ""), nil
	})}
	defer func() { httpClient = origClient }()

	_, err := fetchGallery(context.Background(), "https://www.reddit.com/r/pics/comments/abc123/title/")
	if !errors.Is(err, ErrPostNotFound) {
		t.Errorf("err = %v, want ErrPostNotFound", err)
	}
}
