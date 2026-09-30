package main

import (
	"errors"
	"net/url"
	"strings"
)

// ErrInvalidURL is returned for input that is not a link to a Reddit post.
var ErrInvalidURL = errors.New("invalid reddit url")

// isRedditHost reports whether host belongs to Reddit's web or media domains.
//
// This is the security boundary for every user-supplied URL, so it is
// deliberately fail-closed: it matches whole labels only, which rejects
// "evil-reddit.com", "i.redd.it.evil.com" and trailing-dot bypasses, and it
// rejects IP literals, so a bare address can never satisfy it.
func isRedditHost(host string) bool {
	host = strings.ToLower(host)
	return host == "reddit.com" || host == "redd.it" ||
		strings.HasSuffix(host, ".reddit.com") ||
		strings.HasSuffix(host, ".redd.it")
}

// isRedditMediaHost reports whether host serves Reddit-hosted media, and is
// therefore the only thing the download endpoint may fetch on a user's behalf.
func isRedditMediaHost(host string) bool {
	return strings.HasSuffix(strings.ToLower(host), ".redd.it")
}

// postRef identifies a post.
//
// subreddit is carried alongside the ID because Reddit's embed endpoint needs it
// in the request path. Links that do not name a subreddit, such as redd.it
// shortlinks, leave it empty; sources that require it report that and let the
// ladder move on.
type postRef struct {
	postID    string
	subreddit string
}

// link is a parsed Reddit URL: either a post reference, or a share link whose
// post ID is not in the URL at all.
type link struct {
	ref   postRef
	share string
}

// parseLink normalises the many shapes a Reddit link arrives in — pasted
// without a scheme, shared from the mobile app as /gallery/<id>, shortened to
// redd.it/<id>, or carrying a share code — into a single post identity.
//
// It performs no I/O, so every accepted shape is covered by table tests.
func parseLink(input string) (link, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return link{}, ErrInvalidURL
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || !isRedditHost(u.Host) {
		return link{}, ErrInvalidURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return link{}, ErrInvalidURL
	}

	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(segments) >= 4 && segments[0] == "r" && segments[2] == "comments":
		// /r/<sub>/comments/<id>[/<slug>]
		if !isPostID(segments[3]) {
			return link{}, ErrInvalidURL
		}
		return link{ref: postRef{postID: segments[3], subreddit: subredditOf(segments[1])}}, nil

	case len(segments) == 4 && segments[0] == "r" && segments[2] == "s":
		// /r/<sub>/s/<code>: the post ID is only discoverable by following a
		// redirect, so hand the path to the fetcher intact.
		if !isPostID(segments[3]) {
			return link{}, ErrInvalidURL
		}
		return link{share: "/" + strings.Join(segments, "/")}, nil

	case len(segments) == 2 && segments[0] == "gallery":
		// /gallery/<id>, which is what the mobile app puts on the clipboard.
		if !isPostID(segments[1]) {
			return link{}, ErrInvalidURL
		}
		return link{ref: postRef{postID: segments[1]}}, nil

	case len(segments) == 1 && strings.EqualFold(u.Host, "redd.it"):
		// redd.it/<id> shortlink.
		if !isPostID(segments[0]) {
			return link{}, ErrInvalidURL
		}
		return link{ref: postRef{postID: segments[0]}}, nil
	}
	return link{}, ErrInvalidURL
}

// subredditOf returns s if it could name a subreddit, and "" otherwise. The
// check keeps user input from reaching a request path unvalidated, and treats
// "all", the catch-all subreddit, as unusable.
func subredditOf(s string) string {
	if s == "all" || len(s) > 32 {
		return ""
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return ""
		}
	}
	return s
}

// isPostID reports whether s has the shape of a Reddit base-36 post ID. It is a
// sanity check rather than a lookup: it keeps junk and traversal attempts out of
// the request paths built from user input.
func isPostID(s string) bool {
	if len(s) < 4 || len(s) > 20 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
