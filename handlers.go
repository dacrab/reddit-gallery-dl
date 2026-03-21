package main

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"
)

type Server struct {
	reddit *RedditClient
	tmpl   *template.Template
}

func NewServer(tmpl *template.Template) *Server {
	return &Server{
		reddit: NewRedditClient(),
		tmpl:   tmpl,
	}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	static := http.StripPrefix("/static/", http.FileServer(http.Dir("./static")))
	mux.Handle("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		static.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/", withGzip(s.handleIndex))
	mux.HandleFunc("/download-zip", s.handleDownloadZip)
	mux.HandleFunc("/download-single", s.handleDownloadSingle)
	return mux
}

type gzipWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g gzipWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

// withGzip compresses HTML responses when the client supports it.
func withGzip(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h(w, r)
			return
		}
		gz, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		defer gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		h(gzipWriter{w, gz}, r)
	}
}

// render executes the named template, logging any error instead of silently dropping it.
// Broken pipe / connection reset errors are ignored — they just mean the client left.
func (s *Server) render(w http.ResponseWriter, data TemplateData) {
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		if isClientDisconnect(err) {
			return
		}
		slog.Error("Template render failed", "error", err)
	}
}

// isClientDisconnect reports whether err is a broken pipe or connection reset,
// which happens when the client closes the connection before we finish writing.
func isClientDisconnect(err error) bool {
	s := err.Error()
	return strings.Contains(s, "broken pipe") || strings.Contains(s, "connection reset by peer")
}

type Alert struct {
	Message string
	Type    string
}

type TemplateData struct {
	Title  string
	Images []string
	URL    string
	Alert  *Alert
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		s.render(w, TemplateData{})
		return
	}

	urlStr := r.FormValue("url")
	gallery, err := s.reddit.FetchGallery(r.Context(), urlStr)
	if err != nil {
		alert := &Alert{Message: err.Error(), Type: "danger"}
		switch {
		case errors.Is(err, ErrInvalidURL):
			alert = &Alert{Message: "That doesn't look like a valid Reddit link.", Type: "warning"}
		case errors.Is(err, ErrPostNotFound):
			alert = &Alert{Message: "Post not found. It might be deleted or private.", Type: "warning"}
		case errors.Is(err, ErrNoImages):
			alert = &Alert{Message: "This post exists but has no images.", Type: "info"}
		case errors.Is(err, ErrRateLimited):
			alert = &Alert{Message: "Reddit is rate limiting requests right now. Please wait a moment and try again.", Type: "warning"}
		}
		s.render(w, TemplateData{URL: urlStr, Alert: alert})
		return
	}

	s.render(w, TemplateData{
		Title:  gallery.Title,
		Images: gallery.Images,
		URL:    urlStr,
		Alert:  &Alert{Message: fmt.Sprintf("Loaded %d images!", len(gallery.Images)), Type: "success"},
	})
}

func (s *Server) handleDownloadSingle(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		http.Error(w, "Missing URL", http.StatusBadRequest)
		return
	}
	s.serveSingleImage(w, r.Context(), rawURL)
}

func (s *Server) handleDownloadZip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	urls := r.Form["image_urls"]
	if len(urls) == 0 {
		http.Error(w, "No images selected", http.StatusBadRequest)
		return
	}

	if len(urls) == 1 {
		s.serveSingleImage(w, r.Context(), urls[0])
		return
	}

	title := cleanFilename(r.FormValue("page_title"))

	// Fetch the first image before writing headers — lets us return a real
	// error if everything fails instead of sending a corrupt empty zip.
	var firstBody io.ReadCloser
	var firstExt string
	firstIdx := 0
	for firstIdx < len(urls) {
		body, ext, err := s.reddit.StreamImage(r.Context(), urls[firstIdx])
		if err != nil {
			slog.Warn("Skipping image", "url", urls[firstIdx], "error", err)
			firstIdx++
			continue
		}
		firstBody, firstExt = body, ext
		firstIdx++
		break
	}
	if firstBody == nil {
		http.Error(w, "No images could be downloaded", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": title + ".zip"}))

	z := zip.NewWriter(w)
	defer z.Close()

	writeEntry := func(idx int, body io.ReadCloser, ext string) {
		defer body.Close()
		f, err := z.Create(fmt.Sprintf("image_%03d%s", idx+1, ext))
		if err != nil {
			slog.Error("Zip create error", "error", err)
			return
		}
		if _, err := io.Copy(f, body); err != nil && !isClientDisconnect(err) {
			slog.Error("Zip write error", "url", urls[idx], "error", err)
		}
	}

	writeEntry(firstIdx-1, firstBody, firstExt)

	for i := firstIdx; i < len(urls); i++ {
		if r.Context().Err() != nil {
			slog.Info("Client disconnected, stopping zip stream")
			return
		}
		body, ext, err := s.reddit.StreamImage(r.Context(), urls[i])
		if err != nil {
			slog.Warn("Skipping image", "url", urls[i], "error", err)
			continue
		}
		writeEntry(i, body, ext)
	}
}


func (s *Server) serveSingleImage(w http.ResponseWriter, ctx context.Context, rawURL string) {
	body, ext, err := s.reddit.StreamImage(ctx, rawURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer body.Close()

	filename := "image" + ext
	if u, _ := url.Parse(rawURL); u != nil {
		if base := path.Base(u.Path); strings.Contains(base, ".") {
			filename = strings.TrimSuffix(base, path.Ext(base)) + ext
		}
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Content-Type", mime.TypeByExtension(ext))
	if _, err := io.Copy(w, body); err != nil && !isClientDisconnect(err) {
		slog.Error("Error streaming single image", "error", err)
	}
}

func cleanFilename(s string) string {
	if s == "" {
		return "reddit_gallery"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			return r
		case unicode.IsSpace(r):
			return '_'
		default:
			return -1
		}
	}, s)
}
