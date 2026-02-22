package main

import (
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"net/url"
	"path"
	"strings"

	_ "golang.org/x/image/webp"
)

// isOriginalFormat reports whether the requested format means "keep as-is".
func isOriginalFormat(format string) bool {
	return format == "" || format == "original"
}

// resolvedExt returns the file extension to use based on the requested format.
// Falls back to the original extension when format is empty or "original".
func resolvedExt(originalExt, format string) string {
	if isOriginalFormat(format) {
		return originalExt
	}
	if format == "jpeg" {
		return ".jpg"
	}
	return "." + format
}

// streamImage copies src to dst, converting the image format on-the-fly if needed.
func streamImage(src io.Reader, format string, dst io.Writer) error {
	if isOriginalFormat(format) {
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

// detectExtension infers a file extension from the URL path or Content-Type header.
// URL path is preferred since mime.ExtensionsByType sorts alphabetically and returns
// unreliable results (e.g. .jfif instead of .jpg for image/jpeg).
func detectExtension(urlStr, contentType string) string {
	if u, err := url.Parse(urlStr); err == nil {
		ext := strings.ToLower(path.Ext(u.Path))
		switch ext {
		case ".png", ".gif", ".jpg", ".jpeg", ".webp":
			return ext
		}
	}
	if contentType != "" {
		if exts, _ := mime.ExtensionsByType(contentType); len(exts) > 0 {
			return exts[0]
		}
	}
	return ".jpg"
}
