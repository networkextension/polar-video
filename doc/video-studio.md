# Video Studio

A first-class module inside IdeaMesh for producing multi-shot AI videos. The
target user is a creator who writes a short script (e.g. ten 10-second shots),
sends each shot to a video-generation API, polls until the MP4s are ready,
optionally layers in background music and a recorded voiceover, and merges
everything into a single deliverable. Heavy editing is intentionally **not**
in scope — that workflow stays on CapCut. Video Studio's job is to **produce
edit-ready material**, not replace the editor.

## Goals

- **Project workspace.** Users own *projects*, each with an ordered list of
  *shots* and per-project audio *assets*. Refresh-safe — projects, shots, and
  pending tasks survive server restarts.
- **Reuse existing LLM credential plumbing.** Add a `provider_kind` discriminator
  to `llm_configs` and a `extras` JSONB blob for provider-specific defaults
  (ratio, duration, watermark, etc.). The chat bot picker continues to filter
  to `'text'` configs; the video studio config picker filters to video kinds.
- **Multi-provider seam from day one.** The first PR ships only Volces
  Seedance, but the dispatcher in `video_provider.go` is structured so that
  Runway / Kling / Pika land as one new file plus one switch case.
- **Async-by-default.** Submission writes a row, returns a task ID, and a
  background worker polls each provider on a ticker. The MP4 lands in the
  same `Storage` interface chat attachments use (local disk in dev, R2 in
  prod). The frontend gets live status via the existing WS hub.
- **Voiceover in the browser.** A MediaRecorder booth captures the user's
  voice, uploads as a `voiceover` asset, and is mixed during the final render
  so the result feels like a person narrating instead of a silent generated
  reel.
- **Final render = ffmpeg concat.** Shots concatenated in order with optional
  BGM (ducked to ~0.3) and voiceover (1.0). Output is one MP4 the user can
  download or hand off to CapCut.

## Non-goals

- In-browser timeline editor with multi-track / transitions / keyframes — use
  CapCut.
- Subtitle burn-in, motion graphics, color grading — out of scope.
- Per-shot trim UI (the schema reserves `trim_start_ms` / `trim_end_ms`, and
  the ffmpeg pipeline already honors them, so a future PR is a pure UI add).
- Real-time render progress percentage. First cut shows `rendering` →
  `rendered` and that's enough for a 30-second concat job.

## Data model

All schema changes are `ALTER TABLE ... ADD COLUMN IF NOT EXISTS` so existing
deployments upgrade automatically on next boot.

### `llm_configs` (extended)

| Column           | Type   | Default | Notes                                         |
| ---------------- | ------ | ------- | --------------------------------------------- |
| `provider_kind`  | TEXT   | `'text'`| `'text'` for chat LLMs, `'video.seedance'` for Volces. |
| `extras`         | JSONB  | `'{}'`  | Per-kind defaults, e.g. `{"ratio":"9:16","duration":10,"generate_audio":true,"watermark":false}` for Seedance. Untouched for text rows. |

### `video_projects`

| Column                | Type        | Notes                                               |
| --------------------- | ----------- | --------------------------------------------------- |
| `id`                  | BIGSERIAL   |                                                     |
| `owner_user_id`       | TEXT        | NOT NULL.                                           |
| `title`               | TEXT        |                                                     |
| `status`              | TEXT        | `'draft'` / `'rendering'` / `'rendered'` / `'failed'`. |
| `final_video_url`     | TEXT        | Set on render success.                              |
| `final_render_error`  | TEXT        | Set on render failure.                              |
| `created_at`          | TIMESTAMPTZ |                                                     |
| `updated_at`          | TIMESTAMPTZ |                                                     |

### `video_shots`

