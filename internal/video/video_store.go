package video

// Store helpers for the video-studio module. Kept in its own file so the
// already-large store.go doesn't grow further; the schema lives in
// openDB above. All helpers enforce per-user ownership via WHERE clauses
// in the caller — handlers must still verify the session user owns the
// project before invoking these.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ---- LLMConfig: video-kind helpers ---------------------------------------

// listVideoLLMConfigsForUser returns the workspace's own video-kind
// configs plus any shared video-kind configs from any workspace. Used
// by the video-studio config picker. Name kept for back-compat; the
// scope is workspace not user post-T6-misc.
func (p *Plugin) listVideoLLMConfigsForUser(workspaceID string) ([]LLMConfig, error) {
	rows, err := p.DB.Query(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), share_id, shared, name, base_url, model, system_prompt,
		        provider_kind, extras, (api_key <> '') AS has_api_key, created_at, updated_at
		   FROM llm_configs
		  WHERE provider_kind LIKE 'video.%'
		    AND (workspace_id = $1 OR shared = TRUE OR is_platform = TRUE)
		  ORDER BY (workspace_id = $1) DESC, updated_at DESC, id DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]LLMConfig, 0)
	for rows.Next() {
		var item LLMConfig
		var extras []byte
		if err := rows.Scan(&item.ID, &item.OwnerUserID, &item.WorkspaceID, &item.ShareID, &item.Shared, &item.Name, &item.BaseURL, &item.Model, &item.SystemPrompt, &item.ProviderKind, &extras, &item.HasAPIKey, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if len(extras) > 0 {
			item.Extras = json.RawMessage(extras)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// getVideoLLMConfigWithAPIKey fetches a video-kind config the user is allowed
// to use (own or shared) along with the decrypted API key and extras blob.
// Returns (nil, "", nil, nil) if the config doesn't exist or is the wrong
// kind.
func (p *Plugin) getVideoLLMConfigWithAPIKey(workspaceID string, id int64) (*LLMConfig, string, error) {
	var item LLMConfig
	var apiKey string
	var extras []byte
	err := p.DB.QueryRow(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), share_id, shared, name, base_url, model, api_key, system_prompt,
		        provider_kind, extras, (api_key <> '') AS has_api_key, created_at, updated_at
		   FROM llm_configs
		  WHERE id = $1
		    AND provider_kind LIKE 'video.%'
		    AND (workspace_id = $2 OR shared = TRUE OR is_platform = TRUE)`,
		id, workspaceID,
	).Scan(&item.ID, &item.OwnerUserID, &item.WorkspaceID, &item.ShareID, &item.Shared, &item.Name, &item.BaseURL, &item.Model, &apiKey, &item.SystemPrompt, &item.ProviderKind, &extras, &item.HasAPIKey, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", nil
		}
		return nil, "", err
	}
	if len(extras) > 0 {
		item.Extras = json.RawMessage(extras)
	}
	return &item, apiKey, nil
}

// upsertLLMConfigKindAndExtras flips an existing config to a different kind
// or rewrites its extras blob. Used by the LLM config edit form when the
// user changes the provider type.
func (p *Plugin) upsertLLMConfigKindAndExtras(workspaceID string, id int64, kind string, extras json.RawMessage, now time.Time) error {
	if extras == nil {
		extras = json.RawMessage(`{}`)
	}
	_, err := p.DB.Exec(
		`UPDATE llm_configs
		    SET provider_kind = $3, extras = $4, updated_at = $5
		  WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID, kind, []byte(extras), now,
	)
	return err
}

// ---- video_projects -------------------------------------------------------

func scanVideoProject(scan func(...any) error) (*VideoProject, error) {
	var vp VideoProject
	var defaultLLMConfigID sql.NullInt64
	if err := scan(&vp.ID, &vp.OwnerUserID, &vp.WorkspaceID, &vp.Title, &defaultLLMConfigID, &vp.Status, &vp.FinalVideoURL, &vp.FinalRenderError, &vp.CreatedAt, &vp.UpdatedAt); err != nil {
		return nil, err
	}
	if defaultLLMConfigID.Valid {
		v := defaultLLMConfigID.Int64
		vp.DefaultLLMConfigID = &v
	}
	return &vp, nil
}

func (p *Plugin) listVideoProjects(workspaceID string) ([]VideoProject, error) {
	rows, err := p.DB.Query(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), title, default_llm_config_id, status, final_video_url, final_render_error, created_at, updated_at
		   FROM video_projects
		  WHERE workspace_id = $1
		  ORDER BY updated_at DESC, id DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]VideoProject, 0)
	for rows.Next() {
		row, err := scanVideoProject(rows.Scan)
		if err != nil {
			return nil, err
		}
		items = append(items, *row)
	}
	return items, rows.Err()
}

