package main

import (
	"html"
	"net/url"
	"path"
	"slices"
	"strings"
)

// redditPost is the subset of Reddit's post JSON this tool needs: every source
// in the fallback ladder is normalised into this one shape before extraction.
type redditPost struct {
	Title     string `json:"title"`
	IsGallery bool   `json:"is_gallery"`
	IsVideo   bool   `json:"is_video"`
	URL       string `json:"url_overridden_by_dest"`

	GalleryData   *galleryData             `json:"gallery_data"`
	MediaMetadata map[string]mediaMetadata `json:"media_metadata"`
	Media         *redditMedia             `json:"media"`
	Preview       *redditPreview           `json:"preview"`
}

type redditVideo struct {
	FallbackURL string `json:"fallback_url"`
}

type redditMedia struct {
	RedditVideo *redditVideo `json:"reddit_video"`
}

type previewSource struct {
	URL string `json:"url"`
}

type previewVariant struct {
	Source previewSource `json:"source"`
}

type previewImage struct {
	Source   previewSource   `json:"source"`
	Variants previewVariants `json:"variants"`
}

type previewVariants struct {
	GIF *previewVariant `json:"gif"`
	MP4 *previewVariant `json:"mp4"`
}

type redditPreview struct {
	RedditVideoPreview *redditVideo   `json:"reddit_video_preview"`
	Images             []previewImage `json:"images"`
}

type galleryData struct {
	Items []galleryItem `json:"items"`
}

type galleryItem struct {
	MediaID string `json:"media_id"`
}

type mediaSource struct {
	U   string `json:"u"`
	Gif string `json:"gif"`
	Mp4 string `json:"mp4"`
}

type mediaMetadata struct {
	S mediaSource `json:"s"`
}

// gallery is a post's downloadable media: a title and the URLs, in the order
// the post presents them. Entries may be images, GIFs, or videos.
type gallery struct {
	Title string
	Media []string
}

// extractMedia pulls downloadable media out of a post.
//
// The candidate groups are ordered by fidelity: a gallery's media_metadata
// carries the original upload, while preview.images carries a re-encoded,
// downscaled copy, so the preview is the last resort.
func extractMedia(post redditPost) []string {
	groups := [][]string{
		galleryMedia(post),
		videoMedia(post),
		directMedia(post),
		previewMedia(post),
	}
	for _, candidates := range groups {
		if urls := redditMediaURLs(candidates); len(urls) > 0 {
			return urls
		}
	}
	return nil
}

// galleryMedia reads the authoritative source for multi-image posts: the
// ordered item list plus the metadata for each item.
func galleryMedia(post redditPost) []string {
	if !post.IsGallery || post.GalleryData == nil {
		return nil
	}
	var urls []string
	for _, item := range post.GalleryData.Items {
		meta, ok := post.MediaMetadata[item.MediaID]
		if !ok {
			continue
		}
		urls = append(urls, richestVariant(meta.S))
	}
	return urls
}

// richestVariant picks the best downloadable variant of one gallery item.
//
// When Reddit provides an mp4 rendition it is the same animation at a fraction
// of the size: GIF stores each frame as a full image, so originals routinely
// run to tens of megabytes. The mp4 wins when present; the GIF (original or
// still) is the fallback, then the plain URL.
func richestVariant(s mediaSource) string {
	switch {
	case s.Mp4 != "":
		return s.Mp4
	case s.Gif != "":
		return s.Gif
	default:
		return s.U
	}
}

func videoMedia(post redditPost) []string {
	if post.IsVideo && post.Media != nil && post.Media.RedditVideo != nil {
		// Keep the query: Reddit serves GIF posts through preview.redd.it, where
		// format=mp4 and the s= signature are both required.
		return []string{post.Media.RedditVideo.FallbackURL}
	}
	return nil
}

// directMedia is the post's own link, which for an image post is the original
// upload. redditMediaURLs filters it to Reddit hosts, so a post linking
// somewhere external falls through to its Reddit-hosted preview instead.
//
// A direct GIF gives way to the preview's mp4 when there is one: the original
// GIF is the full upload and can be tens of megabytes, while the mp4 is the
// same animation transcoded by Reddit.
func directMedia(post redditPost) []string {
	if post.URL == "" {
		return nil
	}
	if urlExt(post.URL) == ".gif" && previewHasMP4(post) {
		return nil
	}
	return []string{post.URL}
}

