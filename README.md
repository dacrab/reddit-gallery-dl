# Reddit Gallery DL

A web tool to browse and download Reddit galleries — images, GIFs and videos — as a ZIP file or individually.

## Features

- **Gallery support** — multi-image posts, GIFs, Reddit-hosted videos (`v.redd.it`)
- **ZIP download** — stream selected media directly to a ZIP, no server buffering
- **Smart rate limiting** — PRAW-style proactive throttling respects Reddit's API headers
- **Zero external dependencies** — pure Go standard library
- **Dark/light mode** — persisted via localStorage
- **Mobile friendly** — responsive grid, works on any screen size

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
| `main.go` | HTTP server setup and timeouts |
| `reddit.go` | Reddit API client, rate limiter, media extraction |
| `handlers.go` | HTTP handlers, gzip compression, ZIP streaming |
| `image.go` | File extension detection |

## License

MIT
