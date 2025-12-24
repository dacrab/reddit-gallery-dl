package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"html/template"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"unicode"

	_ "golang.org/x/image/webp"
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
		s.tmpl.ExecuteTemplate(w, "index.html", TemplateData{})
		return
	}

	urlStr := r.FormValue("url")
	gallery, err := s.reddit.FetchGallery(r.Context(), urlStr)
	if err != nil {
		alert := &Alert{Message: err.Error(), Type: "danger"}
		if errors.Is(err, ErrInvalidURL) {
			alert = &Alert{Message: "That doesn't look like a valid Reddit link.", Type: "warning"}
		} else if errors.Is(err, ErrPostNotFound) {
			alert = &Alert{Message: "Post not found. It might be deleted or private.", Type: "warning"}
		} else if errors.Is(err, ErrNoImages) {
			alert = &Alert{Message: "This post exists but has no images.", Type: "info"}
		}
		s.tmpl.ExecuteTemplate(w, "index.html", TemplateData{URL: urlStr, Alert: alert})
		return
	}

	s.tmpl.ExecuteTemplate(w, "index.html", TemplateData{
		Title:  gallery.Title,
		Images: gallery.Images,
		URL:    urlStr,
		Alert:  &Alert{Message: fmt.Sprintf("Loaded %d images!", len(gallery.Images)), Type: "success"},
	})
}

func (s *Server) handleDownloadSingle(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	format := r.URL.Query().Get("format")
	if rawURL == "" {
		http.Error(w, "Missing URL", http.StatusBadRequest)
		return
	}

	s.serveSingleImage(w, r.Context(), rawURL, format)
}

func (s *Server) serveSingleImage(w http.ResponseWriter, ctx context.Context, rawURL, format string) {
	body, ext, err := s.reddit.StreamImage(ctx, rawURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer body.Close()

	finalExt := ext
	if format != "" && format != "original" {
		finalExt = "." + format
		if format == "jpeg" {
			finalExt = ".jpg"
		}
	}

	filename := "image" + finalExt
	if u, _ := url.Parse(rawURL); u != nil {
		base := path.Base(u.Path)
		if strings.Contains(base, ".") {
			filename = strings.TrimSuffix(base, path.Ext(base)) + finalExt
		}
	}

	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	w.Header().Set("Content-Type", mime.TypeByExtension(finalExt))

	if err := s.streamImage(body, format, w); err != nil {
		log.Printf("Error streaming single image: %v", err)
	}
}

func (s *Server) handleDownloadZip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	r.ParseForm()
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
			log.Println("Client disconnected, stopping zip stream.")
			return
		}

		body, ext, err := s.reddit.StreamImage(r.Context(), u)
		if err != nil {
			log.Printf("Skipping %s: %v", u, err)
			continue
		}

		finalExt := ext
		if format != "" && format != "original" {
			finalExt = "." + format
			if format == "jpeg" {
				finalExt = ".jpg"
			}
		}

		f, err := z.Create(fmt.Sprintf("image_%03d%s", i+1, finalExt))
		if err != nil {
			body.Close()
			log.Printf("Zip create error: %v", err)
			continue
		}

		if err := s.streamImage(body, format, f); err != nil {
			log.Printf("Zip write error for %s: %v", u, err)
		}
		body.Close()
	}
}

// streamImage streams the image from src to dst, converting it on-the-fly if needed.
func (s *Server) streamImage(src io.Reader, format string, dst io.Writer) error {
	if format == "" || format == "original" {
		_, err := io.Copy(dst, src)
		return err
	}

	img, _, err := image.Decode(src)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	switch format {
	case "jpg", "jpeg":
		return jpeg.Encode(dst, img, &jpeg.Options{Quality: 90})
	case "png":
		return png.Encode(dst, img)
	case "gif":
		return gif.Encode(dst, img, nil)
	}
	return fmt.Errorf("unsupported format: %s", format)
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
