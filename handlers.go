package main

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
)

func routes(tmpl *template.Template, f *Fetcher) *http.ServeMux {
	mux := http.NewServeMux()
	files := http.StripPrefix("/static/", http.FileServer(http.Dir("./static")))
	mux.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", handleIndex(tmpl, f))
	mux.HandleFunc("/download-zip", handleDownloadZip(f))
	return mux
}

type templateData struct {
	Title string
	Media []string
	URL   string
	Alert *alert
}

type alert struct {
	Message string
	Type    string
}

func handleIndex(tmpl *template.Template, f *Fetcher) http.HandlerFunc {
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
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if err := r.ParseForm(); err != nil {
			render(w, templateData{Alert: &alert{
				Message: "Form data too large or malformed.", Type: "warning"}})
			return
		}
		rawURL := r.FormValue("url")
		gallery, err := f.Gallery(r.Context(), rawURL)
		if err != nil {
			log.Printf("fetch gallery %q: %v", rawURL, err)
			render(w, templateData{URL: rawURL, Alert: alertForError(err)})
			return
		}
		render(w, templateData{
			Title: gallery.Title,
			Media: gallery.Media,
			URL:   rawURL,
			Alert: &alert{
				Message: loadedMessage(len(gallery.Media)),
				Type:    "success",
			},
		})
	}
}

func loadedMessage(n int) string {
	if n == 1 {
		return "Loaded 1 file!"
	}
	return fmt.Sprintf("Loaded %d files!", n)
}

func alertForError(err error) *alert {
	// Order matters: ErrUnavailable wraps the per-source errors, which may
	// include ErrPostNotFound from an archive that has not indexed the post.
	// Checking "deleted" first would blame a blocked fetch on the user's post.
	switch {
	case errors.Is(err, ErrInvalidURL):
		return &alert{Message: "That doesn't look like a valid Reddit link.", Type: "warning"}
	case errors.Is(err, ErrUnavailable):
		return &alert{
			Message: "Couldn't reach any source for this post. Reddit may be rate limiting this server — try again in a minute.",
			Type:    "danger",
		}
	case errors.Is(err, ErrNoMedia):
		return &alert{Message: "This post exists but has no downloadable media.", Type: "info"}
	case errors.Is(err, ErrPostNotFound):
		return &alert{Message: "Post not found. It might be deleted, private, or too new.", Type: "warning"}
	default:
		return &alert{Message: "Something went wrong. Please try again.", Type: "danger"}
	}
}
