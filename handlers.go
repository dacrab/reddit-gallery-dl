package main

import (
	"archive/zip"
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

// Alert represents a UI notification
type Alert struct {
	Message string
	Type    string // danger, warning, success, info
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

	url := r.FormValue("url")
	
	render := func(data TemplateData) {
		s.tmpl.ExecuteTemplate(w, "index.html", data)
	}

	gallery, err := s.reddit.FetchGallery(url)
	if err != nil {
		// Distinguish between client errors and server errors for better UX
		msg := err.Error()
		alertType := "danger"
		
		if strings.Contains(msg, "invalid reddit url") {
			msg = "The link provided does not look like a valid Reddit URL."
			alertType = "warning"
		} else if strings.Contains(msg, "no post data") {
			msg = "Could not find any content at that URL. It might be deleted or private."
			alertType = "warning"
		}

		render(TemplateData{
			URL: url,
			Alert: &Alert{Message: msg, Type: alertType},
		})
		return
	}

	if len(gallery.Images) == 0 {
		render(TemplateData{
			URL: url,
			Alert: &Alert{
				Message: "Found the post, but it contains no accessible images.", 
				Type: "warning",
			},
		})
		return
	}

	render(TemplateData{
		Title:  gallery.Title,
		Images: gallery.Images,
		URL:    url,
		Alert: &Alert{
			Message: fmt.Sprintf("Successfully loaded %d images!", len(gallery.Images)), 
			Type: "success",
		},
	})
}

func (s *Server) handleDownloadSingle(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		http.Error(w, "Missing URL", http.StatusBadRequest)
		return
	}

	body, _, err := s.reddit.StreamImage(rawURL)
	if err != nil {
		http.Error(w, fmt.Sprintf("Download failed: %v", err), http.StatusBadGateway)
		return
	}
	defer body.Close()

	// Parse URL to get a clean filename
	u, err := url.Parse(rawURL)
	filename := "image.jpg"
	if err == nil {
		filename = path.Base(u.Path)
	}
	if filename == "" || filename == "." || filename == "/" {
		filename = "image.jpg"
	}

	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	io.Copy(w, body)
}

func (s *Server) handleDownloadZip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", 303)
		return
	}

	r.ParseForm()
	urls := r.Form["image_urls"]
	if len(urls) == 0 {
		// Since this is a form post, a simple text error is fine, 
		// but ideally we'd redirect with flash message. 
		// For now, keeping it simple as this mostly happens if JS is disabled/bypassed.
		http.Error(w, "No images selected for download.", 400)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.zip\"", cleanFilename(r.FormValue("page_title"))))

	z := zip.NewWriter(w)
	defer z.Close()

	for i, u := range urls {
		body, ext, err := s.reddit.StreamImage(u)
		if err != nil {
			log.Printf("Failed to stream image %s: %v", u, err)
			// We continue to next image instead of breaking the whole zip
			continue
		}
		
		f, err := z.Create(fmt.Sprintf("image_%03d%s", i+1, ext))
		if err != nil {
			log.Printf("Failed to create zip entry: %v", err)
			body.Close()
			continue
		}

		io.Copy(f, body)
		body.Close()
	}
}

func cleanFilename(s string) string {
	if s == "" { return "reddit_gallery" }
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) { return r }
		if unicode.IsSpace(r) { return '_' }
		return -1
	}, s)
}