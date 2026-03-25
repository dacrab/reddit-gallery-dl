package main

import (
	"net/url"
	"path"
	"strings"
)

// detectExtension infers a file extension from the URL path or Content-Type header.
// URL path is preferred since mime.ExtensionsByType sorts alphabetically and returns
// unreliable results (e.g. .jfif instead of .jpg for image/jpeg).
func detectExtension(urlStr, contentType string) string {
	if u, err := url.Parse(urlStr); err == nil {
		ext := strings.ToLower(path.Ext(u.Path))
		switch ext {
		case ".png", ".gif", ".jpg", ".jpeg", ".webp", ".mp4":
			return ext
		}
	}
	ct, _, _ := strings.Cut(contentType, ";")
	ct = strings.ToLower(strings.TrimSpace(ct))
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
