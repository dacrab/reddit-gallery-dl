package main

import (
	"archive/zip"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
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
		s.tmpl.ExecuteTemplate(w, "index.html", nil)
		return
	}

	urlStr := r.FormValue("url")

	// Use Context for cancellation
	gallery, err := s.reddit.FetchGallery(r.Context(), urlStr)
	if err != nil {
		alertMsg := err.Error()
		alertType := "danger"

		// Senior Error Handling: Type assertions / Sentinel checks
		if errors.Is(err, ErrInvalidURL) {
			alertMsg = "That doesn't look like a valid Reddit link."
			alertType = "warning"
		} else if errors.Is(err, ErrPostNotFound) {
			alertMsg = "Post not found. It might be deleted or private."
			alertType = "warning"
		} else if errors.Is(err, ErrNoImages) {
			alertMsg = "This post exists but has no images."
			alertType = "info"
		}

		s.tmpl.ExecuteTemplate(w, "index.html", TemplateData{
			URL:   urlStr,
			Alert: &Alert{Message: alertMsg, Type: alertType},
		})
		return
	}

	s.tmpl.ExecuteTemplate(w, "index.html", TemplateData{
		Title:  gallery.Title,
		Images: gallery.Images,
		URL:    urlStr,
		Alert: &Alert{
			Message: fmt.Sprintf("Loaded %d images!", len(gallery.Images)),
			Type:    "success",
		},
	})
}

func (s *Server) handleDownloadSingle(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		http.Error(w, "Missing URL", http.StatusBadRequest)
		return
	}

	body, _, err := s.reddit.StreamImage(r.Context(), rawURL)
	if err != nil {
		http.Error(w, "Image download failed", http.StatusBadGateway)
		return
	}
	defer body.Close()

	u, _ := url.Parse(rawURL)
	filename := "image.jpg"
	if u != nil {
		filename = path.Base(u.Path)
	}
	if filename == "" || filename == "/" {
		filename = "image.jpg"
	}

	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	io.Copy(w, body)
}

func (s *Server) handleDownloadZip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	r.ParseForm()
	urls := r.Form["image_urls"]
	if len(urls) == 0 {
		http.Error(w, "No images selected", http.StatusBadRequest)
		return
	}

	// Sanitize title
	title := cleanFilename(r.FormValue("page_title"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.zip\"", title))

	z := zip.NewWriter(w)
	defer z.Close()

	// Process serially to minimize memory usage (Low RAM env compatible)
	for i, u := range urls {
		// Context check: stop if user disconnects
		if r.Context().Err() != nil {
			log.Println("Client disconnected, stopping zip stream.")
			return
		}

		body, ext, err := s.reddit.StreamImage(r.Context(), u)
		if err != nil {
			log.Printf("Skipping %s: %v", u, err)
			continue
		}

		f, err := z.Create(fmt.Sprintf("image_%03d%s", i+1, ext))
		if err != nil {
			body.Close()
			log.Printf("Zip create error: %v", err)
			continue
		}

		// Copy buffer (32KB chunks default)
		if _, err := io.Copy(f, body); err != nil {
			log.Printf("Stream error: %v", err)
		}
		body.Close()
	}
}

func cleanFilename(s string) string {
	if s == "" {
		return "reddit_gallery"
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		if unicode.IsSpace(r) {
			return '_'
		}
		return -1
	}, s)
}
