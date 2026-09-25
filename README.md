# Reddit Gallery DL

A web tool to browse and download Reddit galleries — images, GIFs and videos — as a ZIP file or individually.

## Features

- **Gallery support** — multi-image posts, GIFs, Reddit-hosted videos (`v.redd.it`)
- **ZIP download** — stream selected media directly to a ZIP, no server buffering
- **Rate-limit handling** — retries transient Reddit rate limits using the server-provided delay
- **Zero external dependencies** — pure Go standard library
- **Reddit access fallback** — uses a public post archive when Reddit blocks the JSON endpoint
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
| `main.go` | HTTP server setup, lifecycle, and graceful shutdown |
| `reddit.go` | Reddit/archive clients, media extraction, and URL utilities |
| `handlers.go` | HTTP handlers, ZIP streaming, and error mapping |
| `templates/` | Server-rendered page and browser behavior |
| `static/` | Served browser assets |
| `*_test.go` | HTTP, extraction, and utility regression tests |

## License

MIT