func (p *Plugin) getVideoProject(workspaceID string, id int64) (*VideoProject, error) {
	row := p.DB.QueryRow(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), title, default_llm_config_id, status, final_video_url, final_render_error, created_at, updated_at
		   FROM video_projects
		  WHERE id = $1 AND workspace_id = $2`,
		id, workspaceID,
	)
	out, err := scanVideoProject(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// getVideoProjectByID skips the workspace check; used by background workers
// that already validated ownership at submit time.
func (p *Plugin) getVideoProjectByID(id int64) (*VideoProject, error) {
	row := p.DB.QueryRow(
		`SELECT id, owner_user_id, COALESCE(workspace_id, ''), title, default_llm_config_id, status, final_video_url, final_render_error, created_at, updated_at
		   FROM video_projects
		  WHERE id = $1`,
		id,
	)
	out, err := scanVideoProject(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

func (p *Plugin) createVideoProject(workspaceID, ownerUserID, title string, defaultLLMConfigID *int64, now time.Time) (*VideoProject, error) {
	row := p.DB.QueryRow(
		`INSERT INTO video_projects (owner_user_id, workspace_id, title, default_llm_config_id, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 'draft', $5, $5)
		 RETURNING id, owner_user_id, COALESCE(workspace_id, ''), title, default_llm_config_id, status, final_video_url, final_render_error, created_at, updated_at`,
		ownerUserID, workspaceID, strings.TrimSpace(title), defaultLLMConfigID, now,
	)
	return scanVideoProject(row.Scan)
}

func (p *Plugin) updateVideoProject(workspaceID string, id int64, title string, defaultLLMConfigID *int64, now time.Time) (*VideoProject, error) {
	row := p.DB.QueryRow(
		`UPDATE video_projects
		    SET title = $3, default_llm_config_id = $4, updated_at = $5
		  WHERE id = $1 AND workspace_id = $2
		  RETURNING id, owner_user_id, COALESCE(workspace_id, ''), title, default_llm_config_id, status, final_video_url, final_render_error, created_at, updated_at`,
		id, workspaceID, strings.TrimSpace(title), defaultLLMConfigID, now,
	)
	out, err := scanVideoProject(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

func (p *Plugin) updateVideoProjectStatus(id int64, status, finalURL, renderError string, now time.Time) error {
	_, err := p.DB.Exec(
		`UPDATE video_projects
		    SET status = $2, final_video_url = $3, final_render_error = $4, updated_at = $5
		  WHERE id = $1`,
		id, status, finalURL, renderError, now,
	)
	return err
}

func (p *Plugin) deleteVideoProject(workspaceID string, id int64) (bool, error) {
	res, err := p.DB.Exec(`DELETE FROM video_projects WHERE id = $1 AND workspace_id = $2`, id, workspaceID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ---- video_shots ----------------------------------------------------------

func scanVideoShot(scan func(...any) error) (*VideoShot, error) {
	var vs VideoShot
	var llmConfigID sql.NullInt64
	var submittedAt, completedAt sql.NullTime
	if err := scan(&vs.ID, &vs.ProjectID, &vs.Ord, &vs.Prompt, &vs.Ratio, &vs.Duration, &vs.GenerateAudio, &vs.Watermark, &llmConfigID, &vs.TaskID, &vs.Status, &vs.VideoURL, &vs.PosterURL, &vs.TrimStartMs, &vs.TrimEndMs, &vs.ErrorMessage, &submittedAt, &completedAt, &vs.CreatedAt, &vs.UpdatedAt); err != nil {
		return nil, err
	}
	if llmConfigID.Valid {
		v := llmConfigID.Int64
		vs.LLMConfigID = &v
	}
	if submittedAt.Valid {
		t := submittedAt.Time
		vs.SubmittedAt = &t
	}
	if completedAt.Valid {
		t := completedAt.Time
		vs.CompletedAt = &t
	}
	return &vs, nil
}

const videoShotColumns = `id, project_id, ord, prompt, ratio, duration, generate_audio, watermark, llm_config_id, task_id, status, video_url, poster_url, trim_start_ms, trim_end_ms, error_message, submitted_at, completed_at, created_at, updated_at`

func (p *Plugin) listVideoShotsForProject(projectID int64) ([]VideoShot, error) {
	rows, err := p.DB.Query(
		`SELECT `+videoShotColumns+` FROM video_shots WHERE project_id = $1 ORDER BY ord ASC, id ASC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]VideoShot, 0)
	for rows.Next() {
		shot, err := scanVideoShot(rows.Scan)
		if err != nil {
			return nil, err
		}
		items = append(items, *shot)
	}
	return items, rows.Err()
}

