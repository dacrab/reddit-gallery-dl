# Reddit Gallery Downloader (Go)

A fast, memory-efficient web tool to download high-resolution Reddit galleries. Written in Go, it supports individual file downloads and on-the-fly ZIP streaming, making it perfect for deployment on memory-constrained platforms like Render, Railway, or Heroku.

## Features

-   **Memory Efficient:** Streams ZIP downloads directly to the client. RAM usage remains constant (~10-20MB) regardless of gallery size.
-   **No API Keys Required:** Uses standard JSON endpoints.
-   **Bulk & Individual Downloads:** Download all images as a ZIP or pick specific ones.
-   **Docker Ready:** Tiny production image (~15MB) based on Alpine Linux.
-   **Smart Validation:** Handles shortened URLs (`redd.it`), NSFW bypass, and redirects automatically.

## Quick Start

### Using Docker (Recommended)

```bash
docker build -t reddit-gallery-dl .
docker run -p 5000:5000 reddit-gallery-dl
```

Open [http://localhost:5000](http://localhost:5000) in your browser.

### Local Development

Requires [Go 1.22+](https://go.dev/dl/).

```bash
# Clone the repository
git clone https://github.com/dacrab/reddit-gallery-dl.git
cd reddit-gallery-dl

# Run directly
go run .
```

## Deployment

### Render / Railway / Fly.io

1.  Fork this repository.
2.  Connect your GitHub account to your hosting provider.
3.  Create a new service.
4.  The provider will automatically detect the `Dockerfile` and build it.

## Architecture

-   **`main.go`**: Application entry point and configuration.
-   **`handlers.go`**: HTTP handlers, template rendering, and streaming logic.
-   **`reddit.go`**: Encapsulated Reddit API client.

## License

MIT