func previewMedia(post redditPost) []string {
	if post.Preview == nil {
		return nil
	}
	if rvp := post.Preview.RedditVideoPreview; rvp != nil && rvp.FallbackURL != "" {
		return []string{rvp.FallbackURL}
	}
	var urls []string
	for _, img := range post.Preview.Images {
		src := mediaSource{U: img.Source.URL}
		if img.Variants.GIF != nil {
			src.Gif = img.Variants.GIF.Source.URL
		}
		if img.Variants.MP4 != nil {
			src.Mp4 = img.Variants.MP4.Source.URL
		}
		urls = append(urls, richestVariant(src))
	}
	return urls
}

// previewHasMP4 reports whether Reddit offers an mp4 rendition of the post's
// preview, so a direct GIF can defer to it.
func previewHasMP4(post redditPost) bool {
	if post.Preview == nil {
		return false
	}
	if rvp := post.Preview.RedditVideoPreview; rvp != nil && rvp.FallbackURL != "" {
		return true
	}
	for _, img := range post.Preview.Images {
		if img.Variants.MP4 != nil {
			return true
		}
	}
	return false
}

// redditMediaURLs normalises a batch of candidate URLs: it unescapes them,
// upgrades resized previews to the original upload, drops anything that is not
// Reddit-hosted media, and deduplicates. The host filter is also what keeps a
// post from talking the download endpoint into fetching a foreign URL.
func redditMediaURLs(candidates []string) []string {
	var urls []string
	for _, candidate := range candidates {
		u := originalURL(html.UnescapeString(strings.TrimSpace(candidate)))
		if u == "" {
			continue
		}
		parsed, err := url.Parse(u)
		if err != nil || !isRedditMediaHost(parsed.Host) {
			continue
		}
		if !slices.Contains(urls, u) {
			urls = append(urls, u)
		}
	}
	return urls
}

// originalURL rewrites a preview.redd.it URL to the equivalent i.redd.it URL:
// preview.redd.it serves a re-encoded, downscaled copy while i.redd.it is the
// untouched upload, and both name the same media ID. The ID is the final
// hyphen-separated path segment, with or without the embed page's post slug.
//
// The rewrite is skipped when no i.redd.it original exists: format=png8 names a
// still that lives only under preview.redd.it, and mp4/webm/mov are renditions
// that only preview.redd.it serves. Rewriting either produces a 404, so the
// preview URL (and its signature) is kept instead.
func originalURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Host, "preview.redd.it") {
		return raw
	}
	if strings.EqualFold(u.Query().Get("format"), "png8") {
		return raw
	}
	switch urlExt(raw) {
	case ".mp4", ".webm", ".mov":
		return raw
	}
	id, ext, ok := strings.Cut(strings.TrimPrefix(u.Path, "/"), ".")
	if !ok {
		return raw
	}
	ext = strings.ToLower(ext)
	if !mediaExts["."+ext] {
		return raw
	}
	if i := strings.LastIndexByte(id, '-'); i >= 0 {
		id = id[i+1:]
	}
	if !isMediaID(id) {
		return raw
	}
	return "https://i.redd.it/" + id + "." + ext
}

// isMediaID reports whether s looks like a Reddit media ID, which is the
// lowercase alphanumeric stem shared by a preview URL and its original.
func isMediaID(s string) bool {
	if len(s) < 6 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// urlExt returns the file extension a URL names. Reddit's format parameter
// wins over the path when it names a known media type: preview URLs reuse the
// original's path, so .../abc.gif?format=png8 is a still PNG and
// .../abc.gif?format=mp4 is a video.
func urlExt(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return strings.ToLower(path.Ext(rawURL))
	}
	format := strings.ToLower(u.Query().Get("format"))
	if format == "png8" {
		return ".png"
	}
	if ext := "." + format; mediaExts[ext] {
		return ext
	}
	return strings.ToLower(path.Ext(u.Path))
}

// mediaExts are the extensions a download may be named by. detectExtension
// below maps content types onto this set.
var mediaExts = map[string]bool{
	".png": true, ".gif": true, ".jpg": true, ".jpeg": true, ".webp": true,
	".avif": true, ".mp4": true, ".webm": true, ".mov": true,
}

// detectExtension picks a filename extension for downloaded media, trusting
// the URL first and the response's Content-Type second.
func detectExtension(urlStr, contentType string) string {
	if ext := urlExt(urlStr); mediaExts[ext] {
		return ext
	}
	mediaType, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/avif":
		return ".avif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	}
	return ".jpg"
}