// listInflightVideoShots returns shots in queued or running state across all
// projects. Used by the poll worker on each tick to drive Seedance polling.
func (p *Plugin) listInflightVideoShots(ctx context.Context, limit int) ([]VideoShot, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := p.DB.QueryContext(ctx,
		`SELECT `+videoShotColumns+`
		   FROM video_shots
		  WHERE status IN ('queued','running')
		    AND task_id <> ''
		  ORDER BY submitted_at ASC NULLS FIRST, id ASC
		  LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]VideoShot, 0)
	for rows.Next() {
		shot, err := scanVideoShot(rows.Scan)
		if err != nil {
			return nil, err
		}
		items = append(items, *shot)
	}
	return items, rows.Err()
}

func (p *Plugin) getVideoShot(projectID, id int64) (*VideoShot, error) {
	row := p.DB.QueryRow(
		`SELECT `+videoShotColumns+` FROM video_shots WHERE id = $1 AND project_id = $2`,
		id, projectID,
	)
	shot, err := scanVideoShot(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return shot, nil
}

type CreateVideoShotInput struct {
	ProjectID     int64
	Ord           int
	Prompt        string
	Ratio         string
	Duration      int
	GenerateAudio bool
	Watermark     bool
	LLMConfigID   *int64
	TrimStartMs   int
	TrimEndMs     int
}

func (p *Plugin) createVideoShot(in CreateVideoShotInput, now time.Time) (*VideoShot, error) {
	row := p.DB.QueryRow(
		`INSERT INTO video_shots (project_id, ord, prompt, ratio, duration, generate_audio, watermark, llm_config_id, status, trim_start_ms, trim_end_ms, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', $9, $10, $11, $11)
		 RETURNING `+videoShotColumns,
		in.ProjectID, in.Ord, in.Prompt, in.Ratio, in.Duration, in.GenerateAudio, in.Watermark, in.LLMConfigID, in.TrimStartMs, in.TrimEndMs, now,
	)
	return scanVideoShot(row.Scan)
}

type UpdateVideoShotInput struct {
	Prompt        *string
	Ratio         *string
	Duration      *int
	GenerateAudio *bool
	Watermark     *bool
	Ord           *int
	LLMConfigID   *int64
	TrimStartMs   *int
	TrimEndMs     *int
}

func (p *Plugin) updateVideoShotFields(projectID, id int64, in UpdateVideoShotInput, now time.Time) (*VideoShot, error) {
	clauses := make([]string, 0, 9)
	args := []any{id, projectID}
	idx := 3
	if in.Prompt != nil {
		clauses = append(clauses, "prompt = $"+itoa(idx))
		args = append(args, *in.Prompt)
		idx++
	}
	if in.Ratio != nil {
		clauses = append(clauses, "ratio = $"+itoa(idx))
		args = append(args, *in.Ratio)
		idx++
	}
	if in.Duration != nil {
		clauses = append(clauses, "duration = $"+itoa(idx))
		args = append(args, *in.Duration)
		idx++
	}
	if in.GenerateAudio != nil {
		clauses = append(clauses, "generate_audio = $"+itoa(idx))
		args = append(args, *in.GenerateAudio)
		idx++
	}
	if in.Watermark != nil {
		clauses = append(clauses, "watermark = $"+itoa(idx))
		args = append(args, *in.Watermark)
		idx++
	}
	if in.Ord != nil {
		clauses = append(clauses, "ord = $"+itoa(idx))
		args = append(args, *in.Ord)
		idx++
	}
	if in.LLMConfigID != nil {
		clauses = append(clauses, "llm_config_id = $"+itoa(idx))
		args = append(args, *in.LLMConfigID)
		idx++
	}
	if in.TrimStartMs != nil {
		clauses = append(clauses, "trim_start_ms = $"+itoa(idx))
		args = append(args, *in.TrimStartMs)
		idx++
	}
	if in.TrimEndMs != nil {
		clauses = append(clauses, "trim_end_ms = $"+itoa(idx))
		args = append(args, *in.TrimEndMs)
		idx++
	}
	if len(clauses) == 0 {
		return p.getVideoShot(projectID, id)
	}
	clauses = append(clauses, "updated_at = $"+itoa(idx))
	args = append(args, now)

	query := `UPDATE video_shots SET ` + strings.Join(clauses, ", ") +
		` WHERE id = $1 AND project_id = $2 RETURNING ` + videoShotColumns
	shot, err := scanVideoShot(p.DB.QueryRow(query, args...).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return shot, nil
}

// setVideoShotPoster persists the cached poster URL so the frontend can
// render <video poster="..."> without forcing the browser to scan the
// MP4. Empty values are tolerated (we fail gracefully if ffmpeg can't
// produce a frame for some reason).
func (p *Plugin) setVideoShotPoster(id int64, posterURL string, now time.Time) error {
	_, err := p.DB.Exec(
		`UPDATE video_shots SET poster_url = $2, updated_at = $3 WHERE id = $1`,
		id, posterURL, now,
	)
	return err
}

// markVideoShotSubmitted flips a shot to 'queued' with a fresh task_id and
// submission timestamp. Idempotent on retry — overwrites the task_id.
func (p *Plugin) markVideoShotSubmitted(id int64, taskID string, now time.Time) error {
	_, err := p.DB.Exec(
		`UPDATE video_shots
		    SET task_id = $2, status = 'queued', submitted_at = $3, error_message = '', updated_at = $3
		  WHERE id = $1`,
		id, taskID, now,
	)
	return err
}

// markVideoShotResubmitted is the path used by both first-time submissions
// and regenerations — clears video_url / poster_url / completed_at along
// with the queued-state reset so the UI flips back to "generating" the
// instant the user clicks Regenerate. Safe for first-time submissions
// because those fields are already empty.
func (p *Plugin) markVideoShotResubmitted(id int64, taskID string, now time.Time) error {
	_, err := p.DB.Exec(
		`UPDATE video_shots
		    SET task_id = $2,
		        status = 'queued',
		        video_url = '',
		        poster_url = '',
		        error_message = '',
		        completed_at = NULL,
		        submitted_at = $3,
		        updated_at = $3
		  WHERE id = $1`,
		id, taskID, now,
	)
	return err
}

// markVideoShotStatus updates the in-flight status as the poll worker sees
// new states from the provider. videoURL is only meaningful when the new
// status is 'succeeded'; errorMessage only when 'failed'.
func (p *Plugin) markVideoShotStatus(id int64, status, videoURL, errorMessage string, now time.Time) error {
	completed := status == VideoShotStatusSucceeded || status == VideoShotStatusFailed
	if completed {
		_, err := p.DB.Exec(
			`UPDATE video_shots
			    SET status = $2, video_url = $3, error_message = $4, completed_at = $5, updated_at = $5
			  WHERE id = $1`,
			id, status, videoURL, errorMessage, now,
		)
		return err
	}
	_, err := p.DB.Exec(
		`UPDATE video_shots SET status = $2, updated_at = $3 WHERE id = $1`,
		id, status, now,
	)
	return err
}

// BilledShotFields is the per-shot billing payload written by the
// poll worker after a successful generation. Mirrors the columns
// added in the P0 migration of doc/llm/video-billing.md.
type BilledShotFields struct {
	Provider           string
	Model              string
	Resolution         string
	DurationChargedSec float64
	FPS                int
	FramesTotal        int
	CostUSD            float64
	CostPerFrameUSD    float64
	BillingMetaJSON    []byte // already-marshaled JSON for the JSONB column; nil → SQL NULL
}

// markVideoShotBilled writes the billing columns once per shot. The
// `billed_at IS NULL` clause makes the update idempotent — a re-poll
// (or a manual reconcile) will not double-count.
func (p *Plugin) markVideoShotBilled(id int64, in BilledShotFields, now time.Time) error {
	var meta interface{}
	if len(in.BillingMetaJSON) > 0 {
		meta = in.BillingMetaJSON
	}
	_, err := p.DB.Exec(
		`UPDATE video_shots
		    SET provider = $2,
		        billing_model = $3,
		        resolution = $4,
		        duration_charged_sec = $5,
		        fps = $6,
		        frames_total = $7,
		        cost_usd = $8,
		        cost_per_frame_usd = $9,
		        billing_meta = $10,
		        billed_at = $11,
		        updated_at = $11
		  WHERE id = $1 AND billed_at IS NULL`,
		id,
		in.Provider, in.Model, in.Resolution,
		in.DurationChargedSec, in.FPS, in.FramesTotal,
		in.CostUSD, in.CostPerFrameUSD,
		meta, now,
	)
	return err
}

// getVideoShotBilling reads back the billing columns for diagnostics
// and for the dock POST payload (so the polar-video → dock hop in P2
// can use canonical values rather than re-derive them).
func (p *Plugin) getVideoShotBilling(id int64) (BilledShotFields, bool, error) {
	var (
		out      BilledShotFields
		dur      sql.NullFloat64
		fps      sql.NullInt64
		frames   sql.NullInt64
		cost     sql.NullFloat64
		perFrame sql.NullFloat64
		meta     []byte
		billedAt sql.NullTime
	)
	err := p.DB.QueryRow(
		`SELECT COALESCE(provider, ''),
		        COALESCE(billing_model, ''),
		        COALESCE(resolution, ''),
		        duration_charged_sec, fps, frames_total,
		        cost_usd, cost_per_frame_usd, billing_meta, billed_at
		   FROM video_shots WHERE id = $1`,
		id,
	).Scan(
		&out.Provider, &out.Model, &out.Resolution,
		&dur, &fps, &frames,
		&cost, &perFrame, &meta, &billedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, false, nil
		}
		return out, false, err
	}
	if !billedAt.Valid {
		return out, false, nil
	}
	if dur.Valid {
		out.DurationChargedSec = dur.Float64
	}
	if fps.Valid {
		out.FPS = int(fps.Int64)
	}
	if frames.Valid {
		out.FramesTotal = int(frames.Int64)
	}
	if cost.Valid {
		out.CostUSD = cost.Float64
	}
	if perFrame.Valid {
		out.CostPerFrameUSD = perFrame.Float64
	}
	out.BillingMetaJSON = meta
	return out, true, nil
}

func (p *Plugin) deleteVideoShot(projectID, id int64) (bool, error) {
	res, err := p.DB.Exec(`DELETE FROM video_shots WHERE id = $1 AND project_id = $2`, id, projectID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ---- video_assets ---------------------------------------------------------

func scanVideoAsset(scan func(...any) error) (*VideoAsset, error) {
	var a VideoAsset
	if err := scan(&a.ID, &a.ProjectID, &a.Kind, &a.URL, &a.FileName, &a.MimeType, &a.Size, &a.DurationMs, &a.BGMVolume, &a.VoiceVolume, &a.CreatedAt); err != nil {
		return nil, err
	}
	return &a, nil
}

const videoAssetColumns = `id, project_id, kind, url, file_name, mime_type, size, duration_ms, bgm_volume, voice_volume, created_at`

func (p *Plugin) listVideoAssetsForProject(projectID int64) ([]VideoAsset, error) {
	rows, err := p.DB.Query(
		`SELECT `+videoAssetColumns+` FROM video_assets WHERE project_id = $1 ORDER BY created_at DESC, id DESC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]VideoAsset, 0)
	for rows.Next() {
		asset, err := scanVideoAsset(rows.Scan)
		if err != nil {
			return nil, err
		}
		items = append(items, *asset)
	}
	return items, rows.Err()
}

