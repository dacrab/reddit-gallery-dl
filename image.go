package main

import (
	"net/url"
	"path"
	"strings"
)

// detectExtension infers a file extension from a URL or Content-Type header.
// The URL path takes priority; mime.ExtensionsByType is avoided because it
// sorts results alphabetically and returns unreliable values (e.g. ".jfif"
// instead of ".jpg" for image/jpeg).
func detectExtension(urlStr, contentType string) string {
	if u, err := url.Parse(urlStr); err == nil {
		switch ext := strings.ToLower(path.Ext(u.Path)); ext {
		case ".png", ".gif", ".gifv", ".jpg", ".jpeg", ".webp", ".mp4", ".webm", ".mov":
			return ext
		}
	}
	ct, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(ct)) {
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
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	}
	return ".jpg"
}
