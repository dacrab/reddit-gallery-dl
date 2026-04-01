package main

import (
	"archive/zip"
	"bufio"
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
	"sync"
	"syscall"
	"time"
	"unicode"
)

type Server struct {
	reddit *RedditClient
	tmpl   *template.Template
}

func NewServer(tmpl *template.Template) *Server {
	return &Server{reddit: NewRedditClient(), tmpl: tmpl}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	static := http.StripPrefix("/static/", http.FileServer(http.Dir("./static")))
	mux.Handle("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		static.ServeHTTP(w, r)
	}))
	csrf := http.NewCrossOriginProtection()
	mux.HandleFunc("/", withGzip(s.handleIndex))
	mux.Handle("/download-zip", csrf.Handler(http.HandlerFunc(s.handleDownloadZip)))
	return mux
}

// gzipWriter wraps ResponseWriter to compress output transparently.
type gzipWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g gzipWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

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

func (s *Server) render(w http.ResponseWriter, data TemplateData) {
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil && !isClientDisconnect(err) {
		slog.Error("Template render failed", "error", err)
	}
}

// isClientDisconnect reports whether err is a normal client-side disconnect.
// String matching is needed because net stack errors don't always unwrap to syscall.
func isClientDisconnect(err error) bool {
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "broken pipe") || strings.Contains(s, "connection reset by peer")
}

type Alert struct {
	Message string
	Type    string // success | info | warning | danger
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
		s.render(w, TemplateData{URL: urlStr, Alert: alertForError(err)})
		return
	}
	s.render(w, TemplateData{
		Title:  gallery.Title,
		Images: gallery.Images,
		URL:    urlStr,
		Alert:  &Alert{fmt.Sprintf("Loaded %d images!", len(gallery.Images)), "success"},
	})
}

func alertForError(err error) *Alert {
	switch {
	case errors.Is(err, ErrInvalidURL):
		return &Alert{"That doesn't look like a valid Reddit link.", "warning"}
	case errors.Is(err, ErrPostNotFound):
		return &Alert{"Post not found. It might be deleted or private.", "warning"}
	case errors.Is(err, ErrNoImages):
		return &Alert{"This post exists but has no images.", "info"}
	case errors.Is(err, ErrRateLimited):
		return &Alert{"Reddit is rate limiting requests right now. Please wait a moment and try again.", "warning"}
	default:
		return &Alert{err.Error(), "danger"}
	}
}

type prefetched struct {
	ext  string
	data []byte
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
	ctx := r.Context()

	// Cap concurrency to avoid CDN rate limiting.
	const maxConcurrent = 5
	results := make([]prefetched, len(urls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)
	for i, u := range urls {
		wg.Add(1)
		go func(idx int, imgURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			body, ext, err := s.reddit.StreamImage(ctx, imgURL)
			if err != nil {
				slog.Warn("Skipping image", "url", imgURL, "error", err)
				return
			}
			defer body.Close()
			data, err := io.ReadAll(body)
			if err != nil {
				slog.Warn("Skipping image (read error)", "url", imgURL, "error", err)
				return
			}
			results[idx] = prefetched{ext: ext, data: data}
		}(i, u)
	}
	wg.Wait()

	hasAny := false
	for _, p := range results {
		if p.data != nil {
			hasAny = true
			break
		}
	}
	if !hasAny {
		http.Error(w, "No images could be downloaded", http.StatusBadGateway)
		return
	}

	// Clear the write deadline — 60s is too tight for large galleries on slow connections.
	http.NewResponseController(w).SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": title + ".zip"}))

	bw := bufio.NewWriterSize(w, 256*1024) // 256 KiB buffer reduces syscall overhead
	defer bw.Flush()
	z := zip.NewWriter(bw)
	defer z.Close()

	for i, p := range results {
		if p.data == nil {
			continue
		}
		f, err := z.Create(fmt.Sprintf("image_%03d%s", i+1, p.ext))
		if err != nil {
			slog.Error("Zip create error", "error", err)
			continue
		}
		if _, err := f.Write(p.data); err != nil && !isClientDisconnect(err) {
			slog.Error("Zip write error", "error", err)
		}
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

// cleanFilename sanitises s for use as a filename: letters, digits, hyphens and
// underscores are kept; spaces become underscores; everything else is stripped.
func cleanFilename(s string) string {
	if s == "" {
		return "reddit_gallery"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			return r
		case unicode.IsSpace(r):
			return '_'
		default:
			return -1
		}
	}, s)
}
