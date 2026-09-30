package main

import (
	"html"
	"net/url"
	"path"
	"slices"
	"strings"
)

// redditPost is the subset of Reddit's post JSON this tool needs. Every source
// in the fallback ladder is normalised into this one shape before extraction,
// so media extraction has a single implementation to keep correct instead of
// one per source.
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

type gallery struct {
	Title  string
	Images []string
}

// extractImages pulls downloadable media out of a post.
//
// The candidate groups are ordered by how faithful they are, not by how often
// they are hit: a gallery's media_metadata carries the original upload, while
// preview.images carries a re-encoded, downscaled copy. Trying preview first
// silently handed users a smaller file than the one they asked for, so the
// preview is now the last resort and only for posts with nothing better.
func extractImages(post redditPost) []string {
	groups := [][]string{
		galleryImages(post),
		videoImages(post),
		directImages(post),
		previewImages(post),
	}
	for _, candidates := range groups {
		if urls := redditMediaURLs(candidates); len(urls) > 0 {
			return urls
		}
	}
	return nil
}

// galleryImages reads the authoritative source for multi-image posts: the
// ordered item list plus the metadata for each item.
func galleryImages(post redditPost) []string {
	if !post.IsGallery || post.GalleryData == nil {
		return nil
	}
	var urls []string
	for _, item := range post.GalleryData.Items {
		meta, ok := post.MediaMetadata[item.MediaID]
		if !ok {
			continue
		}
		urls = append(urls, richestVariant(meta.S)...)
	}
	return urls
}

// richestVariant prefers an animated variant over the still frame, so a GIF
// gallery downloads as a GIF rather than as its poster image.
func richestVariant(s mediaSource) []string {
	switch {
	case s.Mp4 != "" && urlExt(s.Mp4) == ".mp4":
		return []string{s.Mp4}
	case s.Gif != "":
		return []string{s.Gif}
	default:
		return []string{s.U}
	}
}

func videoImages(post redditPost) []string {
	if post.IsVideo && post.Media != nil && post.Media.RedditVideo != nil {
		return []string{stripQuery(post.Media.RedditVideo.FallbackURL)}
	}
	return nil
}

// directImages is the post's own link, which for an image post is the original
// upload. It is filtered to Reddit hosts downstream, so a post linking somewhere
// external falls through to its Reddit-hosted preview instead.
func directImages(post redditPost) []string {
	if post.URL == "" {
		return nil
	}
	return []string{post.URL}
}

func previewImages(post redditPost) []string {
	if post.Preview == nil {
		return nil
	}
	if rvp := post.Preview.RedditVideoPreview; rvp != nil && rvp.FallbackURL != "" {
		return []string{stripQuery(rvp.FallbackURL)}
	}
	var urls []string
	for _, img := range post.Preview.Images {
		switch {
		case img.Variants.MP4 != nil:
			urls = append(urls, img.Variants.MP4.Source.URL)
		case img.Variants.GIF != nil:
			urls = append(urls, img.Variants.GIF.Source.URL)
		default:
			urls = append(urls, img.Source.URL)
		}
	}
	return urls
}

// redditMediaURLs normalises a batch of candidate URLs: it unescapes them,
// upgrades resized previews to the original upload, drops anything that is not
// Reddit-hosted media, and deduplicates.
//
// The host filter is the second half of the download endpoint's SSRF defence
// (the first being isRedditMediaHost at fetch time). Because every URL that
// reaches the browser has already been through here, the browser cannot talk
// the server into fetching an address it should not.
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

// originalURL rewrites a preview.redd.it URL to the equivalent i.redd.it URL.
//
// Reddit's metadata and its embed page both point at
// preview.redd.it?width=...&auto=webp, which has the right dimensions but is a
// re-encoded copy. i.redd.it is the untouched upload; on live posts the gap
// between the two reached 13% of file size, and the conversion is lossless
// because both URLs name the same media ID.
//
// Two path shapes have to be handled. Post JSON gives a bare media ID
// ("/4hpo47nlimsh1.jpg"), while the embed page prefixes the post slug
// ("/my-post-title-v0-4hpo47nlimsh1.jpg"). In both the media ID is the final
// hyphen-separated segment.
func originalURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Host, "preview.redd.it") {
		return raw
	}
	id, ext, ok := strings.Cut(strings.TrimPrefix(u.Path, "/"), ".")
	if !ok {
		return raw
	}
	ext = strings.ToLower(ext)
	if !imageExts["."+ext] {
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

func stripQuery(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		u.RawQuery = ""
		u.Fragment = ""
		return u.String()
	}
	return raw
}

func urlExt(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return strings.ToLower(path.Ext(u.Path))
	}
	return strings.ToLower(path.Ext(rawURL))
}

var imageExts = map[string]bool{
	".png": true, ".gif": true, ".gifv": true, ".jpg": true, ".jpeg": true,
	".webp": true, ".avif": true, ".bmp": true, ".mp4": true, ".webm": true,
	".mov": true,
}

// detectExtension picks a filename extension for a downloaded image, trusting
// the URL first and the response's Content-Type second.
func detectExtension(urlStr, contentType string) string {
	if ext := urlExt(urlStr); imageExts[ext] {
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
