package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"html/template"
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
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("./static"))))
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/download-zip", s.handleDownloadZip)
	mux.HandleFunc("/download-single", s.handleDownloadSingle)
	return mux
}

// render executes the named template, logging any error instead of silently dropping it.
func (s *Server) render(w http.ResponseWriter, data TemplateData) {
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		slog.Error("Template render failed", "error", err)
	}
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
	s.serveSingleImage(w, r.Context(), rawURL, r.URL.Query().Get("format"))
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
	format := r.FormValue("format")
	if len(urls) == 0 {
		http.Error(w, "No images selected", http.StatusBadRequest)
		return
	}

	if len(urls) == 1 {
		s.serveSingleImage(w, r.Context(), urls[0], format)
		return
	}

	title := cleanFilename(r.FormValue("page_title"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.zip\"", title))

	z := zip.NewWriter(w)
	defer z.Close()

	for i, u := range urls {
		if r.Context().Err() != nil {
			slog.Info("Client disconnected, stopping zip stream")
			return
		}

		body, ext, err := s.reddit.StreamImage(r.Context(), u)
		if err != nil {
			slog.Warn("Skipping image", "url", u, "error", err)
			continue
		}

		f, err := z.Create(fmt.Sprintf("image_%03d%s", i+1, resolvedExt(ext, format)))
		if err != nil {
			body.Close()
			slog.Error("Zip create error", "error", err)
			continue
		}

		if err := streamImage(body, format, f); err != nil {
			slog.Error("Zip write error", "url", u, "error", err)
		}
		body.Close()
	}
}

func (s *Server) serveSingleImage(w http.ResponseWriter, ctx context.Context, rawURL, format string) {
	body, ext, err := s.reddit.StreamImage(ctx, rawURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer body.Close()

	finalExt := resolvedExt(ext, format)

	filename := "image" + finalExt
	if u, _ := url.Parse(rawURL); u != nil {
		if base := path.Base(u.Path); strings.Contains(base, ".") {
			filename = strings.TrimSuffix(base, path.Ext(base)) + finalExt
		}
	}

	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Content-Type", mime.TypeByExtension(finalExt))

	if err := streamImage(body, format, w); err != nil {
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
