package main

import (
	"archive/zip"
	"bytes"
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
		s.tmpl.ExecuteTemplate(w, "index.html", nil)
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

	s.serveImage(w, r, rawURL, format)
}

func (s *Server) serveImage(w http.ResponseWriter, r *http.Request, rawURL, format string) {
	data, ext, err := s.downloadAndConvert(r.Context(), rawURL, format)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	u, _ := url.Parse(rawURL)
	filename := "image" + ext
	if u != nil {
		base := path.Base(u.Path)
		if strings.Contains(base, ".") {
			filename = strings.TrimSuffix(base, path.Ext(base)) + ext
		}
	}

	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Write(data)
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

	// If only one image is selected, don't zip it
	if len(urls) == 1 {
		s.serveImage(w, r, urls[0], format)
		return
	}

	// Sanitize title
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

		data, ext, err := s.downloadAndConvert(r.Context(), u, format)
		if err != nil {
			log.Printf("Skipping %s: %v", u, err)
			continue
		}

		f, err := z.Create(fmt.Sprintf("image_%03d%s", i+1, ext))
		if err != nil {
			log.Printf("Zip create error: %v", err)
			continue
		}

		if _, err := f.Write(data); err != nil {
			log.Printf("Zip write error: %v", err)
		}
	}
}

func (s *Server) downloadAndConvert(ctx context.Context, urlStr, format string) ([]byte, string, error) {
	body, ext, err := s.reddit.StreamImage(ctx, urlStr)
	if err != nil {
		return nil, "", fmt.Errorf("download failed: %w", err)
	}
	defer body.Close()

	if format != "" && format != "original" {
		return convertImage(body, format)
	}
	data, err := io.ReadAll(body)
	return data, ext, err
}

func convertImage(input io.Reader, format string) ([]byte, string, error) {
	img, _, err := image.Decode(input)
	if err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}

	var buf bytes.Buffer
	var ext string

	switch format {
	case "jpg", "jpeg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
		ext = ".jpg"
	case "png":
		err = png.Encode(&buf, img)
		ext = ".png"
	case "gif":
		err = gif.Encode(&buf, img, nil)
		ext = ".gif"
	default:
		return nil, "", fmt.Errorf("unsupported format: %s", format)
	}

	if err != nil {
		return nil, "", fmt.Errorf("encode: %w", err)
	}
	return buf.Bytes(), ext, nil
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
