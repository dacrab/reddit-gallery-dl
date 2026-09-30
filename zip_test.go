package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// imageFetcher serves fake Reddit media. failed URLs answer 404 so the
// download-skip path can be exercised.
func imageFetcher(failing ...string) *Fetcher {
	return testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		for _, f := range failing {
			if strings.Contains(r.URL.Path, f) {
				return testResponse(r, http.StatusNotFound, ""), nil
			}
		}
		return testResponse(r, http.StatusOK, "BYTES:"+r.URL.Path), nil
	}))
}

func postForm(t *testing.T, srvURL string, imageURLs []string, title string) *http.Request {
	t.Helper()
	form := url.Values{"page_title": {title}}
	for _, u := range imageURLs {
		form.Add("image_urls", u)
	}
	req := httptest.NewRequest(http.MethodPost, srvURL+"/download-zip", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

func zipEntries(t *testing.T, body []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("unreadable zip (%d bytes): %v", len(body), err)
	}
	out := make(map[string]string, len(zr.File))
	for _, file := range zr.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[file.Name] = string(content)
	}
	return out
}

func galleryURLs(n int) []string {
	urls := make([]string, n)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://i.redd.it/img%02d.png", i)
	}
	return urls
}

// TestZipKeepsSelectionOrder is the ordinary case: every download succeeds and
// image_NNN lines up with the Nth URL the user ticked.
func TestZipKeepsSelectionOrder(t *testing.T) {
	f := imageFetcher()
	urls := galleryURLs(5)
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, "http://x", urls, "My Post"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	entries := zipEntries(t, rec.Body.Bytes())
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5: %v", len(entries), entries)
	}
	for i, u := range urls {
		name := fmt.Sprintf("image_%03d.png", i+1)
		want := "BYTES:/" + strings.TrimPrefix(u, "https://i.redd.it/")
		if entries[name] != want {
			t.Errorf("%s = %q, want %q", name, entries[name], want)
		}
	}
}

// TestZipSurvivesAFirstImageFailure is the regression test for the bug that made
// downloads silently produce an empty archive.
//
// The reordering loop waited for image 1 before writing anything. One dead image
// at the head of the list meant every later image downloaded, buffered, and was
// then thrown away unopened — the user got a valid but empty ZIP. Signed Reddit
// preview URLs expire, so the first image failing was routine, not rare.
func TestZipSurvivesAFirstImageFailure(t *testing.T) {
	f := imageFetcher("img00")
	urls := galleryURLs(4)
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, "http://x", urls, "My Post"))

	entries := zipEntries(t, rec.Body.Bytes())
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (only the first image should be lost): %v",
			len(entries), entries)
	}
	// Numbering must stay contiguous so the archive is not full of gaps.
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		if want := fmt.Sprintf("image_%03d.png", i+1); name != want {
			t.Errorf("entry %d = %q, want %q (numbering must have no gaps)", i, name, want)
		}
	}
}

func TestZipSurvivesAMiddleAndLastFailure(t *testing.T) {
	f := imageFetcher("img01", "img03")
	urls := galleryURLs(5)
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, "http://x", urls, "My Post"))

	entries := zipEntries(t, rec.Body.Bytes())
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %v", len(entries), entries)
	}
	for _, name := range []string{"image_001.png", "image_002.png", "image_003.png"} {
		if _, ok := entries[name]; !ok {
			t.Errorf("missing %s: %v", name, entries)
		}
	}
	// Surviving images keep their content, not their original position.
	if entries["image_002.png"] != "BYTES:/img02.png" {
		t.Errorf("image_002.png = %q, want img02", entries["image_002.png"])
	}
}

func TestZipSurvivesEveryDownloadFailing(t *testing.T) {
	urls := galleryURLs(3)
	f := imageFetcher("img00", "img01", "img02")
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, "http://x", urls, "My Post"))

	// An empty archive is still a valid, openable ZIP.
	entries := zipEntries(t, rec.Body.Bytes())
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0: %v", len(entries), entries)
	}
}

