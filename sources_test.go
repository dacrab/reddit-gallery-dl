package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

// testFetcher builds a Fetcher whose every client shares one transport, so a
// test can describe the whole network in one function.
func testFetcher(rt http.RoundTripper) *Fetcher {
	f := NewFetcher()
	f.client = &http.Client{Transport: rt}
	f.noRedirect = &http.Client{Transport: rt, CheckRedirect: refuseRedirect}
	f.mediaClient = &http.Client{Transport: rt}
	return f
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestEmbedFixture pins the parser against a real embed page. The page carries a
// subreddit icon on a non-media host and an <img> with no src at all; both are
// in the fixture precisely because a naive scan used to pick them up.
func TestEmbedFixture(t *testing.T) {
	gallery := galleryFromEmbedHTML(readFixture(t, "embed_gallery.html"))
	if gallery == nil {
		t.Fatal("galleryFromEmbedHTML returned nil for a real six-image gallery")
	}
	if gallery.Title != "From recent trip to Austria" {
		t.Errorf("title = %q", gallery.Title)
	}
	if len(gallery.Media) != 6 {
		t.Fatalf("got %d images, want 6: %v", len(gallery.Media), gallery.Media)
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
			t.Errorf("image %d = %q, want %q", i, gallery.Media[i], want[i])
		}
	}
	for _, u := range gallery.Media {
		if strings.Contains(u, "redditmedia.com") {
			t.Errorf("subreddit icon leaked into the gallery: %q", u)
		}
		if strings.Contains(u, "preview.redd.it") {
			t.Errorf("expected the i.redd.it original, got a resized preview: %q", u)
		}
	}
}

func TestGalleryFromEmbedHTML_NoMedia(t *testing.T) {
	for name, page := range map[string]string{
		"empty":        "",
		"text post":    "<h1>Just a thought</h1><div>no media here</div>",
		"block page":   "<!DOCTYPE html><html><body>Blocked</body></html>",
		"stylesheet":   `<link href="https://preview.redd.it/abc12345.jpg" rel="preload">`,
		"external img": `<img src="https://cdn.example.com/a.png">`,
	} {
		if got := galleryFromEmbedHTML(page); got != nil {
			t.Errorf("%s: got %+v, want nil (must not be mistaken for media)", name, got)
		}
	}
}

func TestGalleryFromEmbedHTML_PrefersImagesOverVideo(t *testing.T) {
	page := `<h1>Mixed</h1>
		<video src="https://v.redd.it/abc/DASH_720.mp4"></video>
		<img src="https://preview.redd.it/one-v0-4hpo47nlimsh1.jpg?width=640">`
	gallery := galleryFromEmbedHTML(page)
	if gallery == nil {
		t.Fatal("nil gallery")
	}
	if len(gallery.Media) != 1 || gallery.Media[0] != "https://i.redd.it/4hpo47nlimsh1.jpg" {
		t.Errorf("images = %v, want the gallery image", gallery.Media)
	}
}

func TestGalleryFromEmbedHTML_VideoOnly(t *testing.T) {
	page := `<h1>Clip</h1><video src="https://v.redd.it/abc/DASH_720.mp4"></video>`
	gallery := galleryFromEmbedHTML(page)
	if gallery == nil {
		t.Fatal("nil gallery")
	}
	if len(gallery.Media) != 1 || !strings.HasPrefix(gallery.Media[0], "https://v.redd.it/") {
		t.Errorf("images = %v, want the v.redd.it clip", gallery.Media)
	}
}

// TestSourceOrder pins the ladder. The archives answer first because they return
// Reddit's own JSON shape and survive a front-end redesign; Reddit's own
// endpoint is last because it answers anonymous traffic with 403s.
func TestSourceOrder(t *testing.T) {
	want := []string{"arctic-shift", "embed", "pullpush", "reddit"}
	if len(sources) != len(want) {
		t.Fatalf("ladder has %d rungs, want %d", len(sources), len(want))
	}
	for i, name := range want {
		if sources[i].name != name {
			t.Errorf("rung %d = %q, want %q", i, sources[i].name, name)
		}
	}
}

