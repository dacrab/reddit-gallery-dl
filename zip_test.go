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

// mediaFetcher serves fake Reddit media. failed URLs answer 404 so the
// download-skip path can be exercised.
func mediaFetcher(failing ...string) *Fetcher {
	return testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		for _, f := range failing {
			if strings.Contains(r.URL.Path, f) {
				return testResponse(r, http.StatusNotFound, ""), nil
			}
		}
		return testResponse(r, http.StatusOK, "BYTES:"+r.URL.Path), nil
	}))
}

func postForm(t *testing.T, mediaURLs []string, title string) *http.Request {
	t.Helper()
	form := url.Values{"page_title": {title}}
	for _, u := range mediaURLs {
		form.Add("media_urls", u)
	}
	req := httptest.NewRequest(http.MethodPost, "/download-zip", strings.NewReader(form.Encode()))
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
		_ = rc.Close()
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
// media_NNN lines up with the Nth URL the user ticked.
func TestZipKeepsSelectionOrder(t *testing.T) {
	f := mediaFetcher()
	urls := galleryURLs(5)
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, urls, "My Post"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	entries := zipEntries(t, rec.Body.Bytes())
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5: %v", len(entries), entries)
	}
	for i, u := range urls {
		name := fmt.Sprintf("media_%03d.png", i+1)
		want := "BYTES:/" + strings.TrimPrefix(u, "https://i.redd.it/")
		if entries[name] != want {
			t.Errorf("%s = %q, want %q", name, entries[name], want)
		}
	}
}

// TestZipSurvivesAFirstImageFailure is the regression test for silently empty
// archives: the writer once waited for image 1 before writing anything, so one
// dead image at the head meant every later image was buffered and then thrown
// away. Signed Reddit URLs expire, so a failing first image was routine.
func TestZipSurvivesAFirstImageFailure(t *testing.T) {
	f := mediaFetcher("img00")
	urls := galleryURLs(4)
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, urls, "My Post"))

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
		if want := fmt.Sprintf("media_%03d.png", i+1); name != want {
			t.Errorf("entry %d = %q, want %q (numbering must have no gaps)", i, name, want)
		}
	}
}

func TestZipSurvivesAMiddleAndLastFailure(t *testing.T) {
	f := mediaFetcher("img01", "img03")
	urls := galleryURLs(5)
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, urls, "My Post"))

	entries := zipEntries(t, rec.Body.Bytes())
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %v", len(entries), entries)
	}
	for _, name := range []string{"media_001.png", "media_002.png", "media_003.png"} {
		if _, ok := entries[name]; !ok {
			t.Errorf("missing %s: %v", name, entries)
		}
	}
	// Surviving images keep their content, not their original position.
	if entries["media_002.png"] != "BYTES:/img02.png" {
		t.Errorf("media_002.png = %q, want img02", entries["media_002.png"])
	}
}

func TestZipSurvivesEveryDownloadFailing(t *testing.T) {
	urls := galleryURLs(3)
	f := mediaFetcher("img00", "img01", "img02")
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, urls, "My Post"))

	// An empty archive is still a valid, openable ZIP.
	entries := zipEntries(t, rec.Body.Bytes())
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0: %v", len(entries), entries)
	}
}

func TestDownloadZipRejectsNonRedditURLs(t *testing.T) {
	// The regression test for the open-proxy hole: /download-zip took
	// media_urls straight from the form and fetched whatever it was given,
	// including loopback and link-local addresses.
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("fetcher dialled %s; the request should have been rejected up front", r.URL)
		return testResponse(r, http.StatusOK, "SECRET-INTERNAL-RESPONSE"), nil
	}))

	for _, hostile := range hostileURLs {
		rec := httptest.NewRecorder()
		// A valid URL first, so the rejection cannot be blamed on an empty list.
		handleDownloadZip(f)(rec, postForm(t, []string{"https://i.redd.it/ok.png", hostile}, "t"))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("hostile URL %q: status = %d, want 400", hostile, rec.Code)
		}
		// A 400 is only possible if validation happens before the first ZIP
		// byte, so the response must not advertise or contain an archive.
		if ct := rec.Header().Get("Content-Type"); ct == "application/zip" {
			t.Errorf("hostile URL %q: rejected request still advertised %s", hostile, ct)
		}
		if strings.Contains(rec.Body.String(), "SECRET-INTERNAL-RESPONSE") {
			t.Errorf("hostile URL %q leaked internal content", hostile)
		}
	}
}

func TestSingleMediaIsServedDirectly(t *testing.T) {
	f := mediaFetcher()
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, []string{"https://i.redd.it/solo.jpg"}, "t"))

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

func TestSingleMediaNamesTheServedRendition(t *testing.T) {
	// Reddit serves variants through the original's path: .../clip.gif with
	// format=mp4 is an mp4, and format=png8 is a png. The download must not be
	// named .gif in either case.
	tests := []struct{ url, name, contentType string }{
		{"https://preview.redd.it/0n5exiers7yh1.gif?width=1080&format=mp4&s=abc", "0n5exiers7yh1.mp4", "video/mp4"},
		{"https://preview.redd.it/0n5exiers7yh1.gif?format=png8&s=abc", "0n5exiers7yh1.png", "image/png"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		handleDownloadZip(mediaFetcher())(rec, postForm(t, []string{tt.url}, "t"))

		if ct := rec.Header().Get("Content-Type"); ct != tt.contentType {
			t.Errorf("%s: content-type = %q, want %q", tt.url, ct, tt.contentType)
		}
		cd := rec.Header().Get("Content-Disposition")
		if !strings.Contains(cd, tt.name) || strings.Contains(cd, ".gif") {
			t.Errorf("%s: content-disposition = %q, want %q and no .gif", tt.url, cd, tt.name)
		}
	}
}

// TestDownloadZipRejectsTooManyFiles pins the over-cap behaviour: a request over
// the limit is refused outright rather than answered with a ZIP that silently
// contains fewer files than the user selected.
func TestDownloadZipRejectsTooManyFiles(t *testing.T) {
	f := mediaFetcher()
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, galleryURLs(maxURLs+20), "t"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "application/zip" {
		t.Errorf("rejected request still advertised %s", ct)
	}
}

func TestDownloadZipRequiresMedia(t *testing.T) {
	f := mediaFetcher()
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, nil, "t"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestDownloadZipRedirectsNonPost(t *testing.T) {
	f := mediaFetcher()
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, httptest.NewRequest(http.MethodGet, "/download-zip", nil))
	if rec.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", rec.Code)
	}
}

