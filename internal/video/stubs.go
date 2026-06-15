package video

// stubs.go — type definitions copied from dock/store.go (the canonical
// schema lives there; we mirror the row shapes so the moved handlers
// + store + workers compile against a self-contained type set) and
// shims for cross-domain helpers that previously lived in dock but
// don't have a clean SDK surface yet. Each TODO(extract) call site
// logs + degrades gracefully so the extracted svc boots; the follow-up
// PR wires the real path.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	sdk "github.com/networkextension/polar-sdk"
)

// ---- Mirrored row types (canonical = dock/store.go) ------------------

// VideoProject is the top-level container for a multi-shot video
// production. Owns shots + audio assets via FK cascade.
type VideoProject struct {
	ID                 int64     `json:"id"`
	OwnerUserID        string    `json:"owner_user_id"`
	WorkspaceID        string    `json:"workspace_id"`
	Title              string    `json:"title"`
	DefaultLLMConfigID *int64    `json:"default_llm_config_id,omitempty"`
	Status             string    `json:"status"`
	FinalVideoURL      string    `json:"final_video_url,omitempty"`
	FinalRenderError   string    `json:"final_render_error,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

const (
	VideoProjectStatusDraft     = "draft"
	VideoProjectStatusRendering = "rendering"
	VideoProjectStatusRendered  = "rendered"
	VideoProjectStatusFailed    = "failed"
)

// VideoShot is one prompt → one external task → one downloaded MP4.
type VideoShot struct {
	ID            int64      `json:"id"`
	ProjectID     int64      `json:"project_id"`
	Ord           int        `json:"ord"`
	Prompt        string     `json:"prompt"`
	Ratio         string     `json:"ratio"`
	Duration      int        `json:"duration"`
	GenerateAudio bool       `json:"generate_audio"`
	Watermark     bool       `json:"watermark"`
	LLMConfigID   *int64     `json:"llm_config_id,omitempty"`
	TaskID        string     `json:"task_id,omitempty"`
	Status        string     `json:"status"`
	VideoURL      string     `json:"video_url,omitempty"`
	PosterURL     string     `json:"poster_url,omitempty"`
	TrimStartMs   int        `json:"trim_start_ms"`
	TrimEndMs     int        `json:"trim_end_ms"`
	ErrorMessage  string     `json:"error_message,omitempty"`
	SubmittedAt   *time.Time `json:"submitted_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

const (
	VideoShotStatusPending   = "pending"
	VideoShotStatusQueued    = "queued"
	VideoShotStatusRunning   = "running"
	VideoShotStatusSucceeded = "succeeded"
	VideoShotStatusFailed    = "failed"
)

// VideoAsset is a per-project audio attachment: either background music
// or a voiceover.
type VideoAsset struct {
	ID          int64     `json:"id"`
	ProjectID   int64     `json:"project_id"`
	Kind        string    `json:"kind"`
	URL         string    `json:"url"`
	FileName    string    `json:"file_name"`
	MimeType    string    `json:"mime_type"`
	Size        int64     `json:"size"`
	DurationMs  int       `json:"duration_ms"`
	BGMVolume   float64   `json:"bgm_volume"`
	VoiceVolume float64   `json:"voice_volume"`
	CreatedAt   time.Time `json:"created_at"`
}

const (
	VideoAssetKindBGM          = "audio_bgm"
	VideoAssetKindVoiceover    = "voiceover"
	VideoAssetKindCharacterRef = "character_reference"
)

// LLMConfig mirrors dock's LLMConfig row shape (the columns the video
// store/provider actually read). The schema still lives in dock's
// llm_configs table — the extracted video-svc reads it through its DB
// pool (TODO: split into its own polar_video_llm_configs view or move
// to an SDK call in the follow-up PR).
type LLMConfig struct {
	ID           int64           `json:"id"`
	OwnerUserID  string          `json:"owner_user_id"`
	WorkspaceID  string          `json:"workspace_id"`
	ShareID      string          `json:"share_id"`
	Shared       bool            `json:"shared"`
	Name         string          `json:"name"`
	BaseURL      string          `json:"base_url"`
	Model        string          `json:"model"`
	SystemPrompt string          `json:"system_prompt"`
	Streaming    bool            `json:"streaming"`
	HasAPIKey    bool            `json:"has_api_key"`
	ProviderKind string          `json:"provider_kind,omitempty"`
	IsPlatform   bool            `json:"is_platform,omitempty"`
	Extras       []byte          `json:"extras,omitempty"`
	ProxyURL     string          `json:"proxy_url,omitempty"`
	IsSystem     bool            `json:"is_system,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// LLMConfigKind discriminator values for the provider_kind column.
const (
	LLMConfigKindText          = "text"
	LLMConfigKindVideoSeedance = "video.seedance"
)

// systemUserID — sentinel "system" user that owns workspace-agnostic
// rows. Matches dock's constant; kept here to avoid the dock dep.
const systemUserID = "system"

// ---- ffmpeg / sanitization helpers (copied from dock) ----------------

// sanitizeFilename strips path traversal + non-printable bytes from a
// caller-supplied filename. Mirrors dock's helper of the same name.
// Returns "file" if the result is empty.
func sanitizeFilename(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		if r == '/' || r == '\\' || r == 0 {
			continue
		}
		if r < 0x20 {
			continue
		}
		out = append(out, r)
	}
	s := string(out)
	if s == "" || s == "." || s == ".." {
		return "file"
	}
	return s
}

// ---- Cross-domain runtime ops (TODO follow-up) ----------------------

// removeLocalFile is a no-op replacement for dock's helper. The poll
// worker uses it to clean up local intermediate files after upload;
// the extracted video-svc writes to its own BlobDir so cleanup is the
// same os.Remove call. TODO(extract): use os.Remove directly instead
// of going through this shim.
func removeLocalFile(path string) {
	if path == "" {
		return
	}
	_ = osRemove(path)
}

// chatStorageStub is the video-svc upload-store shim. It now uploads
// generated media (shots, posters, source assets) to the central
// assets module via the SDK and returns a permanent /api/media/<id>
// handle that dock serves with a signed provider URL — so video bytes
// flow client↔provider, bypassing both dock and video-svc. The local
// copy in blobDir remains as a transient/fallback.
type chatStorageStub struct {
	blobDir string
	dock    *sdk.Client
}

// Store uploads the file at src into the central assets catalog
// (platform-public) and returns /api/media/<asset_id>. Post-cutover there
// is no /uploads fallback — assets is the single source of truth, so an
// upload failure is a hard error (the caller fails the generation step).
func (cs *chatStorageStub) Store(_ context.Context, src, filename, mimeType string) (string, error) {
	if cs.dock == nil {
		return "", fmt.Errorf("video: asset client unavailable")
	}
	f, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("video: open %s for asset upload: %w", src, err)
	}
	meta, uerr := cs.dock.AssetUpload(sdk.AssetUploadInput{
		Kind: "video", Name: filename, Mime: mimeType, // WorkspaceID nil → platform-public
	}, f)
	f.Close()
	if uerr != nil || meta == nil {
		return "", fmt.Errorf("video: asset upload %s: %w", filename, uerr)
	}
	return "/api/media/" + strconv.FormatInt(meta.ID, 10), nil
}

// chatStorage is a struct field on Plugin (NOT a method) to match the
// call site `p.chatStorage.Store(...)`. Initialized in plugin.New.
//
// (Declared on Plugin via a method-shaped accessor below would mean
// `p.chatStorage().Store(...)`; the moved code uses the field form.)

// requireWorkspaceID extracts the auth-middleware-set workspace_id
// from the gin context. Returns ("", false) and writes 500 if missing,
// mirroring requireUserID. Hoisted here because dock had a shared
// helper of the same name in handler_helpers.go that we can't import.
func requireWorkspaceID(c *gin.Context) (string, bool) {
	v, _ := c.Get(ctxKeyWorkspaceID)
	id, ok := v.(string)
	if !ok || id == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return "", false
	}
	return id, true
}

// osRemove indirection so removeLocalFile can be swapped in tests.
var osRemove = func(path string) error { return nil }
