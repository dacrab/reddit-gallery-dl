# Reddit Gallery DL

Paste a Reddit link, get the post's photos and videos — one at a time or all in a ZIP.

**Live demo:** https://reddit-gallery-dl.onrender.com

It works with multi-image galleries, GIFs, and Reddit-hosted video, and needs no Reddit login or API key. If one way of looking up a post is blocked, it quietly tries another.

## It handles

- Galleries with several images, plus GIFs and hosted video
- Individual downloads or a single ZIP for the whole post
- MP4 versions of animated posts when Reddit has one
- Lookup fallbacks when Reddit rate-limits a source
- Light and dark themes, and a layout that fits a phone

## Run it

With Go 1.27 or newer:

```bash
go run .
```

Open <http://localhost:5000>. Or use Docker:

```bash
docker build -t reddit-gallery-dl .
docker run -p 5000:5000 reddit-gallery-dl
```

Set `PORT` if 5000 is taken.

## Details

- Media streams from Reddit's CDN; the server stores nothing.
- Media is only ever fetched from Reddit's own hosts.
- The free Render demo naps when idle, so a first visit may take a few seconds.

MIT licensed — see [LICENSE](LICENSE).
