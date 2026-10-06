# couchside-capture

Shows a web page as a live HLS stream, encoding only while someone is
watching. Made for [Couchside](https://github.com/timothydodd/couchside)'s
stream channels, where it turns a WeatherStar 4000 recreation
([ws4kp](https://github.com/netbymatt/ws4kp)) into a 1990s-style weather
channel, but it captures any page.

## How it works

- A headless Chromium opens the page once. Between viewers it's **frozen**
  (its processes are stopped), so it holds its memory but uses no CPU.
- The first request for `/stream.m3u8` wakes it and starts one ffmpeg, which
  encodes the tab's frames to H.264 HLS with your music (or silence) under
  them. The first segment is ready in about three seconds.
- A minute after the last request, ffmpeg stops and the page freezes again. A
  page that slept for more than ten minutes is reloaded on wake, so its data
  is fresh.
- `CAPTURE_MODE=cold` doesn't keep Chromium around at all: nothing runs while
  idle, and the first viewer waits a few seconds longer while it launches.

## Run

```bash
docker run -p 9800:9800 \
  -e CAPTURE_URL='http://ws4kp:8080/?kiosk=true&latLonQuery=Orlando%20International%20Airport%20Orlando%20FL%20USA' \
  ghcr.io/timothydodd/couchside-capture
```

Then play `http://<host>:9800/stream.m3u8`, or add it to Couchside as a
channel: Settings → Live TV → Your channels → New channel → **A stream
address**. `deploy/k3s/weatherstar.yaml` is a ready k3s deployment of ws4kp
plus this.

| Variable | Default | Meaning |
| --- | --- | --- |
| `CAPTURE_URL` | required | The page to show (http or https) |
| `CAPTURE_ADDR` | `:9800` | Listen address |
| `CAPTURE_WIDTH` / `CAPTURE_HEIGHT` | `640` / `480` | Picture size |
| `CAPTURE_FPS` | `10` | Frames per second (1 to 30) |
| `CAPTURE_BITRATE_K` | `1500` | Video bitrate, kbit/s |
| `CAPTURE_MUSIC` | none | Folder of audio files (MP3, M4A, Ogg, FLAC, WAV) looped under the picture, shuffled |
| `CAPTURE_IDLE` | `1m` | Stop encoding this long after the last request |
| `CAPTURE_MODE` | `warm` | `warm` keeps the page loaded and frozen; `cold` launches Chromium per viewer |
| `CAPTURE_RELOAD_AFTER` | `10m` | Reload a page that slept longer than this when it wakes |
| `CAPTURE_START_MAX` | `25s` | How long a playlist request waits for the first segment |
| `CAPTURE_CHROME` | found on PATH | Chromium executable |
| `CAPTURE_FFMPEG` | `ffmpeg` | ffmpeg executable |
| `CAPTURE_DIR` | temp dir | Where segments are written |

`GET /health` answers `{"state": "idle" | "starting" | "streaming"}`.

## Notes

- **Linux only.** Freezing uses SIGSTOP on Chromium's processes.
- **Music is yours to supply.** Nothing is bundled.
- **ws4kp** uses US National Weather Service data, so the forecast is US only.
  Its name, look and data are its own; this project only captures the page.
- Chromium runs with `--no-sandbox`, as containers without user namespaces
  need. Point it only at pages you trust.

## Build

```bash
go build ./cmd/capture
CAPTURE_URL=https://example.com CAPTURE_CHROME=/usr/bin/google-chrome ./capture
```

Tests: `go test ./...`. The release workflow publishes a multi-arch image on a
`v*` tag.