func TestDownloadZipRejectsNonRedditURLs(t *testing.T) {
	// The regression test for the open-proxy hole: /download-zip took
	// image_urls straight from the form and fetched whatever it was given,
	// including loopback and link-local addresses.
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("fetcher dialled %s; the request should have been rejected up front", r.URL)
		return testResponse(r, http.StatusOK, "SECRET-INTERNAL-RESPONSE"), nil
	}))

	for _, hostile := range []string{
		"http://127.0.0.1:8080/latest/meta-data/",
		"https://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://localhost:5000/admin",
		"http://[::1]:9000/",
		"http://10.1.2.3/internal.png",
		"https://evil.com/i.redd.it/a.png",
		"https://i.redd.it.evil.com/a.png",
		"https://b.thumbs.redditmedia.com/a.png",
		"file:///etc/passwd",
		"gopher://127.0.0.1:11211/_stats",
	} {
		rec := httptest.NewRecorder()
		// A valid URL first, so the rejection cannot be blamed on an empty list.
		handleDownloadZip(f)(rec, postForm(t, "http://x", []string{"https://i.redd.it/ok.png", hostile}, "t"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("hostile URL %q: status = %d, want 400", hostile, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "SECRET-INTERNAL-RESPONSE") {
			t.Errorf("hostile URL %q leaked internal content", hostile)
		}
	}
}

func TestDownloadZipRejectsBeforeWritingAnything(t *testing.T) {
	// A 400 is only possible if validation happens before the first ZIP byte.
	f := imageFetcher()
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, "http://x", []string{"https://evil.com/a.png"}, "t"))
	if ct := rec.Header().Get("Content-Type"); ct == "application/zip" {
		t.Errorf("rejected request still advertised %s", ct)
	}
}

func TestSingleImageIsServedDirectly(t *testing.T) {
	f := imageFetcher()
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, "http://x", []string{"https://i.redd.it/solo.jpg"}, "t"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", ct)
	}
	if got := rec.Body.String(); got != "BYTES:/solo.jpg" {
		t.Errorf("body = %q", got)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "solo.jpg") {
		t.Errorf("content-disposition = %q", cd)
	}
}

func TestDownloadZipCapsTheNumberOfImages(t *testing.T) {
	f := imageFetcher()
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, "http://x", galleryURLs(maxURLs+20), "t"))
	if got := len(zipEntries(t, rec.Body.Bytes())); got != maxURLs {
		t.Errorf("got %d entries, want the cap of %d", got, maxURLs)
	}
}

func TestDownloadZipRequiresImages(t *testing.T) {
	f := imageFetcher()
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, "http://x", nil, "t"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestDownloadZipRedirectsNonPost(t *testing.T) {
	f := imageFetcher()
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, httptest.NewRequest(http.MethodGet, "/download-zip", nil))
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
}

func TestDownloadZipContentDisposition(t *testing.T) {
	f := imageFetcher()
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, "http://x", galleryURLs(2), "My Great Post! (2024)"))
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "My_Great_Post_2024.zip") {
		t.Errorf("content-disposition = %q", cd)
	}
}

func TestIsFetchableImageURL(t *testing.T) {
	for _, raw := range []string{
		"https://i.redd.it/a.jpg",
		"https://preview.redd.it/a.jpg?width=640&auto=webp",
		"https://v.redd.it/a/DASH_720.mp4",
		"https://external-preview.redd.it/a.png",
	} {
		if !isFetchableImageURL(raw) {
			t.Errorf("isFetchableImageURL(%q) = false, want true", raw)
		}
	}
	for _, raw := range []string{
		"http://i.redd.it/a.jpg", // plaintext
		"https://www.reddit.com/r/pics/comments/1wu034e/x/",
		"https://evil.com/a.jpg",
		"https://i.redd.it@evil.com/a.jpg",
		"//i.redd.it/a.jpg",
		"",
	} {
		if isFetchableImageURL(raw) {
			t.Errorf("isFetchableImageURL(%q) = true, want false", raw)
		}
	}
}

func TestCleanFilename(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "reddit_gallery"},
		{"My Post", "My_Post"},
		{"a/b\\c:d*e?f", "abcdef"},
		{"Ünïcodé 123", "Ünïcodé_123"},
		{"...", "reddit_gallery"},
		{"keep-me_1", "keep-me_1"},
	}
	for _, tt := range tests {
		if got := cleanFilename(tt.in); got != tt.want {
			t.Errorf("cleanFilename(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// --- index handler --------------------------------------------------------

func testTemplates(t *testing.T) *template.Template {
	t.Helper()
	return template.Must(template.New("").
		Funcs(template.FuncMap{"urlExt": urlExt}).
		ParseGlob("templates/*.html"))
}

func TestIndexRendersGallery(t *testing.T) {
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testResponse(r, http.StatusOK,
			`{"data":[{"title":"Six snaps","is_gallery":true,"gallery_data":{"items":[{"media_id":"a"}]},"media_metadata":{"a":{"s":{"u":"https://preview.redd.it/aaaabbbbcccc.jpg?width=108"}}}}]}`), nil
	}))
	handler := routes(testTemplates(t), f)

	form := url.Values{"url": {"https://www.reddit.com/r/pics/comments/1wu034e/x/"}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Six snaps", "https://i.redd.it/aaaabbbbcccc.jpg"} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q", want)
		}
	}
}