func TestDownloadZipContentDisposition(t *testing.T) {
	f := mediaFetcher()
	rec := httptest.NewRecorder()
	handleDownloadZip(f)(rec, postForm(t, galleryURLs(2), "My Great Post! (2024)"))
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "My_Great_Post_2024.zip") {
		t.Errorf("content-disposition = %q", cd)
	}
}

func TestIsFetchableMediaURL(t *testing.T) {
	for _, raw := range []string{
		"https://i.redd.it/a.jpg",
		"https://preview.redd.it/a.jpg?width=640&auto=webp",
		"https://v.redd.it/a/DASH_720.mp4",
		"https://external-preview.redd.it/a.png",
	} {
		if !isFetchableMediaURL(raw) {
			t.Errorf("isFetchableMediaURL(%q) = false, want true", raw)
		}
	}
	for _, raw := range hostileURLs {
		if isFetchableMediaURL(raw) {
			t.Errorf("isFetchableMediaURL(%q) = true, want false", raw)
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
	return template.Must(loadTemplates())
}

// postIndex submits a URL to the index handler the way the form does.
func postIndex(t *testing.T, handler http.Handler, rawURL string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"url": {rawURL}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestIndexRendersGallery(t *testing.T) {
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testResponse(r, http.StatusOK,
			`{"data":[{"title":"Six snaps","is_gallery":true,"gallery_data":{"items":[{"media_id":"a"}]},"media_metadata":{"a":{"s":{"u":"https://preview.redd.it/aaaabbbbcccc.jpg?width=108"}}}}]}`), nil
	}))
	handler := routes(testTemplates(t), f)

	rec := postIndex(t, handler, "https://www.reddit.com/r/pics/comments/1wu034e/x/")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Six snaps", "https://i.redd.it/aaaabbbbcccc.jpg", "Loaded 1 file!"} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing %q", want)
		}
	}
}

func TestIndexRendersVideoVariantAsVideo(t *testing.T) {
	// Reddit serves an animated gallery's mp4 through the original's .gif path
	// with format=mp4. The page must treat it as a video, not an <img>.
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return testResponse(r, http.StatusOK,
			`{"data":[{"title":"Clip","is_gallery":true,"gallery_data":{"items":[{"media_id":"a"}]},"media_metadata":{"a":{"s":{"gif":"https://preview.redd.it/clip.gif?format=png8&s=x","mp4":"https://preview.redd.it/clip.gif?format=mp4&s=x"}}}}]}`), nil
	}))
	handler := routes(testTemplates(t), f)

	rec := postIndex(t, handler, "https://www.reddit.com/r/pics/comments/1wu034e/x/")

	body := rec.Body.String()
	if !strings.Contains(body, `<video src="https://preview.redd.it/clip.gif?format=mp4`) {
		t.Errorf("mp4 variant was not rendered as a video:\n%s", body)
	}
	if strings.Contains(body, `<div class="badge">GIF</div>`) {
		t.Errorf("mp4 variant was labelled a GIF:\n%s", body)
	}
}

func TestIndexRejectsBadURL(t *testing.T) {
	f := testFetcher(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request for a bad URL: %s", r.URL)
		return nil, nil
	}))
	handler := routes(testTemplates(t), f)

	rec := postIndex(t, handler, "https://example.com/not/reddit")

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

	rec := postIndex(t, handler, "https://www.reddit.com/r/pics/comments/1wu034e/x/")

	body := rec.Body.String()
	if strings.Contains(body, "Post not found") {
		t.Errorf("a blocked fetch was reported as a deleted post:\n%s", body)
	}
	if !strings.Contains(body, "Couldn&#39;t reach any source") {
		t.Errorf("expected the unavailable alert, got:\n%s", body)
	}
}

func TestIndexUnknownPathIsNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	routes(testTemplates(t), mediaFetcher()).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/somewhere-else", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestAlertForError(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{ErrInvalidURL, "valid Reddit link"},
		{fmt.Errorf("wrapped: %w", ErrPostNotFound), "Post not found"},
		{ErrNoMedia, "no downloadable media"},
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
	req := postForm(t, galleryURLs(6), "t").WithContext(ctx)

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

// TestZipHandlesMoreURLsThanWorkers is a deadlock guard: more URLs than
// workers must still complete, whatever the order results arrive in.
func TestZipHandlesMoreURLsThanWorkers(t *testing.T) {
	f := mediaFetcher("img00", "img01")
	urls := galleryURLs(maxParallelDownloads * 6)

	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		handleDownloadZip(f)(rec, postForm(t, urls, "t"))
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

// TestZipCapsConcurrentStreams checks the worker pool bounds concurrent
// fetches, and that downloads actually run in parallel while it is saturated.
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
	handleDownloadZip(f)(rec, postForm(t, galleryURLs(maxURLs), "t"))

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
