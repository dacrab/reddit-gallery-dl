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
| `reddit.go` | Fetching Reddit posts and extracting media |
| `handlers.go` | HTTP handlers and ZIP streaming |
| `templates/` | Server-rendered page and browser behavior |
| `static/` | Served browser assets |
| `*_test.go` | Regression tests |

## License

MIT