func TestIndexRejectsBadURL(t *testing.T) {
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request for a bad URL: %s", r.URL)
		return nil, nil
	}))
	handler := routes(testTemplates(t), f)

	form := url.Values{"url": {"https://example.com/not/reddit"}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "valid Reddit link") {
		t.Errorf("expected the invalid-link alert, got:\n%s", rec.Body.String())
	}
}

func TestIndexReportsBlockedRatherThanDeleted(t *testing.T) {
	// The headline bug: a login-walled Reddit must never be reported to the
	// user as a deleted post.
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if isRedditHost(r.URL.Host) {
			resp := testResponse(r, http.StatusNotFound, "")
			redirected := *r
			redirected.URL = &url.URL{Scheme: "https", Host: "www.reddit.com", Path: "/login/"}
			resp.Request = &redirected
			return resp, nil
		}
		return testResponse(r, http.StatusForbidden, ""), nil
	}))
	handler := routes(testTemplates(t), f)

	form := url.Values{"url": {"https://www.reddit.com/r/pics/comments/1wu034e/x/"}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "Post not found") {
		t.Errorf("a blocked fetch was reported as a deleted post:\n%s", body)
	}
	if !strings.Contains(body, "Couldn&#39;t reach any source") {
		t.Errorf("expected the unavailable alert, got:\n%s", body)
	}
}

func TestIndexGETIsNotFound(t *testing.T) {
	srv := httptest.NewServer(routes(testTemplates(t), imageFetcher()))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/somewhere-else")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAlertForError(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{ErrInvalidURL, "valid Reddit link"},
		{fmt.Errorf("wrapped: %w", ErrPostNotFound), "Post not found"},
		{ErrNoMedia, "no images"},
		{fmt.Errorf("%w: %w", ErrUnavailable, ErrPostNotFound), "Couldn't reach any source"},
		{errors.New("boom"), "Something went wrong"},
	}
	for _, tt := range tests {
		got := alertForError(tt.err)
		if got == nil || !strings.Contains(got.Message, tt.want) {
			t.Errorf("alertForError(%v) = %+v, want a message containing %q", tt.err, got, tt.want)
		}
	}
}

func TestContextCancellationDoesNotHang(t *testing.T) {
	release := make(chan struct{})
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return testResponse(r, http.StatusOK, "BYTES"), nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req := postForm(t, "http://x", galleryURLs(6), "t").WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleDownloadZip(f)(rec, req)
	}()
	cancel()
	close(release)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler hung after its context was cancelled")
	}
}

// TestZipHandlesMoreURLsThanWorkers is a deadlock guard.
//
// The writer releases download slots by closing bodies, and it only closes a
// body when that body reaches its turn. A design where every URL gets its own
// goroutine, all of which queue on a process-wide slot cap, deadlocks here: the
// workers holding slots wait for the lowest index, and the goroutine owning the
// lowest index waits for a slot.
func TestZipHandlesMoreURLsThanWorkers(t *testing.T) {
	f := imageFetcher("img00", "img01")
	urls := galleryURLs(maxParallelDownloads * 6)

	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		handleDownloadZip(f)(rec, postForm(t, "http://x", urls, "t"))
		if got := len(zipEntries(t, rec.Body.Bytes())); got != len(urls)-2 {
			t.Errorf("got %d entries, want %d", got, len(urls)-2)
		}
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("deadlocked: %d URLs through a pool of %d", len(urls), maxParallelDownloads)
	}
}

// TestZipCapsConcurrentStreams checks the process-wide cap is respected while
// the pool is saturated, and that slots come back as bodies are closed.
func TestZipCapsConcurrentStreams(t *testing.T) {
	var mu sync.Mutex
	var open, peak int
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		open++
		if open > peak {
			peak = open
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		open--
		mu.Unlock()
		return testResponse(r, http.StatusOK, "BYTES"), nil
	}))

	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, "http://x", galleryURLs(maxURLs), "t"))

	if got := len(zipEntries(t, rec.Body.Bytes())); got != maxURLs {
		t.Fatalf("got %d entries, want %d", got, maxURLs)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak > maxParallelDownloads {
		t.Errorf("peak concurrent requests = %d, want at most %d", peak, maxParallelDownloads)
	}
	if peak < 2 {
		t.Errorf("peak concurrent requests = %d; downloads were not parallel at all", peak)
	}
}