func (p *Plugin) getVideoAsset(projectID, id int64) (*VideoAsset, error) {
	row := p.DB.QueryRow(
		`SELECT `+videoAssetColumns+` FROM video_assets WHERE id = $1 AND project_id = $2`,
		id, projectID,
	)
	a, err := scanVideoAsset(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return a, nil
}

type CreateVideoAssetInput struct {
	ProjectID  int64
	Kind       string
	URL        string
	FileName   string
	MimeType   string
	Size       int64
	DurationMs int
}

func (p *Plugin) createVideoAsset(in CreateVideoAssetInput, now time.Time) (*VideoAsset, error) {
	row := p.DB.QueryRow(
		`INSERT INTO video_assets (project_id, kind, url, file_name, mime_type, size, duration_ms, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING `+videoAssetColumns,
		in.ProjectID, in.Kind, in.URL, in.FileName, in.MimeType, in.Size, in.DurationMs, now,
	)
	return scanVideoAsset(row.Scan)
}

func (p *Plugin) updateVideoAssetVolumes(projectID, id int64, bgmVolume, voiceVolume *float64) (*VideoAsset, error) {
	clauses := []string{}
	args := []any{id, projectID}
	idx := 3
	if bgmVolume != nil {
		clauses = append(clauses, "bgm_volume = $"+itoa(idx))
		args = append(args, *bgmVolume)
		idx++
	}
	if voiceVolume != nil {
		clauses = append(clauses, "voice_volume = $"+itoa(idx))
		args = append(args, *voiceVolume)
		idx++
	}
	if len(clauses) == 0 {
		return p.getVideoAsset(projectID, id)
	}
	query := `UPDATE video_assets SET ` + strings.Join(clauses, ", ") +
		` WHERE id = $1 AND project_id = $2 RETURNING ` + videoAssetColumns
	a, err := scanVideoAsset(p.DB.QueryRow(query, args...).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return a, nil
}

func (p *Plugin) deleteVideoAsset(projectID, id int64) (bool, error) {
	res, err := p.DB.Exec(`DELETE FROM video_assets WHERE id = $1 AND project_id = $2`, id, projectID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// itoa is a tiny helper so we can build $N parameter placeholders without
// pulling strconv in just for this. Avoids a heap allocation for small ints.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	buf := [12]byte{}
	pos := len(buf)
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = digits[n%10]
		n /= 10
	}
	if negative {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
