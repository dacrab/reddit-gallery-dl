package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"path"
	"slices"
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

// download is the outcome for one selected image: either an open body to write,
// or the error that ended it. Failures travel down the same channel as successes
// because the writer needs to know about them to stop waiting for an index that
// is never going to arrive.
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
		urls := r.Form["image_urls"]
		if len(urls) == 0 {
			http.Error(w, "No images selected", http.StatusBadRequest)
			return
		}
		if len(urls) > maxURLs {
			urls = urls[:maxURLs]
		}
		// Validate before a single byte of the archive is written: once the ZIP
		// header is out, the status code is already committed. This is also the
		// only thing standing between this endpoint and being an open proxy.
		for _, raw := range urls {
			if !isFetchableImageURL(raw) {
				http.Error(w, "Refusing to download a non-Reddit URL", http.StatusBadRequest)
				return
			}
		}
		if len(urls) == 1 {
			serveSingleImage(r.Context(), f, w, urls[0])
			return
		}
		streamZip(r.Context(), f, w, cleanFilename(r.FormValue("page_title")), urls)
	}
}

// isFetchableImageURL reports whether the download endpoint will fetch raw for
// the user. It re-checks the same host rule the fetcher enforces, so that a
// hostile form post is rejected outright rather than silently skipped.
func isFetchableImageURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && isRedditMediaHost(u.Host)
}

// streamZip downloads the selected images concurrently and writes them into a
// ZIP as they finish, preserving the order the user selected them in.
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

	// Buffered to the full width so no worker can block on a slow consumer,
	// which keeps cancellation from deadlocking the handler.
	results := make(chan download, len(urls))

	// A fixed pool, not a goroutine per URL. A goroutine per URL combined with a
	// process-wide concurrency cap deadlocks: the writers that hold slots are
	// waiting for the lowest index, and the goroutine that owns the lowest index
	// is waiting for a slot. A pool keeps the lowest unfinished index always in
	// flight, so the writer is never blocked on something that cannot arrive.
	// It also bounds how many bodies are open at once, and therefore memory.
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
				body, ext, err := f.streamImage(ctx, rawURL)
				if err != nil {
					if !errors.Is(err, context.Canceled) && !isClientDisconnect(err) {
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
		name := fmt.Sprintf("image_%03d%s", written, d.ext)
		err := func() error {
			f, err := zw.Create(name)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, d.body)
			return err
		}()
		if cerr := d.body.Close(); cerr != nil && !isClientDisconnect(cerr) {
			log.Printf("image close error: %v", cerr)
		}
		if err != nil && !isClientDisconnect(err) {
			log.Printf("zip write error (%s): %v", d.url, err)
		}
	}

	// Hold finished-but-not-yet-writable downloads until their turn comes, so
	// the archive is numbered in selection order.
	pending := make(map[int]download)
	failed := make(map[int]bool)
	next := 0
	drain := func() {
		for {
			if failed[next] {
				delete(failed, next)
				next++
				continue
			}
			d, ok := pending[next]
			if !ok {
				return
			}
			delete(pending, next)
			write(d)
			next++
		}
	}

	for d := range results {
		if d.err != nil {
			failed[d.index] = true
		} else {
			pending[d.index] = d
		}
		drain()
	}

	// Every index reports exactly one outcome, so the loop above walks the whole
	// sequence and nothing is left here. This is a safety net: silently dropping
	// images is the failure this function exists to prevent, so anything that
	// does reach it gets written and logged rather than discarded.
	for _, index := range slices.Sorted(maps.Keys(pending)) {
		log.Printf("zip: image %d was never written in order, appending out of sequence", index)
		write(pending[index])
	}
}

func serveSingleImage(ctx context.Context, f *Fetcher, w http.ResponseWriter, rawURL string) {
	body, ext, err := f.streamImage(ctx, rawURL)
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
		if base := path.Base(u.Path); base != "." && base != "/" && base != "" {
			filename = base
		}
	}
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Content-Type", mime.TypeByExtension(ext))
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
	if s == "" {
		return "reddit_gallery"
	}
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
