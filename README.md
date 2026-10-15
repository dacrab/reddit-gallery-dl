# Reddit Gallery DL

A web tool for browsing and downloading Reddit posts — images, GIFs, and videos
— as a ZIP file or one at a time.

## Features

- No Reddit account or API keys needed
- Multi-image galleries, GIFs, and Reddit-hosted videos
- Download everything as one ZIP, or pick only what you want
- Keeps working when Reddit blocks a lookup source
- No external dependencies — pure Go standard library
- Dark/light theme and a mobile-friendly page

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

Deployed on [Render](https://render.com) from the `Dockerfile`. The server
listens on the `PORT` environment variable and defaults to 5000.

Note: on Render's free plan the service sleeps while idle, so the first visit
after a quiet period takes a moment to wake it up.

## How it works

Paste a Reddit post link and the server looks the post up and lists its media.
If one lookup source is blocked, it tries another. ZIP downloads stream the
files straight from Reddit's CDN — nothing is re-hosted, and only Reddit's own
media hosts are ever fetched.

## Development

```bash
go test ./...
```

## License

MIT