// TestLoginWallDoesNotLookLikeADeletedPost is a regression test: old.reddit.com
// redirects anonymous JSON requests to /login/, the client follows it to a 404,
// and that 404 was once reported as "post not found" for live posts.
func TestLoginWallDoesNotLookLikeADeletedPost(t *testing.T) {
	embed := readFixture(t, "embed_gallery.html")
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "embed.reddit.com":
			return testResponse(r, http.StatusOK, embed), nil
		case "arctic-shift.photon-reddit.com", "api.pullpush.io",
			"www.reddit.com", "old.reddit.com", "api.reddit.com":
			resp := testResponse(r, http.StatusNotFound, "Not Found")
			// The client followed a redirect to the login wall before 404ing.
			redirected := *r
			redirected.URL = &url.URL{Scheme: "https", Host: "www.reddit.com", Path: "/login/?reason=lor2"}
			resp.Request = &redirected
			return resp, nil
		default:
			t.Fatalf("unexpected request to %s", r.URL.Host)
			return nil, nil
		}
	}))

	gallery, err := f.Gallery(context.Background(), "https://www.reddit.com/r/pics/comments/1wu034e/x/")
	if err != nil {
		t.Fatalf("a login wall must not be reported as a missing post: %v", err)
	}
	if len(gallery.Media) != 6 {
		t.Errorf("got %d images, want 6", len(gallery.Media))
	}
}

// TestLoginWallAloneIsUnavailable checks the verdict when nothing succeeds: a
// block is not evidence of deletion, so the user must not be told the post is
// gone.
func TestLoginWallAloneIsUnavailable(t *testing.T) {
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "embed.reddit.com" {
			return testResponse(r, http.StatusOK, "<h1>Nothing here</h1>"), nil
		}
		if isRedditHost(r.URL.Host) {
			resp := testResponse(r, http.StatusNotFound, "Not Found")
			redirected := *r
			redirected.URL = &url.URL{Scheme: "https", Host: "www.reddit.com", Path: "/login/"}
			resp.Request = &redirected
			return resp, nil
		}
		return testResponse(r, http.StatusForbidden, ""), nil
	}))

	_, err := f.Gallery(context.Background(), "https://www.reddit.com/r/pics/comments/1wu034e/x/")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrPostNotFound) {
		t.Errorf("blocked requests were reported as a deleted post: %v", err)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}

// TestEverySourceAgreesPostIsGone is the case where "deleted" is a fair thing
// to say: every source positively reported the post as absent.
func TestEverySourceAgreesPostIsGone(t *testing.T) {
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testResponse(r, http.StatusNotFound, ""), nil
	}))

	_, err := f.Gallery(context.Background(), "https://www.reddit.com/r/pics/comments/1wu034e/x/")
	if !errors.Is(err, ErrPostNotFound) {
		t.Errorf("err = %v, want ErrPostNotFound", err)
	}
}

func TestEmbedNeverShortCircuits(t *testing.T) {
	// The embed page is scraped markup: an empty result might be a text post, a
	// block page, or a redesign. None of those may stop the ladder, because a
	// markup change would otherwise take the whole tool down with it.
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Host == "embed.reddit.com":
			return testResponse(r, http.StatusOK, "<h1>Redesigned</h1><div>no imgs</div>"), nil
		case isRedditHost(r.URL.Host):
			return testResponse(r, http.StatusForbidden, ""), nil
		default:
			return testResponse(r, http.StatusOK, `{"data":[{"title":"From the archive","url_overridden_by_dest":"https://i.redd.it/a.jpg"}]}`), nil
		}
	}))

	gallery, err := f.Gallery(context.Background(), "https://www.reddit.com/r/pics/comments/1wu034e/x/")
	if err != nil {
		t.Fatalf("embed's empty result stopped the ladder: %v", err)
	}
	if gallery.Title != "From the archive" {
		t.Errorf("title = %q", gallery.Title)
	}
}

func TestMediaLessPostShortCircuits(t *testing.T) {
	// A text post is definitive: every source can see there is no media, so
	// there is nothing to gain from asking again.
	calls := 0
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if isRedditHost(r.URL.Host) {
			return testResponse(r, http.StatusForbidden, ""), nil
		}
		return testResponse(r, http.StatusOK, `{"data":[{"title":"A thought","is_self":true}]}`), nil
	}))

	_, err := f.Gallery(context.Background(), "https://www.reddit.com/r/dividends/comments/1d30jbl/x/")
	if !errors.Is(err, ErrNoMedia) {
		t.Fatalf("err = %v, want ErrNoMedia", err)
	}
	if calls != 1 {
		t.Errorf("made %d requests, want 1: a media-less post should stop the ladder", calls)
	}
}

func TestNonJSONRedditResponseIsABlock(t *testing.T) {
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "embed.reddit.com" {
			return testResponse(r, http.StatusOK, readFixture(t, "embed_gallery.html")), nil
		}
		// A 200 that is HTML is Reddit's WAF.
		return testResponse(r, http.StatusOK, "<!DOCTYPE html><html><body>Blocked</body></html>"), nil
	}))

	gallery, err := f.Gallery(context.Background(), "https://www.reddit.com/r/pics/comments/1wu034e/x/")
	if err != nil {
		t.Fatalf("WAF page should have been treated as inconclusive: %v", err)
	}
	if len(gallery.Media) != 6 {
		t.Errorf("got %d images, want 6", len(gallery.Media))
	}
}

