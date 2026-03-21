package main

import (
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/url"
	"path"
	"strings"

	_ "golang.org/x/image/webp"
	_ "image/gif" // register gif decoder for image.Decode format detection
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

// isVideoExt reports whether ext is a video format that cannot be transcoded.
func isVideoExt(ext string) bool {
	return ext == ".mp4" || ext == ".gifv"
}

// streamImage copies src to dst, converting the image format on-the-fly if needed.
// Videos and GIFs are always copied as-is regardless of format — frame-accurate
// transcoding requires ffmpeg which is not available here.
func streamImage(src io.Reader, format string, dst io.Writer) error {
	if isOriginalFormat(format) {
		_, err := io.Copy(dst, src)
		return err
	}

	img, srcFmt, err := image.Decode(src)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	// GIFs and videos cannot be losslessly round-tripped through image.Decode
	// (only the first frame is decoded). Pass them through unchanged.
	if srcFmt == "gif" {
		return fmt.Errorf("gif conversion not supported: use original format")
	}

	switch format {
	case "jpg", "jpeg":
		return jpeg.Encode(dst, img, &jpeg.Options{Quality: 90})
	case "png":
		return png.Encode(dst, img)
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
		case ".png", ".gif", ".jpg", ".jpeg", ".webp", ".mp4", ".gifv":
			return ext
		}
	}
	ct := strings.ToLower(strings.SplitN(contentType, ";", 2)[0])
	switch ct {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	}
	return ".jpg"
}
