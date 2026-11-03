# Reddit Gallery DL

Browse and download the images, GIFs, and videos from any Reddit post.

**Live demo:** https://reddit-gallery-dl.onrender.com

Paste a Reddit link and the page lists every photo and video in the post. Download them one by one, or grab the whole set as a ZIP. No Reddit account or API key needed.

## Features

- Multi-image galleries, GIFs, and Reddit-hosted videos
- Download a single file or the whole post as one ZIP
- Animated posts come as MP4 when Reddit offers one
- Keeps trying other lookup sources when one is blocked
- Dark and light themes with a mobile-friendly layout
- Runs as a single small Go binary with no dependencies

## Usage

You'll need [Go](https://go.dev) 1.27 or newer.

```bash
go run .
```

Open <http://localhost:5000> and paste a Reddit post link.

With Docker:

```bash
docker build -t reddit-gallery-dl .
docker run -p 5000:5000 reddit-gallery-dl
```

## Good to know

- Files stream straight from Reddit's CDN — nothing is stored on the server.
- Only Reddit's own media hosts are fetched.
- The free demo sleeps when idle, so the first visit can take a moment.

## Built with

Go's standard library and a small HTML/CSS front end, packaged in one Docker image.

## License

MIT
