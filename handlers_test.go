package main

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testTemplate() *template.Template {
	return template.Must(template.New("").Funcs(template.FuncMap{"urlExt": urlExt}).Parse(
		`{{define "index.html"}}{{if .Alert}}{{.Alert.Type}}:{{.Alert.Message}}{{end}}{{range .Images}}{{.}}{{end}}{{end}}`))
}

func TestHandleIndex_GET(t *testing.T) {
	mux := routes(testTemplate())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 {
		t.Errorf("GET / = %d", w.Code)
	}
}

func TestHandleIndex_NotFound(t *testing.T) {
	mux := routes(testTemplate())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/nonexistent", nil))
	if w.Code != 404 {
		t.Errorf("GET /nonexistent = %d, want 404", w.Code)
	}
}

func TestHandleIndex_POST_InvalidURL(t *testing.T) {
	mux := routes(testTemplate())
	form := url.Values{"url": {"not-a-reddit-url"}}
	req := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "warning") {
		t.Errorf("expected warning alert, got %q", w.Body.String())
	}
}

func TestHandleDownloadZip_GET_Redirects(t *testing.T) {
	mux := routes(testTemplate())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/download-zip", nil))
	if w.Code != http.StatusSeeOther {
		t.Errorf("GET /download-zip = %d, want 303", w.Code)
	}
}

func TestHandleDownloadZip_NoImages(t *testing.T) {
	mux := routes(testTemplate())
	form := url.Values{}
	req := httptest.NewRequest("POST", "/download-zip", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleDownloadZip_SingleImage(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("fake-png-data"))
	}))
	defer imgSrv.Close()

	origClient := httpClient
	httpClient = imgSrv.Client()
	defer func() { httpClient = origClient }()

	mux := routes(testTemplate())
	form := url.Values{"image_urls": {imgSrv.URL + "/test.png"}}
	req := httptest.NewRequest("POST", "/download-zip", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Error("missing Content-Disposition attachment header")
	}
	if w.Body.String() != "fake-png-data" {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestHandleDownloadZip_MultipleImages(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("img"))
	}))
	defer imgSrv.Close()

	origClient := httpClient
	httpClient = imgSrv.Client()
	defer func() { httpClient = origClient }()

	mux := routes(testTemplate())
	form := url.Values{
		"image_urls": {imgSrv.URL + "/a.jpg", imgSrv.URL + "/b.jpg"},
		"page_title": {"Test Gallery"},
	}
	req := httptest.NewRequest("POST", "/download-zip", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", ct)
	}
	if w.Body.Len() == 0 {
		t.Error("empty zip body")
	}
}

func TestCleanFilename(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "reddit_gallery"},
		{"Hello World!", "Hello_World"},
		{"a/b\\c<d>e", "abcde"},
		{"normal-name_123", "normal-name_123"},
	}
	for _, tt := range tests {
		if got := cleanFilename(tt.in); got != tt.want {
			t.Errorf("cleanFilename(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAlertForError(t *testing.T) {
	tests := []struct {
		err      error
		wantType string
	}{
		{ErrInvalidURL, "warning"},
		{ErrPostNotFound, "warning"},
		{ErrNoImages, "info"},
		{ErrRateLimited, "warning"},
	}
	for _, tt := range tests {
		a := alertForError(tt.err)
		if a.Type != tt.wantType {
			t.Errorf("alertForError(%v).Type = %q, want %q", tt.err, a.Type, tt.wantType)
		}
	}
}
