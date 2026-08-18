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

const maxURLs = 50

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
		if err := r.ParseForm(); err != nil {
			render(w, templateData{Alert: &alert{"Form data too large or malformed.", "warning"}})
			return
		}
		urlStr := r.FormValue("url")
		gallery, err := fetchGallery(r.Context(), urlStr)
		if err != nil {
			log.Printf("fetch gallery %q: %v", urlStr, err)
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
		serveSingleImage(r.Context(), w, urls[0])
		return
	}

	title := cleanFilename(r.FormValue("page_title"))
	ctx := r.Context()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": title + ".zip"}))

	zw := zip.NewWriter(w)
	defer func() {
		if err := zw.Close(); err != nil && !isClientDisconnect(err) {
			log.Printf("zip close error: %v", err)
		}
	}()

	type item struct {
		ext  string
		body io.ReadCloser
	}

	type result struct {
		idx int
		item
	}

	results := make(chan result, len(urls))
	var wg sync.WaitGroup

	go func() {
		for i, u := range urls {
			wg.Add(1)
			go func(idx int, imgURL string) {
				defer wg.Done()
				select {
				case dlSem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-dlSem }()
				body, ext, err := streamImage(ctx, imgURL)
				if err != nil {
					log.Printf("skip %s: %v", imgURL, err)
					return
				}
				results <- result{idx: idx, item: item{ext: ext, body: body}}
			}(i, u)
		}
		wg.Wait()
		close(results)
	}()

	buf := make(map[int]item)
	next := 0
	for res := range results {
		buf[res.idx] = res.item
		if ctx.Err() != nil {
			continue
		}
		for {
			cur, ok := buf[next]
			if !ok {
				break
			}
			delete(buf, next)
			f, err := zw.Create(fmt.Sprintf("image_%03d%s", next+1, cur.ext))
			if err == nil {
				_, err = io.Copy(f, cur.body)
			}
			if cerr := cur.body.Close(); cerr != nil && !isClientDisconnect(cerr) {
				log.Printf("image close error: %v", cerr)
			}
			if err != nil && !isClientDisconnect(err) {
				log.Printf("zip write error: %v", err)
			}
			next++
		}
	}
	for _, cur := range buf {
		_ = cur.body.Close()
	}
}

func serveSingleImage(ctx context.Context, w http.ResponseWriter, rawURL string) {
	body, ext, err := streamImage(ctx, rawURL)
	if err != nil {
		http.Error(w, "Failed to fetch image", http.StatusBadGateway)
		return
	}
	defer func() {
		if err := body.Close(); err != nil && !isClientDisconnect(err) {
			log.Printf("image close error: %v", err)
		}
	}()

	filename := "image" + ext
	if u, err := url.Parse(rawURL); err == nil {
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
