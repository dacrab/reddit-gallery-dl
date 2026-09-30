# Reddit Gallery DL

A web tool to browse and download Reddit galleries — images, GIFs and videos — as a ZIP file or individually.

## Features

- **No Reddit account or API keys needed**
- **Gallery support** — multi-image posts, GIFs, and Reddit-hosted videos
- **ZIP download** — pick what you want and download it in one click
- **Built-in fallbacks** — keeps working even when Reddit blocks the server
- **Zero external dependencies** — pure Go standard library
- **Dark/light mode** and a mobile-friendly layout

## Quick Start

```bash
go run .
# visit http://localhost:5000
```

Or with Docker:

```bash
docker build -t reddit-gallery-dl .
docker run -p 5000:5000 reddit-gallery-dl
```

## Deployment

Deployed on [Render](https://render.com) via the `Dockerfile`. Set the `PORT` environment variable if needed (defaults to 5000).

## Architecture

| File | Purpose |
|---|---|
| `main.go` | HTTP server setup and lifecycle |
| `postref.go` | Parsing and validating the many shapes a Reddit link arrives in |
| `sources.go` | The source fallback ladder, HTTP clients, embed page parsing |
| `media.go` | Turning a post's JSON into a list of original-resolution media URLs |
| `handlers.go` | Page rendering and error messages |
| `zip.go` | Download endpoint, ZIP streaming, filename handling |
| `templates/` | Server-rendered page and browser behavior |
| `static/` | Served browser assets |
| `testdata/` | Captured real Reddit responses, used as regression fixtures |
| `*_test.go` | Tests |

### Where posts come from

Reddit's public JSON endpoints are undocumented and actively defended, so no
single source is dependable. `sources.go` keeps an ordered ladder and tries each
one in turn:

1. **arctic-shift** — a public archive returning Reddit's own post JSON.
2. **embed.reddit.com** — the embed page, scraped.
3. **pullpush.io** — a second public archive.
4. **Reddit's own `.json`** — kept last; it currently answers anonymous requests
   from datacentre IPs with `403`, and `old.reddit.com` answers with a redirect
   to a login page.

Two rules keep this honest, and both exist because breaking them made the tool
report live posts as deleted:

- **A block is never proof of deletion.** Only a source that actually read the
  post may report it missing, and "deleted" is only reported when every source
  agrees. A rate limit, a `403` and a login wall are all inconclusive.
- **A source that reads structured data may declare a post media-less; a scraper
  may not.** If the embed page yields no images that might be a text post, a
  missing post, a block page or a redesign, so the ladder keeps going.

### Security

`/download-zip` fetches URLs on behalf of whoever posts to it, which is an open
proxy unless constrained. Three independent layers stop it: `isRedditMediaHost`
gates the initial request, redirect hops are re-checked on every client, and
`redditMediaURLs` filters candidate media before the browser ever sees it. Only
`*.redd.it` is fetchable, so loopback, link-local and arbitrary external hosts
are all unreachable.


## License

MIT
