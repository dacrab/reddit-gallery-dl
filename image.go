package main

import (
	"net/url"
	"path"
	"strings"
)

// urlExt returns the lowercased extension from a URL's path, ignoring query strings.
func urlExt(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return strings.ToLower(path.Ext(u.Path))
	}
	return strings.ToLower(path.Ext(rawURL))
}

// detectExtension returns a file extension from the URL path or Content-Type.
// URL path is preferred; mime.ExtensionsByType is unreliable (returns .jfif for image/jpeg).
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
