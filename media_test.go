package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestExtractMedia(t *testing.T) {
	tests := []struct {
		name string
		post redditPost
		want []string
	}{
		{
			name: "prefers the original upload over the resized preview",
			post: redditPost{
				URL:     "https://i.redd.it/original.png",
				Preview: &redditPreview{Images: []previewImage{{Source: previewSource{URL: "https://preview.redd.it/tiny.png?width=108&auto=webp"}}}},
			},
			want: []string{"https://i.redd.it/original.png"},
		},
		{
			name: "upgrades a preview URL when the post has no direct link",
			post: redditPost{
				Preview: &redditPreview{Images: []previewImage{{Source: previewSource{URL: "https://preview.redd.it/4hpo47nlimsh1.jpg?width=108&auto=webp"}}}},
			},
			want: []string{"https://i.redd.it/4hpo47nlimsh1.jpg"},
		},
		{
			name: "keeps the reddit-hosted preview of an external link post",
			post: redditPost{
				URL:     "https://play.google.com/store/apps/details?id=com.example",
				Preview: &redditPreview{Images: []previewImage{{Source: previewSource{URL: "https://external-preview.redd.it/AbCdEfGh.png?auto=webp"}}}},
			},
			want: []string{"https://external-preview.redd.it/AbCdEfGh.png?auto=webp"},
		},
		{
			name: "prefers the mp4 over the still and the original gif",
			post: redditPost{
				IsGallery: true,
				GalleryData: &galleryData{Items: []galleryItem{
					{MediaID: "a"}, {MediaID: "b"},
				}},
				MediaMetadata: map[string]mediaMetadata{
					"a": {S: mediaSource{URL: "https://i.redd.it/a.jpg", Gif: "https://i.redd.it/a.gif", Mp4: "https://i.redd.it/a.mp4"}},
					"b": {S: mediaSource{URL: "https://i.redd.it/b.jpg"}},
				},
			},
			want: []string{"https://i.redd.it/a.mp4", "https://i.redd.it/b.jpg"},
		},
		{
			name: "an animated gallery gif downloads as the smaller mp4",
			post: redditPost{
				IsGallery:   true,
				GalleryData: &galleryData{Items: []galleryItem{{MediaID: "a"}}},
				MediaMetadata: map[string]mediaMetadata{
					"a": {S: mediaSource{
						URL: "https://i.redd.it/a.jpg",
						Gif: "https://preview.redd.it/a.gif?s=abc",
						Mp4: "https://preview.redd.it/a.mp4?s=abc",
					}},
				},
			},
			want: []string{"https://preview.redd.it/a.mp4?s=abc"},
		},
		{
			name: "a still framed as a gif loses to the mp4 animation",
			post: redditPost{
				IsGallery:   true,
				GalleryData: &galleryData{Items: []galleryItem{{MediaID: "a"}}},
				MediaMetadata: map[string]mediaMetadata{
					// Reddit hands back s.gif = format=png8 for an animated post;
					// that is a poster frame, not the animation.
					"a": {S: mediaSource{
						Gif: "https://preview.redd.it/a.gif?format=png8&s=abc",
						Mp4: "https://preview.redd.it/a.gif?format=mp4&s=abc",
					}},
				},
			},
			want: []string{"https://preview.redd.it/a.gif?format=mp4&s=abc"},
		},
		{
			name: "keeps the gif when there is no mp4 to prefer",
			post: redditPost{
				IsGallery:   true,
				GalleryData: &galleryData{Items: []galleryItem{{MediaID: "a"}}},
				MediaMetadata: map[string]mediaMetadata{
					"a": {S: mediaSource{Gif: "https://preview.redd.it/a.gif?format=png8&s=abc"}},
				},
			},
			want: []string{"https://preview.redd.it/a.gif?format=png8&s=abc"},
		},
		{
			name: "skips gallery items with no metadata",
			post: redditPost{
				IsGallery:     true,
				GalleryData:   &galleryData{Items: []galleryItem{{MediaID: "gone"}, {MediaID: "here"}}},
				MediaMetadata: map[string]mediaMetadata{"here": {S: mediaSource{URL: "https://i.redd.it/here.jpg"}}},
			},
			want: []string{"https://i.redd.it/here.jpg"},
		},
		{
			name: "gallery order follows the item list, not map order",
			post: redditPost{
				IsGallery: true,
				GalleryData: &galleryData{Items: []galleryItem{
					{MediaID: "third"}, {MediaID: "first"}, {MediaID: "second"},
				}},
				MediaMetadata: map[string]mediaMetadata{
					"first":  {S: mediaSource{URL: "https://i.redd.it/1.jpg"}},
					"second": {S: mediaSource{URL: "https://i.redd.it/2.jpg"}},
					"third":  {S: mediaSource{URL: "https://i.redd.it/3.jpg"}},
				},
			},
			want: []string{"https://i.redd.it/3.jpg", "https://i.redd.it/1.jpg", "https://i.redd.it/2.jpg"},
		},
		{
			name: "video fallback url keeps its query",
			post: redditPost{
				IsVideo: true,
				Media:   &redditMedia{RedditVideo: &redditVideo{FallbackURL: "https://v.redd.it/abc/DASH_720.mp4?source=fallback"}},
			},
			want: []string{"https://v.redd.it/abc/DASH_720.mp4?source=fallback"},
		},
		{
			name: "gif post served as reddit_video keeps its signed mp4 url",
			post: redditPost{
				IsVideo: true,
				URL:     "https://i.redd.it/wut8n9cqe2yh1.gif",
				Media:   &redditMedia{RedditVideo: &redditVideo{FallbackURL: "https://preview.redd.it/wut8n9cqe2yh1.gif?width=1080&format=mp4&s=abc"}},
			},
			want: []string{"https://preview.redd.it/wut8n9cqe2yh1.gif?width=1080&format=mp4&s=abc"},
		},
		{
			name: "direct gif defers to the preview mp4",
			post: redditPost{
				URL: "https://i.redd.it/0n5exiers7yh1.gif",
				Preview: &redditPreview{Images: []previewImage{{
					Source:   previewSource{URL: "https://preview.redd.it/0n5exiers7yh1.gif?s=gif"},
					Variants: previewVariants{MP4: &previewVariant{Source: previewSource{URL: "https://preview.redd.it/0n5exiers7yh1.gif?format=mp4&s=mp4"}}},
				}}},
			},
			want: []string{"https://preview.redd.it/0n5exiers7yh1.gif?format=mp4&s=mp4"},
		},
		{
			name: "direct gif stays a gif when no mp4 exists",
			post: redditPost{
				URL: "https://i.redd.it/0n5exiers7yh1.gif",
				Preview: &redditPreview{Images: []previewImage{{
					Source: previewSource{URL: "https://preview.redd.it/0n5exiers7yh1.gif?s=gif"},
				}}},
			},
			want: []string{"https://i.redd.it/0n5exiers7yh1.gif"},
		},
		{
			name: "video beats preview",
			post: redditPost{
				IsVideo: true,
				Media:   &redditMedia{RedditVideo: &redditVideo{FallbackURL: "https://v.redd.it/abc/DASH_720.mp4"}},
				Preview: &redditPreview{Images: []previewImage{{Source: previewSource{URL: "https://preview.redd.it/zzz.jpg"}}}},
			},
			want: []string{"https://v.redd.it/abc/DASH_720.mp4"},
		},
		{
			name: "preview video variants",
			post: redditPost{
				Preview: &redditPreview{Images: []previewImage{{
					Source:   previewSource{URL: "https://preview.redd.it/still.jpg"},
					Variants: previewVariants{MP4: &previewVariant{Source: previewSource{URL: "https://preview.redd.it/0n5exiers7yh1.mp4?s=abc"}}},
				}}},
			},
			want: []string{"https://preview.redd.it/0n5exiers7yh1.mp4?s=abc"},
		},
		{
			name: "preview prefers the mp4 variant over the gif",
			post: redditPost{
				Preview: &redditPreview{Images: []previewImage{{
					Source: previewSource{URL: "https://preview.redd.it/still.jpg"},
					Variants: previewVariants{
						GIF: &previewVariant{Source: previewSource{URL: "https://preview.redd.it/0n5exiers7yh1.gif?s=abc"}},
						MP4: &previewVariant{Source: previewSource{URL: "https://preview.redd.it/0n5exiers7yh1.mp4?s=abc"}},
					},
				}}},
			},
			want: []string{"https://preview.redd.it/0n5exiers7yh1.mp4?s=abc"},
		},
		{
			name: "gallery flag without gallery data falls through",
			post: redditPost{IsGallery: true, URL: "https://i.redd.it/direct.jpg"},
			want: []string{"https://i.redd.it/direct.jpg"},
		},
		{
			name: "empty post",
			post: redditPost{},
			want: nil,
		},
		{
			name: "only non-reddit hosts is not a gallery",
			post: redditPost{URL: "https://cdn.example.com/a.png"},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMedia(tt.post)
			if len(got) != len(tt.want) {
				t.Fatalf("extractMedia() = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("media %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestOriginalURL(t *testing.T) {
	tests := []struct{ in, want string }{
		// Post JSON shape: bare media ID.
		{"https://preview.redd.it/4hpo47nlimsh1.jpg?width=4284&format=pjpg&auto=webp", "https://i.redd.it/4hpo47nlimsh1.jpg"},
		// Embed page shape: post slug in front of the media ID.
		{"https://preview.redd.it/from-recent-trip-to-austria-v0-4hpo47nlimsh1.jpg?width=640&amp;crop=smart&amp;auto=webp&amp;s=6799", "https://i.redd.it/4hpo47nlimsh1.jpg"},
		{"https://preview.redd.it/some-post-title-v12-8nzalwmlimsh1.png", "https://i.redd.it/8nzalwmlimsh1.png"},
		// Already original, or not a preview host: untouched.
		{"https://i.redd.it/4hpo47nlimsh1.jpg", "https://i.redd.it/4hpo47nlimsh1.jpg"},
		{"https://external-preview.redd.it/AbCdEfGh.png?auto=webp", "https://external-preview.redd.it/AbCdEfGh.png?auto=webp"},
		{"https://v.redd.it/abc/DASH_720.mp4", "https://v.redd.it/abc/DASH_720.mp4"},
		// format=png8 exists only under preview.redd.it; rewriting it to
		// i.redd.it 404s, so the resized still is kept instead.
		{"https://preview.redd.it/x-v0-abc123.gif?format=png8&s=zz", "https://preview.redd.it/x-v0-abc123.gif?format=png8&s=zz"},
		{"https://preview.redd.it/x-v0-abc123.png?format=png8&s=zz", "https://preview.redd.it/x-v0-abc123.png?format=png8&s=zz"},
		// mp4/webm renditions are served only by preview.redd.it, and their
		// signature must survive; rewriting them to i.redd.it 404s.
		{"https://preview.redd.it/0n5exiers7yh1.mp4?format=mp4&s=zz", "https://preview.redd.it/0n5exiers7yh1.mp4?format=mp4&s=zz"},
		{"https://preview.redd.it/0n5exiers7yh1.gif?format=mp4&s=zz", "https://preview.redd.it/0n5exiers7yh1.gif?format=mp4&s=zz"},
		// No usable media ID: left alone rather than mangled.
		{"https://preview.redd.it/noextension", "https://preview.redd.it/noextension"},
		{"https://preview.redd.it/short.zzz", "https://preview.redd.it/short.zzz"},
		{"https://preview.redd.it/" + strings.Repeat("a", 41) + ".png", "https://preview.redd.it/" + strings.Repeat("a", 41) + ".png"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := originalURL(tt.in); got != tt.want {
			t.Errorf("originalURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDetectExtension(t *testing.T) {
	tests := []struct{ url, ct, want string }{
		{"https://i.redd.it/abc.png", "", ".png"},
		{"https://i.redd.it/abc.jpg?width=640", "", ".jpg"},
		{"https://v.redd.it/abc/DASH_720.mp4", "", ".mp4"},
		{"https://i.redd.it/abc.gif", "", ".gif"},
		// Reddit's format parameter overrides a path named after the original.
		{"https://preview.redd.it/abc.gif?format=png8&s=x", "", ".png"},
		{"https://preview.redd.it/abc.gif?format=mp4&s=x", "", ".mp4"},
		{"https://i.redd.it/abc", "image/png", ".png"},
		{"https://i.redd.it/abc", "image/jpeg; charset=utf-8", ".jpg"},
		{"https://i.redd.it/abc", "video/mp4", ".mp4"},
		{"https://i.redd.it/abc", "application/octet-stream", ".jpg"},
		{"https://i.redd.it/abc", "", ".jpg"},
	}
	for _, tt := range tests {
		if got := detectExtension(tt.url, tt.ct); got != tt.want {
			t.Errorf("detectExtension(%q, %q) = %q, want %q", tt.url, tt.ct, got, tt.want)
		}
	}
}

func TestURLExt(t *testing.T) {
	tests := []struct{ url, want string }{
		{"https://i.redd.it/abc.png", ".png"},
		{"https://i.redd.it/abc.JPG?width=640", ".jpg"},
		{"https://preview.redd.it/abc.gif?format=png8&s=x", ".png"},
		{"https://preview.redd.it/abc.gif?format=mp4&s=x", ".mp4"},
		// Unknown formats fall back to the path rather than inventing one.
		{"https://preview.redd.it/abc.jpg?format=pjpg&auto=webp", ".jpg"},
		{"https://i.redd.it/abc", ""},
	}
	for _, tt := range tests {
		if got := urlExt(tt.url); got != tt.want {
			t.Errorf("urlExt(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// TestArchiveFixtureIsParsed guards the shape of a real arctic-shift response,
// so a field rename upstream shows up as a test failure rather than as empty
// galleries in production.
func TestArchiveFixtureIsParsed(t *testing.T) {
	raw, err := os.ReadFile("testdata/archive_gallery.json")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Data []redditPost `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 1 {
		t.Fatalf("fixture has %d posts, want 1", len(payload.Data))
	}
	post := payload.Data[0]

	gallery, err := galleryFromPost(post)
	if err != nil {
		t.Fatalf("galleryFromPost: %v", err)
	}
	if gallery.Title != "From recent trip to Austria" {
		t.Errorf("title = %q", gallery.Title)
	}
	// Six images, and every one must be the original i.redd.it upload rather
	// than the re-encoded preview the JSON actually points at.
	if len(gallery.Media) != 6 {
		t.Fatalf("got %d images, want 6: %v", len(gallery.Media), gallery.Media)
	}
	for i, u := range gallery.Media {
		if !strings.HasPrefix(u, "https://i.redd.it/") {
			t.Errorf("image %d = %q, want an i.redd.it original", i, u)
		}
		if strings.Contains(u, "preview.redd.it") || strings.Contains(u, "width=") {
			t.Errorf("image %d = %q, want the original, not a resized preview", i, u)
		}
	}
	want := []string{
		"https://i.redd.it/4hpo47nlimsh1.jpg",
		"https://i.redd.it/lb39p6nlimsh1.jpg",
		"https://i.redd.it/iu9rbumlimsh1.jpg",
		"https://i.redd.it/cvl0x2nlimsh1.jpg",
		"https://i.redd.it/8nzalwmlimsh1.jpg",
		"https://i.redd.it/wdrfb3nlimsh1.jpg",
	}
	for i := range want {
		if gallery.Media[i] != want[i] {
			t.Errorf("image %d = %q, want %q (order must follow gallery_data.items)",
				i, gallery.Media[i], want[i])
		}
	}
}
