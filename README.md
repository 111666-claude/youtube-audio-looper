# YouTube Audio Looper

YouTube Audio Looper creates looped MP3 clips from a selected part of a YouTube video.

The project includes:

- REST API for generating looped audio.
- One-page web UI for using the API from a browser.
- Telegram bot for a step-by-step chat flow.
- Docker setup with FFmpeg and yt-dlp.

Live app: https://youtube-audio-looper.onrender.com/

## Features

| Feature | Description |
| --- | --- |
| Web UI | Browser form for creating and downloading looped MP3 files |
| REST API | `POST /api/v1/audio/loop` returns generated MP3 audio |
| Telegram bot | Conversational flow for the same audio generation workflow |
| YouTube download | Uses `yt-dlp` and downloads only the requested video segment, not full playlists |
| Segment extraction | Requests the selected audio range during download |
| Audio looping | Repeats the selected segment to the requested final duration |
| Seamless looping | Equal-power crossfade between the clip's tail and head, repeating the mixed loop unit with no seam |
| Validation | Validates YouTube URL, timestamps, clip length, and duration |
| Isolated jobs | Uses per-request temporary job directories and cleans them up |

## Requirements

- Go 1.26+
- FFmpeg
- yt-dlp
- Docker and Docker Compose, if running containerized

## Quick Start

Use the deployed app:

```text
https://youtube-audio-looper.onrender.com/
```

Run the API and web UI:

```bash
go run ./cmd/api
```

Open the web app:

```text
http://localhost:8084/
```

Health check:

```bash
curl http://localhost:8084/health
```

## Web UI

The web UI is served by the API process:

- `GET /` serves `web/index.html`.
- `GET /web/*` serves static assets.
- The form sends JSON to `POST /api/v1/audio/loop`.
- Successful responses are shown as an audio player with a download link.
- Error responses are shown on the page.

Expected time fields use `MM:SS` or `HH:MM:SS`.

Production URL:

```text
https://youtube-audio-looper.onrender.com/
```

## API

### Create Looped Audio

```http
POST /api/v1/audio/loop
Content-Type: application/json
```

Request body:

```json
{
  "youtube_url": "https://www.youtube.com/watch?v=HDDdKCSn25Y",
  "start": "00:20",
  "end": "00:36",
  "duration": "01:00",
  "seamless": true,
  "crossfade": 0.5,
  "loops": 0
}
```

| Field | Required | Default | Meaning |
| --- | --- | --- | --- |
| `youtube_url` | yes | - | YouTube video URL; playlists download only the selected video |
| `start` | yes | - | Segment start, `MM:SS` or `HH:MM:SS` |
| `end` | yes | - | Segment end, `MM:SS` or `HH:MM:SS` |
| `duration` | yes | - | Requested final length, `MM:SS` or `HH:MM:SS` |
| `seamless` | no | `true` | `true` applies the tail/head crossfade, `false` keeps the original plain concatenation |
| `crossfade` | no | `0.5` | Crossfade length in seconds; ignored when `seamless` is `false` |
| `loops` | no | derived | Explicit loop count; `0`/omitted derives it from `duration` |

The web UI sends exactly these fields, so the API and the browser share the same
parameter names and semantics.

Successful response:

```http
200 OK
Content-Type: audio/mpeg
Content-Disposition: attachment; filename="looped-audio.mp3"
X-Seamless: true
X-Crossfade-Seconds: 0.500
X-Loop-Count: 4
X-Clip-Seconds: 16.000
X-Output-Seconds: 62.000
```

The `X-*` headers describe the timeline that was actually rendered. They are
informational; the body is always the MP3.

Error response:

```json
{
  "error": "invalid youtube url"
}
```

Example request:

```bash
curl -X POST http://localhost:8084/api/v1/audio/loop \
  -H "Content-Type: application/json" \
  -d '{
    "youtube_url": "https://www.youtube.com/watch?v=HDDdKCSn25Y&list=RDHDDdKCSn25Y&start_radio=1",
    "start": "00:20",
    "end": "00:36",
    "duration": "01:00"
  }' \
  --output result.mp3
```

Playlist and YouTube Mix URLs are accepted, but only the selected video is downloaded.

### Seamless Loop Math

Let, in frames at the output sample rate (44.1 kHz):

- `C` = decoded clip length
- `F` = `round(crossfade * sampleRate)`
- `P` = `C - F` = loop unit length
- `D` = `ceil(duration * sampleRate)`
- `N` = loop count

The loop unit mixes the clip's tail into its head with equal-power gains:

```text
u[q] = clip[C-F+q] * cos(a) + clip[q] * sin(a)   for q < F,  a = (q+0.5)/F * pi/2
u[q] = clip[q]                                   for F <= q < P
```

The gains are computed once per frame and applied to every channel
independently, so stereo left/right never bleed into each other. The output is
the unit repeated:

```text
N = max(1, ceil(D / P))
output = N * P frames
output_seconds = N * (C - F) / sampleRate
```