| Column           | Type        | Notes                                                                    |
| ---------------- | ----------- | ------------------------------------------------------------------------ |
| `id`             | BIGSERIAL   |                                                                          |
| `project_id`     | BIGINT      | FK to `video_projects` ON DELETE CASCADE.                                |
| `ord`            | INT         | Display order (0-based). Drag-to-reorder updates this column.            |
| `prompt`         | TEXT        | The prompt text sent to the provider.                                    |
| `ratio`          | TEXT        | `'9:16'` etc.                                                            |
| `duration`       | INT         | Seconds.                                                                 |
| `generate_audio` | BOOLEAN     | Provider-side audio (Seedance can synth ambient sound).                  |
| `watermark`      | BOOLEAN     |                                                                          |
| `llm_config_id`  | BIGINT      | FK to `llm_configs` (must be a video-kind row).                          |
| `task_id`        | TEXT        | Provider task ID. Empty until submission.                                |
| `status`         | TEXT        | `'pending'` / `'queued'` / `'running'` / `'succeeded'` / `'failed'`.     |
| `video_url`      | TEXT        | Storage URL after the worker downloads the MP4.                          |
| `trim_start_ms`  | INT         | Default 0. Reserved for future trim UI.                                  |
| `trim_end_ms`    | INT         | Default 0 (no trim).                                                     |
| `error_message`  | TEXT        | Populated on failure.                                                    |
| `submitted_at`   | TIMESTAMPTZ |                                                                          |
| `completed_at`   | TIMESTAMPTZ |                                                                          |
| `created_at`     | TIMESTAMPTZ |                                                                          |
| `updated_at`     | TIMESTAMPTZ |                                                                          |

### `video_assets`

| Column          | Type        | Notes                                                       |
| --------------- | ----------- | ----------------------------------------------------------- |
| `id`            | BIGSERIAL   |                                                             |
| `project_id`    | BIGINT      | FK to `video_projects` ON DELETE CASCADE.                   |
| `kind`          | TEXT        | `'audio_bgm'` (background music) or `'voiceover'`.          |
| `url`           | TEXT        | Storage URL.                                                |
| `file_name`     | TEXT        |                                                             |
| `mime_type`     | TEXT        | `audio/mpeg`, `audio/aac`, `audio/webm`, `audio/mp4`.       |
| `size`          | BIGINT      |                                                             |
| `duration_ms`   | INT         | Filled by ffprobe at upload time when possible.             |
| `bgm_volume`    | REAL        | Default 0.3. Used at mix time. Editable per-asset.          |
| `voice_volume`  | REAL        | Default 1.0.                                                |
| `created_at`    | TIMESTAMPTZ |                                                             |

## Backend layout

### Provider adapter

`internal/app/dock/video_seedance.go` — direct port of the bash scripts:

```go
type seedanceParams struct {
    Ratio          string
    Duration       int
    GenerateAudio  bool
    Watermark      bool
}

func submitSeedanceTask(ctx context.Context, cfg aiRuntimeConfig, prompt string, params seedanceParams) (taskID string, err error)
func pollSeedanceTask(ctx context.Context, cfg aiRuntimeConfig, taskID string) (status, videoURL string, err error)
```

Reuses `aiRuntimeConfig.APIKey` / `BaseURL` / `Model`. The HTTP body is the
same JSON shape as `openclaw.sh` builds.

### Provider dispatcher

`internal/app/dock/video_provider.go` exposes:

```go
func submitVideoTask(ctx context.Context, cfg aiRuntimeConfig, prompt string, extras map[string]any) (string, error)
func pollVideoTask(ctx context.Context, cfg aiRuntimeConfig, taskID string) (status, videoURL string, err error)
```

Both switch on `cfg.ProviderKind`. Today only `'video.seedance'` is implemented;
unknown kinds return a typed error.

### Handlers

`internal/app/dock/video_studio.go`:

