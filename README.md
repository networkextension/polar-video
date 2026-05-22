# polar-video

Video-generation plugin for the [Polar](https://github.com/networkextension/Polar) platform.

Multi-shot AI video studio: per-shot prompt → external provider (today: Volces / Doubao Seedance) → downloaded MP4 → ffmpeg concat with optional BGM + voiceover. Owns `polar_video` (projects + shots + assets) and a local blob root.

## Status

W3 extracted at 2026-05-22 from the Polar monorepo. Build green; some cross-domain helpers (WS broadcast, polar-attachment Store, chat-thread persistence) are stubbed pending follow-up PRs — see TODO(extract) markers in `internal/video/stubs.go`. Dock still serves `/api/video-*` until ops flips `POLAR_VIDEO_REMOTE=true`.

## Install

```bash
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /tmp/video-svc ./cmd/video-svc
rsync -avz /tmp/video-svc local@<deploy-box>:/Users/local/.local/bin/
```

Required system dep: **ffmpeg** on $PATH (or set `FFMPEG_BIN=/path/to/ffmpeg`).

## Environment

- `POLAR_DOCK_BASE` — dock URL (default `http://127.0.0.1:8080`)
- `POLAR_PLUGIN_NAME` — `video`
- `POLAR_PLUGIN_TOKEN` — plaintext from `/admin-plugins.html` (one-time print at row creation)
- `POLAR_VIDEO_DB_DSN` — Postgres DSN for `polar_video`
- `POLAR_VIDEO_LISTEN` — default `127.0.0.1:8092`
- `POLAR_VIDEO_VERSION` — git sha
- `POLAR_VIDEO_BLOB_DIR` — local data root (default `/Users/local/video-svc-data`); subdirs: `shots/`, `exports/`, `bgm/`, `voice/`
- `POLAR_VIDEO_METRICS_TOKEN` — bearer for `/metrics`; unset = 404
- `VIDEO_SEEDANCE_BASE_URL` — Volces / Doubao Seedance API endpoint
- `VIDEO_SEEDANCE_MODEL` — Seedance model id (e.g. `doubao-seedance-1-0-pro-250528`)
- `VIDEO_SEEDANCE_API_KEY` — Seedance API key (also auto-bootstrapped into `llm_configs` as `video.seedance` on first boot)
- `FFMPEG_BIN` — override ffmpeg binary path

## Endpoints

All under `/api`, require Bearer token (validated via dock `/internal/v1/auth/verify`):

- `GET  /video-llm-configs`
- `GET  /video-projects`
- `POST /video-projects`
- `GET  /video-projects/:id`
- `PATCH /video-projects/:id`
- `DELETE /video-projects/:id`
- `POST /video-projects/:id/shots`
- `PATCH /video-projects/:id/shots/:shotId`
- `DELETE /video-projects/:id/shots/:shotId`
- `POST /video-projects/:id/shots/:shotId/submit`
- `POST /video-projects/:id/shots/:shotId/retry`
- `POST /video-projects/:id/shots/:shotId/duplicate`
- `POST /video-projects/:id/shots/:shotId/extract-frame`
- `POST /video-projects/:id/submit-all`
- `POST /video-projects/:id/assets`
- `PATCH /video-projects/:id/assets/:assetId`
- `DELETE /video-projects/:id/assets/:assetId`
- `POST /video-projects/:id/render`
- `GET  /video-projects/:id/download`
- `GET  /video-projects/:id/shots.zip`

## Schema

`scripts/migrate/video-schema.sql` — `video_projects` / `video_shots` / `video_assets`. Apply with `scripts/migrate/video-data.sh`.

## Nginx

`scripts/nginx/video-svc-snippet.conf` — drop into the main Polar server block and reload.

## Related

- [Polar dock](https://github.com/networkextension/Polar)
- [polar-sdk](https://github.com/networkextension/polar-sdk)
- [Architecture doc](doc/video-studio.md)

## License

MIT