Because the unit is circular, every seam (including the boundary between the
end of the file and its beginning) is crossfaded. The output is always a whole
number of units, so it covers `duration` and overshoots by less than one unit.
With `crossfade = 0`, `P = C` and the formula reduces to `N = ceil(D / C)` and
`output = N * C`, the original plain concatenation. If `loops` is set, `N` uses
that value instead; `duration` is then only used for validation.

The crossfade must be at most half the clip (`2F <= C`); a longer fade would
overlap more than two copies of the clip. This is checked against the actual
decoded length, because the downloaded segment can be slightly shorter than the
requested timestamps.

## Edge Cases

| Case | Result |
| --- | --- |
| Crossfade longer than the clip | `400` with a crossfade message before any encoding; no output file is left behind |
| Extremely short input | The fade is validated against the decoded length; small clips still loop, and an empty decode returns `422` |
| No audio track | `422` with `Downloaded segment has no audio track`; the ffprobe check runs before decoding |
| Download interrupted / client cancels | The request context cancels `yt-dlp`, the partial clip and output are removed, and the job directory is deleted |
| Processing timeout (5 min) | `504` with `audio processing timed out`; the job directory is deleted |
| Client disconnect | The handler aborts with `408` and the same cleanup runs |

Every request gets an isolated `tmp/jobs/<uuid>` directory. It is removed on
every return path, including cancellation and encoding failures, so partial
downloads and MP3s never accumulate.

## Telegram Bot

Set environment variables:

```bash
export TELEGRAM_BOT_TOKEN=your_token
export AUDIO_API_URL=http://localhost:8084
```

Run the bot:

```bash
go run ./cmd/bot
```

Bot flow:

1. Start the bot with `/start`.
2. Send a YouTube URL.
3. Send the segment start timestamp.
4. Send the segment end timestamp.
5. Send the final audio duration.
6. Receive `looped-audio.mp3`.

## Docker

Build and run:

```bash
docker compose up --build
```

Then open:

```text
http://localhost:8084/
```

## Render Deployment Notes

Current Render URL:

```text
https://youtube-audio-looper.onrender.com/
```

Some YouTube videos require an authenticated browser session and `yt-dlp` can fail with:

```text
Sign in to confirm you're not a bot
```

For Render, upload a cookies file as a secret file:

1. Export YouTube cookies in Netscape/Mozilla `cookies.txt` format.
2. In Render, open the service and go to **Environment**.
3. Under **Secret Files**, add a file named `youtube-cookies.txt`.
4. Set environment variable `YTDLP_COOKIES_PATH` to:

```text
/etc/secrets/youtube-cookies.txt
```

5. Set `YTDLP_USER_AGENT` to the full user-agent string from the same browser session you exported cookies from.
6. Save and redeploy the service.

Do not commit cookies to git. Cookies are account credentials and should be treated as secrets.

After redeploy, the logs should include:

```text
yt-dlp cookies configured path=/etc/secrets/youtube-cookies.txt
yt-dlp user agent configured
```

If logs say `yt-dlp cookies not configured`, Render is still running without the cookie secret or `YTDLP_COOKIES_PATH` value.

Render secret files are read-only, so the app copies the cookie file to temporary storage before running `yt-dlp`.

## Project Structure

```text
cmd/
├── api/                 # REST API and web UI entrypoint
└── bot/                 # Telegram bot entrypoint

internal/
├── bot/                 # Telegram handlers and state machine
├── client/              # API client used by the bot
├── config/              # Runtime configuration
├── downloader/          # yt-dlp wrapper
├── entity/              # Request and job models
├── handler/             # HTTP handlers
├── processor/           # FFmpeg processing
├── service/             # Audio service and job management
├── utils/               # Time and directory helpers
└── validator/           # Request validation

web/
├── index.html           # One-page web interface
├── script.js            # Form submission and result rendering
└── styles.css           # Web UI styles

docs/
└── images/              # Demo assets
```

## Processing Flow

```text
Request
  -> Validate URL and timestamps
  -> Create temporary job directory
  -> Download selected audio segment with yt-dlp
  -> Decode to 44.1 kHz stereo float PCM (ffprobe first checks for an audio track)
  -> Crossfade the clip tail into its head with equal-power gains
  -> Repeat the loop unit N times and encode the MP3
  -> Return MP3 response with timeline headers
  -> Remove temporary files
```

## Validation Rules

- YouTube hosts: `youtube.com`, `www.youtube.com`, `m.youtube.com`, `music.youtube.com`, and `youtu.be`.
- Time format: `MM:SS` or `HH:MM:SS`.
- Final duration must be greater than zero.
- Selected segment must not be longer than the final duration.
- Final duration limit: 10 minutes.
- Selected segment limit: 60 seconds.
- `crossfade` must be between `0` and `10` seconds and no longer than half of the
  downloaded segment (checked again against the real decoded length).
- `loops` must be between `0` and `1000`; `0` or omitted derives the count from
  `duration`.

## Demo

![Telegram Bot Demo](docs/images/demo.png)
