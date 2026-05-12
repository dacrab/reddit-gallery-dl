package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"syscall"
	"unicode"
)

const maxURLs = 100

func routes(tmpl *template.Template) *http.ServeMux {
	mux := http.NewServeMux()
	static := http.StripPrefix("/static/", http.FileServer(http.Dir("./static")))
	mux.Handle("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		static.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/", handleIndex(tmpl))
	mux.HandleFunc("/download-zip", handleDownloadZip)
	return mux
}

type templateData struct {
	Title  string
	Images []string
	URL    string
	Alert  *alert
}

type alert struct {
	Message string
	Type    string
}

func handleIndex(tmpl *template.Template) http.HandlerFunc {
	render := func(w http.ResponseWriter, data templateData) {
		if err := tmpl.ExecuteTemplate(w, "index.html", data); err != nil && !isClientDisconnect(err) {
			log.Printf("template error: %v", err)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			render(w, templateData{})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		urlStr := r.FormValue("url")
		gallery, err := fetchGallery(r.Context(), urlStr)
		if err != nil {
			render(w, templateData{URL: urlStr, Alert: alertForError(err)})
			return
		}
		render(w, templateData{
			Title:  gallery.Title,
			Images: gallery.Images,
			URL:    urlStr,
			Alert:  &alert{fmt.Sprintf("Loaded %d images!", len(gallery.Images)), "success"},
		})
	}
}

func alertForError(err error) *alert {
	switch {
	case errors.Is(err, ErrInvalidURL):
		return &alert{"That doesn't look like a valid Reddit link.", "warning"}
	case errors.Is(err, ErrPostNotFound):
		return &alert{"Post not found. It might be deleted or private.", "warning"}
	case errors.Is(err, ErrNoImages):
		return &alert{"This post exists but has no images.", "info"}
	case errors.Is(err, ErrRateLimited):
		return &alert{"Reddit is rate limiting requests. Please wait a moment and try again.", "warning"}
	default:
		return &alert{"Something went wrong. Please try again.", "danger"}
	}
}

func handleDownloadZip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	urls := r.Form["image_urls"]
	if len(urls) == 0 {
		http.Error(w, "No images selected", http.StatusBadRequest)
		return
	}
	if len(urls) > maxURLs {
		urls = urls[:maxURLs]
	}
	if len(urls) == 1 {
		serveSingleImage(w, r.Context(), urls[0])
		return
	}

	title := cleanFilename(r.FormValue("page_title"))
	ctx := r.Context()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": title + ".zip"}))

	zw := zip.NewWriter(w)
	defer zw.Close()

	// Download and stream directly into ZIP, limited concurrency via sequential writes.
	// We fetch concurrently but write sequentially to avoid ZIP corruption.
	type result struct {
		idx  int
		ext  string
		body io.ReadCloser
	}

	results := make(chan result, 5)
	var wg sync.WaitGroup

	go func() {
		sem := make(chan struct{}, 5)
		for i, u := range urls {
			wg.Add(1)
			go func(idx int, imgURL string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if ctx.Err() != nil {
					return
				}
				body, ext, err := streamImage(ctx, imgURL)
				if err != nil {
					log.Printf("skip %s: %v", imgURL, err)
					return
				}
				results <- result{idx, ext, body}
			}(i, u)
		}
		wg.Wait()
		close(results)
	}()

	for res := range results {
		f, err := zw.Create(fmt.Sprintf("image_%03d%s", res.idx+1, res.ext))
		if err != nil {
			res.body.Close()
			continue
		}
		_, err = io.Copy(f, res.body)
		res.body.Close()
		if err != nil && !isClientDisconnect(err) {
			log.Printf("zip write error: %v", err)
		}
	}
}

func serveSingleImage(w http.ResponseWriter, ctx context.Context, rawURL string) {
	body, ext, err := streamImage(ctx, rawURL)
	if err != nil {
		http.Error(w, "Failed to fetch image", http.StatusBadGateway)
		return
	}
	defer body.Close()

	filename := "image" + ext
	if u, _ := url.Parse(rawURL); u != nil {
		if base := path.Base(u.Path); base != "." && base != "/" {
			filename = base
		}
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Content-Type", mime.TypeByExtension(ext))
	if _, err := io.Copy(w, body); err != nil && !isClientDisconnect(err) {
		log.Printf("stream error: %v", err)
	}
}

func isClientDisconnect(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

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
