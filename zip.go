package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unicode"
)

const (
	maxFormBytes         = 1 << 20
	maxURLs              = 50
	maxParallelDownloads = 8
)

// download is one file's outcome: an open body to write, or the error that
// ended it. Failures share the channel so the writer knows about every index.
type download struct {
	index int
	url   string
	ext   string
	body  io.ReadCloser
	err   error
}

func handleDownloadZip(f *Fetcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid form data", http.StatusBadRequest)
			return
		}
		urls := r.Form["media_urls"]
		if len(urls) == 0 {
			http.Error(w, "No files selected", http.StatusBadRequest)
			return
		}
		if len(urls) > maxURLs {
			http.Error(w, fmt.Sprintf("Too many files selected (limit %d)", maxURLs), http.StatusBadRequest)
			return
		}
		// Validate before a single byte of the archive is written: once the ZIP
		// header is out, the status code is already committed. This is also the
		// only thing standing between this endpoint and being an open proxy.
		for _, raw := range urls {
			if !isFetchableMediaURL(raw) {
				http.Error(w, "Refusing to download a non-Reddit URL", http.StatusBadRequest)
				return
			}
		}
		if len(urls) == 1 {
			serveSingleMedia(r.Context(), f, w, urls[0])
			return
		}
		streamZip(r.Context(), f, w, cleanFilename(r.FormValue("page_title")), urls)
	}
}

// isFetchableMediaURL reports whether the download endpoint will fetch raw on
// the user's behalf, re-checking the same host rule the fetcher enforces.
func isFetchableMediaURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && isRedditMediaHost(u.Host)
}

// streamZip downloads the selected media concurrently and writes it into a
// ZIP as it finishes, preserving the order the user selected it in.
func streamZip(ctx context.Context, f *Fetcher, w http.ResponseWriter, title string, urls []string) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": title + ".zip"}))

	zw := zip.NewWriter(w)
	defer func() {
		if err := zw.Close(); err != nil && !isClientDisconnect(err) {
			log.Printf("zip close error: %v", err)
		}
	}()

	// Buffered to the full width so no worker can block on a slow consumer.
	results := make(chan download, len(urls))

	// A fixed pool, not a goroutine per URL: it bounds how many bodies are
	// open at once, and the writer re-orders outcomes below.
	workers := min(maxParallelDownloads, len(urls))
	var nextIndex int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				index := int(atomic.AddInt64(&nextIndex, 1)) - 1
				if index >= len(urls) {
					return
				}
				rawURL := urls[index]
				body, ext, err := f.streamMedia(ctx, rawURL)
				if err != nil {
					if !isClientDisconnect(err) {
						log.Printf("skip %s: %v", rawURL, err)
					}
					results <- download{index: index, url: rawURL, err: err}
					continue
				}
				results <- download{index: index, url: rawURL, ext: ext, body: body}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var written int
	write := func(d download) {
		written++
		name := fmt.Sprintf("media_%03d%s", written, d.ext)
		err := func() error {
			f, err := zw.Create(name)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, d.body)
			return err
		}()
		if cerr := d.body.Close(); cerr != nil && !isClientDisconnect(cerr) {
			log.Printf("media close error: %v", cerr)
		}
		if err != nil && !isClientDisconnect(err) {
			log.Printf("zip write error (%s): %v", d.url, err)
		}
	}

	// Outcomes may arrive in any order. Hold early ones until their turn comes
	// and skip failed indexes, so the written archive is numbered in selection
	// order with no gaps.
	pending := make(map[int]download)
	next := 0
	for d := range results {
		pending[d.index] = d
		for {
			p, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			if p.err == nil {
				write(p)
			}
		}
	}
}

// serveSingleMedia streams a single file straight through, named after the
// URL rather than zipped.
func serveSingleMedia(ctx context.Context, f *Fetcher, w http.ResponseWriter, rawURL string) {
	body, ext, err := f.streamMedia(ctx, rawURL)
	if err != nil {
		http.Error(w, "Failed to fetch media", http.StatusBadGateway)
		return
	}
	defer func() {
		if err := body.Close(); err != nil && !isClientDisconnect(err) {
			log.Printf("media close error: %v", err)
		}
	}()

	filename := "image" + ext
	if u, err := url.Parse(rawURL); err == nil {
		base := path.Base(u.Path)
		// The URL path names the original even when Reddit serves a different
		// rendition: .../clip.gif?format=mp4 is an mp4, so replace its extension.
		if stem := strings.TrimSuffix(base, path.Ext(base)); stem != "" && stem != "." && stem != "/" {
			filename = stem + ext
		}
	}
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Content-Type", mediaContentTypes[ext])
	if _, err := io.Copy(w, body); err != nil && !isClientDisconnect(err) {
		log.Printf("stream error: %v", err)
	}
}

func isClientDisconnect(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, context.Canceled)
}

func cleanFilename(s string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			return r
		case unicode.IsSpace(r):
			return '_'
		default:
			return -1
		}
	}, s)
	if cleaned == "" {
		return "reddit_gallery"
	}
	return cleaned
}
