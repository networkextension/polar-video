package video

// WS broadcast helpers for the video-studio module. These reuse the
// existing wsHub from chat_events.go but emit a separate `type` so the
// chat page's message handler ignores them. The video-studio page
// subscribes via the same WebSocket and switches on `type`.

import (
	"encoding/json"
	"log"
	"time"
)

// videoStudioEvent is the payload shape we send over WS for project /
// shot updates. The chat WS handler returns on unknown types so reusing
// the connection is safe.
type videoStudioEvent struct {
	Type      string         `json:"type"`
	ProjectID int64          `json:"project_id"`
	Kind      string         `json:"kind"`
	Payload   map[string]any `json:"payload"`
	OccurredAt time.Time     `json:"occurred_at"`
}

const videoStudioEventType = "video_project"

const (
	videoStudioEventKindShotStatus   = "shot_status"
	videoStudioEventKindRenderStatus = "render_status"
)

func (p *Plugin) broadcastVideoStudioEvent(ownerUserID string, event videoStudioEvent) {
	if p == nil || ownerUserID == "" {
		return
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now()
	}
	payload, err := json.Marshal(event)
	if err != nil {
		log.Printf("marshal video studio event failed: %v", err)
		return
	}
	// TODO(extract): dock owned the wsHub; until video-svc has its own
	// WS multiplexer or an SDK push channel, log + drop. Frontend will
	// fall back to polling /api/video-projects/:id.
	log.Printf("video: TODO(extract) WS broadcast user=%s len=%d", ownerUserID, len(payload))
}

// broadcastVideoShotEvent fires when a shot's status changes — typically
// queued -> running -> succeeded/failed. The frontend patches the row in
// place rather than re-fetching the whole project.
func (p *Plugin) broadcastVideoShotEvent(project *VideoProject, shotID int64, status, videoURL, errorMessage string) {
	if project == nil {
		return
	}
	payload := map[string]any{
		"shot_id": shotID,
		"status":  status,
	}
	if videoURL != "" {
		payload["video_url"] = videoURL
	}
	if errorMessage != "" {
		payload["error_message"] = errorMessage
	}
	p.broadcastVideoStudioEvent(project.OwnerUserID, videoStudioEvent{
		Type:      videoStudioEventType,
		ProjectID: project.ID,
		Kind:      videoStudioEventKindShotStatus,
		Payload:   payload,
	})
}

// broadcastVideoRenderEvent fires when the final-render status changes —
// rendering / rendered / failed. The frontend swaps the project's status
// pill and reveals the download button on success.
func (p *Plugin) broadcastVideoRenderEvent(project *VideoProject, status, finalURL, errorMessage string) {
	if project == nil {
		return
	}
	payload := map[string]any{
		"status": status,
	}
	if finalURL != "" {
		payload["final_video_url"] = finalURL
	}
	if errorMessage != "" {
		payload["final_render_error"] = errorMessage
	}
	p.broadcastVideoStudioEvent(project.OwnerUserID, videoStudioEvent{
		Type:      videoStudioEventType,
		ProjectID: project.ID,
		Kind:      videoStudioEventKindRenderStatus,
		Payload:   payload,
	})
}
