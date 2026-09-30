package main

import (
	"testing"
)

func TestIsRedditHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"reddit.com", true},
		{"www.reddit.com", true},
		{"old.reddit.com", true},
		{"api.reddit.com", true},
		{"embed.reddit.com", true},
		{"redd.it", true},
		{"i.redd.it", true},
		{"v.redd.it", true},
		{"preview.redd.it", true},
		// Bypass attempts. Every one of these reaches an attacker-controlled
		// host, so every one has to be rejected.
		{"notreddit.com", false},
		{"evil-reddit.com", false},
		{"reddit.com.evil.com", false},
		{"i.redd.it.evil.com", false},
		{"x.reddit.com", true},
		{"", false},
		{"127.0.0.1", false},
		{"169.254.169.254", false},
		{"[::1]", false},
		// Trailing-dot forms defeat naive suffix checks.
		{"reddit.com.", false},
		{"i.redd.it.", false},
		// Port must not sneak past the host check.
		{"i.redd.it:8080", false},
		{"WWW.REDDIT.COM", true},
	}
	for _, tt := range tests {
		if got := isRedditHost(tt.host); got != tt.want {
			t.Errorf("isRedditHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestIsRedditMediaHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"i.redd.it", true},
		{"preview.redd.it", true},
		{"external-preview.redd.it", true},
		{"v.redd.it", true},
		// The embed page serves the subreddit icon from here. It is not post
		// media, and allowing it would add a stray image to every gallery.
		{"b.thumbs.redditmedia.com", false},
		// Reddit web hosts are not media hosts.
		{"www.reddit.com", false},
		{"embed.reddit.com", false},
		{"i.redd.it.evil.com", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isRedditMediaHost(tt.host); got != tt.want {
			t.Errorf("isRedditMediaHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestParseLink(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		postID    string
		subreddit string
		share     string
		wantErr   bool
	}{
		{name: "canonical", input: "https://www.reddit.com/r/pics/comments/1wu034e/from_recent_trip/", postID: "1wu034e", subreddit: "pics"},
		{name: "no slug", input: "https://www.reddit.com/r/pics/comments/1wu034e", postID: "1wu034e", subreddit: "pics"},
		{name: "trailing slash", input: "https://www.reddit.com/r/pics/comments/1wu034e/title/", postID: "1wu034e", subreddit: "pics"},
		{name: "no scheme", input: "reddit.com/r/pics/comments/1wu034e/x/", postID: "1wu034e", subreddit: "pics"},
		{name: "bare host", input: "www.reddit.com/r/pics/comments/1wu034e/x/", postID: "1wu034e", subreddit: "pics"},
		{name: "surrounding space", input: "  https://www.reddit.com/r/pics/comments/1wu034e/x/  ", postID: "1wu034e", subreddit: "pics"},
		{name: "query string", input: "https://www.reddit.com/r/pics/comments/1wu034e/x/?utm_source=share", postID: "1wu034e", subreddit: "pics"},
		{name: "old.reddit", input: "https://old.reddit.com/r/pics/comments/1wu034e/x/", postID: "1wu034e", subreddit: "pics"},
		{name: "m.reddit", input: "https://m.reddit.com/r/pics/comments/1wu034e/x/", postID: "1wu034e", subreddit: "pics"},
		{name: "uppercase scheme", input: "HTTPS://WWW.REDDIT.COM/r/pics/comments/1wu034e/x", postID: "1wu034e", subreddit: "pics"},
		{name: "app gallery link", input: "https://www.reddit.com/gallery/1wu034e", postID: "1wu034e"},
		{name: "shortlink", input: "https://redd.it/1wu034e", postID: "1wu034e"},
		{name: "share link", input: "https://www.reddit.com/r/pics/s/AbCdEf12", share: "/r/pics/s/AbCdEf12"},
		{name: "share link no scheme", input: "reddit.com/r/pics/s/AbCdEf12", share: "/r/pics/s/AbCdEf12"},
		{name: "share link bad code", input: "https://www.reddit.com/r/pics/s/../x", wantErr: true},

		{name: "empty", input: "", wantErr: true},
		{name: "whitespace", input: "   ", wantErr: true},
		{name: "not a url", input: "not a url at all !!!", wantErr: true},
		{name: "other host", input: "https://example.com/r/pics/comments/1wu034e/x/", wantErr: true},
		{name: "subreddit only", input: "https://www.reddit.com/r/pics/", wantErr: true},
		{name: "root", input: "https://www.reddit.com/", wantErr: true},
		{name: "user profile", input: "https://www.reddit.com/user/someone", wantErr: true},
		{name: "non http scheme", input: "ftp://www.reddit.com/r/pics/comments/1wu034e/x/", wantErr: true},
		{name: "javascript scheme", input: "javascript:alert(1)//www.reddit.com", wantErr: true},
		{name: "credentials in url", input: "https://www.reddit.com@evil.com/r/pics/comments/1wu034e/x/", wantErr: true},
		{name: "host suffix trick", input: "https://notreddit.com/r/pics/comments/1wu034e/x/", wantErr: true},
		{name: "bad post id", input: "https://www.reddit.com/r/pics/comments/../../etc/passwd/x/", wantErr: true},
		{name: "media url is not a post", input: "https://i.redd.it/abc123.jpg", wantErr: true},
		{name: "share code too short", input: "https://redd.it/x", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLink(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseLink(%q) = %+v, want error", tt.input, got)
				}
				if err != ErrInvalidURL {
					t.Errorf("err = %v, want ErrInvalidURL", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLink(%q): %v", tt.input, err)
			}
			if got.ref.postID != tt.postID || got.share != tt.share {
				t.Errorf("parseLink(%q) = %+v, want postID=%q share=%q",
					tt.input, got, tt.postID, tt.share)
			}
			if got.ref.subreddit != tt.subreddit {
				t.Errorf("parseLink(%q) subreddit = %q, want %q",
					tt.input, got.ref.subreddit, tt.subreddit)
			}
		})
	}
}

func TestIsPostID(t *testing.T) {
	for _, s := range []string{"1wu034e", "abcd", "1a2B3c", "aaaaaaaaaaaa"} {
		if !isPostID(s) {
			t.Errorf("isPostID(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "ab", "..", "a/b", "a b", "a?b", "a#b", strings30} {
		if isPostID(s) {
			t.Errorf("isPostID(%q) = true, want false", s)
		}
	}
}

const strings30 = "0123456789012345678901234567890123456789"