func TestRawJSONFlagIsSent(t *testing.T) {
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "www.reddit.com" {
			if r.URL.Query().Get("raw_json") != "1" {
				t.Errorf("missing raw_json=1, query = %q", r.URL.RawQuery)
			}
			if !strings.HasPrefix(r.URL.Path, "/comments/1wu034e") {
				t.Errorf("path = %q, want a slug-free /comments/<id> path", r.URL.Path)
			}
		}
		return testResponse(r, http.StatusNotFound, ""), nil
	}))
	_, _ = f.Gallery(context.Background(), "https://www.reddit.com/r/pics/comments/1wu034e/some/slug/")
}

// TestShareLinkFollowsRedirect covers the /r/<sub>/s/<code> shape end to end.
func TestShareLinkFollowsRedirect(t *testing.T) {
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/s/") {
			resp := testResponse(r, http.StatusFound, "")
			resp.Header.Set("Location", "https://www.reddit.com/r/pics/comments/1wu034e/from_recent_trip_to_austria/")
			return resp, nil
		}
		if r.URL.Host == "embed.reddit.com" {
			return testResponse(r, http.StatusOK, readFixture(t, "embed_gallery.html")), nil
		}
		return testResponse(r, http.StatusForbidden, ""), nil
	}))

	gallery, err := f.Gallery(context.Background(), "https://www.reddit.com/r/pics/s/AbCdEf12")
	if err != nil {
		t.Fatalf("share link did not resolve: %v", err)
	}
	if len(gallery.Media) != 6 {
		t.Errorf("got %d images, want 6", len(gallery.Media))
	}
}

func TestShareLinkFailure(t *testing.T) {
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testResponse(r, http.StatusNotFound, ""), nil
	}))
	_, err := f.Gallery(context.Background(), "https://www.reddit.com/r/pics/s/AbCdEf12")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "full post URL") {
		t.Errorf("error should tell the user what to do instead: %v", err)
	}
}

// --- SSRF -----------------------------------------------------------------

// hostileURLs must be refused by every layer that accepts a user-supplied URL
// directly: the form check and the fetcher. Keeping one list means a new class
// of input cannot be covered at one layer and forgotten at another. The
// redirect guard is tested separately because it sees already-parsed hosts.
var hostileURLs = []string{
	"http://127.0.0.1:8080/latest/meta-data/",
	"https://169.254.169.254/latest/meta-data/iam/security-credentials/",
	"http://localhost:5000/admin",
	"http://[::1]:9000/",
	"http://10.1.2.3/internal.png",
	"http://i.redd.it/a.jpg",                            // plaintext
	"https://www.reddit.com/r/pics/comments/1wu034e/x/", // Reddit, but not media
	"https://evil.com/a.jpg",
	"https://evil.com/i.redd.it/a.png",
	"https://i.redd.it.evil.com/a.png",
	"https://i.redd.it@evil.com/a.jpg",
	"https://b.thumbs.redditmedia.com/a.png",
	"//i.redd.it/a.jpg",
	"file:///etc/passwd",
	"gopher://127.0.0.1:11211/_stats",
	"",
}

func TestStreamMediaRefusesNonRedditHosts(t *testing.T) {
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("fetcher reached %s; it must refuse before dialling", r.URL)
		return testResponse(r, http.StatusOK, "SECRET"), nil
	}))

	for _, raw := range hostileURLs {
		if _, _, err := f.streamMedia(context.Background(), raw); err == nil {
			t.Errorf("streamMedia(%q) succeeded; want refusal", raw)
		}
	}
}

func TestStreamMediaAllowsRedditMedia(t *testing.T) {
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testResponse(r, http.StatusOK, "IMAGEBYTES"), nil
	}))
	for _, raw := range []string{
		"https://i.redd.it/abc123.jpg",
		"https://preview.redd.it/abc123.jpg?width=640&auto=webp",
		"https://v.redd.it/abc/DASH_720.mp4",
	} {
		body, ext, err := f.streamMedia(context.Background(), raw)
		if err != nil {
			t.Errorf("streamMedia(%q): %v", raw, err)
			continue
		}
		got, _ := io.ReadAll(body)
		_ = body.Close()
		if string(got) != "IMAGEBYTES" {
			t.Errorf("streamMedia(%q) body = %q", raw, got)
		}
		if ext == "" {
			t.Errorf("streamMedia(%q) returned no extension", raw)
		}
	}
}