| Method | Path                                                       | Purpose                              |
| ------ | ---------------------------------------------------------- | ------------------------------------ |
| GET    | `/api/video-projects`                                      | List the user's projects.            |
| POST   | `/api/video-projects`                                      | Create a project.                    |
| GET    | `/api/video-projects/:id`                                  | Project detail (project + shots + assets). |
| PATCH  | `/api/video-projects/:id`                                  | Rename / update default config.      |
| DELETE | `/api/video-projects/:id`                                  | Delete (cascades to shots + assets). |
| POST   | `/api/video-projects/:id/shots`                            | Append a shot.                       |
| PATCH  | `/api/video-projects/:id/shots/:shotId`                    | Edit prompt / params / order / trim. |
| DELETE | `/api/video-projects/:id/shots/:shotId`                    | Remove a shot.                       |
| POST   | `/api/video-projects/:id/shots/:shotId/submit`             | Submit one shot to the provider.     |
| POST   | `/api/video-projects/:id/submit-all`                       | Submit all `pending` shots, rate-limited (mirrors bash `sleep 2`). |
| POST   | `/api/video-projects/:id/shots/:shotId/retry`              | Re-submit a failed shot.             |
| POST   | `/api/video-projects/:id/assets`                           | Multipart upload (mp3 / aac / wav / webm). `?kind=audio_bgm\|voiceover`. |
| PATCH  | `/api/video-projects/:id/assets/:assetId`                  | Adjust `bgm_volume` / `voice_volume`. |
| DELETE | `/api/video-projects/:id/assets/:assetId`                  |                                      |
| POST   | `/api/video-projects/:id/render`                           | Enqueue final render. 202.           |
| GET    | `/api/video-projects/:id/download`                         | Redirect to `final_video_url`.       |

All handlers verify `owner_user_id == session.UserID`.

### Workers

`internal/app/dock/video_poll_worker.go` — modeled on `push_worker.go:50-64`:

```go
ticker := time.NewTicker(10 * time.Second)
for {
    select {
    case <-ctx.Done(): return
    case <-ticker.C:
        // claim shots WHERE status IN ('queued','running')
        // for each: pollVideoTask; if succeeded, download MP4 via Storage.Store, set status=succeeded
        // broadcast video_project event to owner
    }
}
```

`internal/app/dock/video_render.go` — single goroutine consuming a `chan int64`
of project IDs. Builds an ffmpeg command:

1. Generate a `concat` list file with one entry per shot (in `ord` order),
   honoring `trim_start_ms` / `trim_end_ms` via input-side `-ss` / `-to`.
2. Optional `-i <bgm>` and `-i <voice>` inputs.
3. `filter_complex` that:
   - Concatenates the video streams (`concat=n=N:v=1:a=1`).
   - If BGM present, mixes BGM at `bgm_volume` ducked behind voice.
   - If voice present, overlays at `voice_volume`.
4. Output `final-<projectID>-<ts>.mp4` in a temp dir, then `Storage.Store` to
   produce the public URL.

Reuses `ffmpegBin` discovery from `video_processing.go`. Updates
`video_projects.status` + `final_video_url` (or `final_render_error`) and
broadcasts a `render_status` WS event.

### WebSocket events

Reuses the existing `wsHub`. New event shape:

```jsonc
{
  "type": "video_project",
  "project_id": 42,
  "kind": "shot_status" | "render_status",
  "payload": {
    /* shot status: shot_id, status, video_url? */
    /* render status: status, final_video_url?, final_render_error? */
  }
}
```

Broadcast only to the project owner. The chat WS handler already returns on
unknown `type` values, so the chat page is unaffected.

## Frontend layout

`ui/public/video-studio.html` — three-pane:

```
┌─────────────────┬───────────────────────────────────────────┐
│ Projects        │ Shots                                     │
│ + New project   │ ┌── shot 1 ── prompt ── status ── video ──│
│                 │ ├── shot 2 ──                             │
│                 │ └── + Add shot                            │
│                 │ Audio                                     │
│                 │ ┌─ BGM ── upload mp3/aac ── volume ───────│
│                 │ └─ Voice ── 🎙 record / upload ── volume ──│
│                 │ Render                                    │
│                 │ [Render final video] → preview / download │
└─────────────────┴───────────────────────────────────────────┘
```

`ui/src/video-studio.ts`:

- Reuses `request` / `requestJson` from `ui/src/api/http.ts`.
- Reuses `initStoredTheme()` + `bindThemeSync()` (UI page-init checklist).
- WS subscribes to `video_project` events and patches the rendered shot rows
  in place (no full re-render, no scroll thrash).
- MediaRecorder voice booth:
  - `navigator.mediaDevices.getUserMedia({ audio: true })`.
  - Records to `audio/webm` (Chromium) or `audio/mp4` (Safari).
  - Uploads via the assets endpoint with `kind=voiceover`. Server transcodes
    to AAC during ffmpeg mix if needed.

`ui/src/api/video.ts` — typed wrappers for every route above.
`ui/src/types/video.ts` — `VideoProject`, `VideoShot`, `VideoAsset`.
`ui/src/lib/i18n.ts` — `video.newProject`, `video.shots`, `video.recordVoice`,
`video.bgm`, `video.render`, etc. (en + zh).
`ui/server.js` — add `video-studio` to `HTML_PAGES` so the cache-bust + no-cache
headers apply.

## Configuration

| Env var                   | Purpose                                                                   |
| ------------------------- | ------------------------------------------------------------------------- |
| `FFMPEG_BIN`              | ffmpeg binary; defaults to existing discovery in `video_processing.go`.   |
| `VIDEO_POLL_INTERVAL`     | Seconds between Seedance polls. Default 10.                               |
| `VIDEO_CONCURRENT_RENDERS`| Max concurrent ffmpeg render jobs. Default 1.                             |
| `VIDEO_SEEDANCE_BASE_URL` | Default `https://ark.cn-beijing.volces.com/api/v3`.                       |
| `VIDEO_SEEDANCE_API_KEY`  | If present and no `'video.seedance'` row exists, seed one for the system bot. |
| `VIDEO_SEEDANCE_MODEL`    | Default `doubao-seedance-1-0-pro-250528`.                                 |

## Verification

1. **Build gate** — `go build ./...`, `go test ./internal/app/dock/...`,
   `cd ui && npx tsc --noEmit && npm run build`.
2. **Schema** — start with an existing local DB, confirm migrations apply
   cleanly and existing `llm_configs` rows get `provider_kind='text'`.
3. **Provider config** — via the LLM-config form, create a `'video.seedance'`
   row pointing at `https://ark.cn-beijing.volces.com/api/v3`. Confirm it
   appears in the Video Studio config picker but not in the chat bot picker.
4. **Project + shots end-to-end** — create a project, paste 3 of the 老梁养龙虾
   prompts, click "Submit all", watch each shot move
   `pending → queued → running → succeeded` via WS without page refresh.
   Preview each MP4 inline; download one.
5. **Audio** — upload an mp3 as BGM; record a 10-second voiceover via the
   browser booth; both should appear as `video_assets` with their `kind`
   set correctly and play back from their storage URLs.
6. **Render** — click "Render final video", watch progress, download the
   result. Verify the visual is the 3 shots concatenated and the audio is
   the BGM mixed under the voiceover (default `bgm_volume=0.3`,
   `voice_volume=1.0`).
7. **Failure paths** — invalidate the API key mid-run → shot flips to `failed`
   with `error_message` populated, retry button re-submits with a new
   `task_id`. Missing ffmpeg → render fails fast with clear error in
   `final_render_error`.
8. **Permission** — second user can't list or access another user's project.

## Out of scope (tracked for follow-up)

- iOS native module that downloads per-shot MP4s for CapCut. The
  per-shot download endpoint here is the contract iOS will consume.
- In-browser trim UI. The `trim_start_ms` / `trim_end_ms` columns and the
  ffmpeg pipeline already honor them — adding the UI is purely frontend work.
- Multi-track timeline editor / transitions / subtitle burn-in.
- Other video providers (Runway, Kling, Pika). The dispatcher seam is in
  place; each addition is one new `video_<provider>.go` file plus one
  switch case.
- Render-progress percentage. The first cut just shows `rendering` →
  `rendered`.
