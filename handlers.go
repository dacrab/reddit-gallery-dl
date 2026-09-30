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
	mux.Handle("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("/", handleIndex(tmpl, f))
	mux.HandleFunc("/download-zip", handleDownloadZip(f))
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
			Title:  gallery.Title,
			Images: gallery.Images,
			URL:    rawURL,
			Alert: &alert{
				Message: fmt.Sprintf("Loaded %d images!", len(gallery.Images)),
				Type:    "success",
			},
		})
	}
}

func alertForError(err error) *alert {
	// Order matters. ErrUnavailable wraps the joined per-source errors, and
	// those often include ErrPostNotFound from an archive that simply has not
	// indexed the post. Checking "deleted" first would tell a user their post
	// is gone when the truth is that we were blocked, which is the same mistake
	// this tool used to make on every request.
	switch {
	case errors.Is(err, ErrInvalidURL):
		return &alert{Message: "That doesn't look like a valid Reddit link.", Type: "warning"}
	case errors.Is(err, ErrUnavailable):
		return &alert{
			Message: "Couldn't reach any source for this post. Reddit may be rate limiting this server — try again in a minute.",
			Type:    "danger",
		}
	case errors.Is(err, ErrNoMedia):
		return &alert{Message: "This post exists but has no images.", Type: "info"}
	case errors.Is(err, ErrPostNotFound):
		return &alert{Message: "Post not found. It might be deleted, private, or too new.", Type: "warning"}
	default:
		return &alert{Message: "Something went wrong. Please try again.", Type: "danger"}
	}
}