func TestRedirectGuard(t *testing.T) {
	media := guardRedirect(isRedditMediaHost)
	req := func(host string) *http.Request {
		r, _ := http.NewRequest("GET", "https://"+host+"/x", nil)
		return r
	}
	if err := media(req("i.redd.it"), nil); err != nil {
		t.Errorf("media host rejected: %v", err)
	}
	for _, host := range []string{"evil.com", "169.254.169.254", "i.redd.it.evil.com", "www.reddit.com"} {
		if err := media(req(host), nil); err == nil {
			t.Errorf("media guard allowed %q", host)
		}
	}
	// Downgrading to plaintext must not be allowed either.
	plain, _ := http.NewRequest("GET", "http://i.redd.it/x", nil)
	if err := media(plain, nil); err == nil {
		t.Error("media guard allowed a plaintext hop")
	}

	// A media fetch may be redirected to www.reddit.com/media, which proxies
	// the file; the chain just has to stay on Reddit.
	site := guardRedirect(isRedditHost)
	hop, _ := http.NewRequest("GET", "https://www.reddit.com/media?url=https%3A%2F%2Fi.redd.it%2Fa.jpg", nil)
	if err := site(hop, nil); err != nil {
		t.Errorf("reddit's own /media proxy was rejected: %v", err)
	}
	offsite, _ := http.NewRequest("GET", "https://evil.com/a.jpg", nil)
	if err := site(offsite, nil); err == nil {
		t.Error("site guard followed a redirect off Reddit")
	}
}

// TestMediaRequestsAdvertiseAnImage pins the Accept header used for media:
// asking for text/html makes Reddit 307 to its /media proxy instead of serving
// the file from its CDN.
func TestMediaRequestsAdvertiseAnImage(t *testing.T) {
	var accept string
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		accept = r.Header.Get("Accept")
		return testResponse(r, http.StatusOK, "BYTES"), nil
	}))
	body, _, err := f.streamMedia(context.Background(), "https://i.redd.it/abc123.jpg")
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()
	if !strings.HasPrefix(accept, "image/") {
		t.Errorf("media Accept = %q, want it to start with image/", accept)
	}
	if strings.Contains(accept, "text/html") {
		t.Errorf("media Accept = %q, must not offer text/html", accept)
	}
}

// TestEmbedRungAsksForTheSubreddit guards the embed rung on its own.
//
// embed.reddit.com/comments/<id>/ answers HTTP 200 with a "Page not found" body
// and no media, so a slug-free request looks like a success while returning an
// empty gallery. The subreddit has to be in the path.
func TestEmbedRungAsksForTheSubreddit(t *testing.T) {
	var embedPath string
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "embed.reddit.com":
			embedPath = r.URL.Path
			return testResponse(r, http.StatusOK, readFixture(t, "embed_gallery.html")), nil
		case "arctic-shift.photon-reddit.com", "api.pullpush.io":
			return testResponse(r, http.StatusForbidden, ""), nil
		default:
			return testResponse(r, http.StatusForbidden, ""), nil
		}
	}))

	gallery, err := f.Gallery(context.Background(),
		"https://www.reddit.com/r/pics/comments/1wu034e/from_recent_trip_to_austria/")
	if err != nil {
		t.Fatalf("embed rung failed: %v", err)
	}
	if want := "/r/pics/comments/1wu034e/"; embedPath != want {
		t.Errorf("embed path = %q, want %q", embedPath, want)
	}
	if len(gallery.Media) != 6 {
		t.Errorf("got %d images, want 6", len(gallery.Media))
	}
}

func TestEmbedRungSkippedWithoutSubreddit(t *testing.T) {
	// redd.it shortlinks and /gallery/ links carry no subreddit, so the embed
	// rung has to bow out rather than request a URL that renders "Page not
	// found" under a 200.
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "embed.reddit.com" {
			t.Errorf("embed rung asked for %s with no subreddit to build the path from", r.URL)
		}
		if isRedditHost(r.URL.Host) {
			return testResponse(r, http.StatusForbidden, ""), nil
		}
		return testResponse(r, http.StatusOK,
			`{"data":[{"title":"Short","url_overridden_by_dest":"https://i.redd.it/abc123.jpg"}]}`), nil
	}))

	gallery, err := f.Gallery(context.Background(), "https://redd.it/1wu034e")
	if err != nil {
		t.Fatalf("shortlink failed: %v", err)
	}
	if gallery.Title != "Short" {
		t.Errorf("title = %q", gallery.Title)
	}
}

func TestSubredditOf(t *testing.T) {
	for _, s := range []string{"pics", "aww", "AskReddit", "a_b_c", "x1"} {
		if subredditOf(s) != s {
			t.Errorf("subredditOf(%q) = %q, want it kept", s, subredditOf(s))
		}
	}
	// "all" is a real path segment that is not a usable subreddit, and anything
	// that could escape the path or confuse a request line must be dropped.
	for _, s := range []string{"all", "", "..", "a/b", "a b", "a\nb", strings.Repeat("x", 33)} {
		if subredditOf(s) != "" {
			t.Errorf("subredditOf(%q) = %q, want it dropped", s, subredditOf(s))
		}
	}
}
