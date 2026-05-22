package video

// Background worker that polls in-flight Seedance (and future-provider) tasks
// and downloads finished MP4s into the existing Storage interface (local or
// R2). Modeled on push_worker.go's ticker pattern: claim a batch from the DB,
// process them serially, log on failure, never crash the loop.

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runVideoPollWorker is started once at server boot. It polls every
// VideoPollIntervalSeconds (default 10) and processes up to
// videoPollBatchSize shots per tick.
const videoPollBatchSize = 32

func (p *Plugin) runVideoPollWorker(ctx context.Context) {
	if p == nil {
		return
	}
	interval := time.Duration(p.videoPollIntervalSec) * time.Second
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("video poll worker started (interval=%s)", interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.processInflightVideoShots(ctx); err != nil {
				log.Printf("video poll batch failed: %v", err)
			}
		}
	}
}

func (p *Plugin) processInflightVideoShots(ctx context.Context) error {
	shots, err := p.listInflightVideoShots(ctx, videoPollBatchSize)
	if err != nil {
		return err
	}
	for i := range shots {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		shot := shots[i]
		if err := p.pollOneVideoShot(ctx, &shot); err != nil {
			log.Printf("poll shot %d (project=%d) failed: %v", shot.ID, shot.ProjectID, err)
		}
	}
	return nil
}

func (p *Plugin) pollOneVideoShot(ctx context.Context, shot *VideoShot) error {
	project, err := p.getVideoProjectByID(shot.ProjectID)
	if err != nil {
		return err
	}
	if project == nil {
		// Project was deleted while the task was in flight; orphan the row
		// (it'll be CASCADE'd by the FK once the project DELETE lands).
		return nil
	}
	cfgID := shot.LLMConfigID
	if cfgID == nil {
		cfgID = project.DefaultLLMConfigID
	}
	if cfgID == nil || *cfgID <= 0 {
		// Shouldn't happen in practice — submission validates this — but be
		// defensive so a corrupted row doesn't churn forever.
		return p.markVideoShotStatus(shot.ID, VideoShotStatusFailed, "", "shot has no llm_config_id", time.Now())
	}
	cfg, apiKey, err := p.getVideoLLMConfigWithAPIKey(project.WorkspaceID, *cfgID)
	if err != nil {
		return err
	}
	if cfg == nil {
		return p.markVideoShotStatus(shot.ID, VideoShotStatusFailed, "", "video config no longer accessible", time.Now())
	}
	status, videoURL, errorMessage, perr := p.videoProvider.pollVideoTask(ctx, cfg, apiKey, shot.TaskID)
	if perr != nil {
		// Treat hard provider errors as a transient blip (don't flip the
		// row to failed). Next tick will try again. Surface in logs.
		return perr
	}
	now := time.Now()
	if status == VideoShotStatusSucceeded {
		// Download the MP4 once, then hand to Storage. We never re-download:
		// once stored we own the URL and the upstream URL can expire.
		filename := videoShotFilename(project.OwnerUserID, project.ID, shot.ID)
		stored, downloadErr := p.downloadAndStoreVideo(ctx, videoURL, filename)
		if downloadErr != nil {
			return p.markVideoShotStatus(shot.ID, VideoShotStatusFailed, "", "download failed: "+downloadErr.Error(), now)
		}
		if err := p.markVideoShotStatus(shot.ID, VideoShotStatusSucceeded, stored, "", now); err != nil {
			return err
		}
		// Cache a poster (first-frame jpg) so the project page can show
		// thumbnails without browsers having to range-request the MP4.
		// Failure is non-fatal — the frontend just falls back to native
		// preview behavior.
		if posterURL, perr := p.generateAndStoreShotPoster(ctx, project, shot.ID); perr != nil {
			log.Printf("poster generation skipped for shot %d: %v", shot.ID, perr)
		} else if posterURL != "" {
			_ = p.setVideoShotPoster(shot.ID, posterURL, time.Now())
		}
		p.broadcastVideoShotEvent(project, shot.ID, VideoShotStatusSucceeded, stored, "")
		return nil
	}
	if status == VideoShotStatusFailed {
		if err := p.markVideoShotStatus(shot.ID, VideoShotStatusFailed, "", errorMessage, now); err != nil {
			return err
		}
		p.broadcastVideoShotEvent(project, shot.ID, VideoShotStatusFailed, "", errorMessage)
		return nil
	}
	// Still queued / running — only update if the status changed so we don't
	// touch updated_at on every tick (cheap optimization).
	if status != shot.Status {
		if err := p.markVideoShotStatus(shot.ID, status, "", "", now); err != nil {
			return err
		}
		p.broadcastVideoShotEvent(project, shot.ID, status, "", "")
	}
	return nil
}

// videoShotFilename builds a stable, collision-free name we can stash in
// either local or R2 storage. Includes the user, project, and shot ids so
// tracing back from a stored URL is mechanical.
func videoShotFilename(ownerID string, projectID, shotID int64) string {
	safe := strings.ReplaceAll(ownerID, "/", "_")
	return "video_shot_" + safe + "_" + itoa64(projectID) + "_" + itoa64(shotID) + ".mp4"
}

// generateAndStoreShotPoster grabs the first ~half-second frame of the
// already-stored shot video, hands the JPG off to the chatStorage
// interface (so local /uploads + R2 both work), and returns the public
// URL. Used as the <video poster="..."> on the studio page so opening
// a 10-shot project doesn't trigger 10 range-requests against the MP4s.
func (p *Plugin) generateAndStoreShotPoster(ctx context.Context, project *VideoProject, shotID int64) (string, error) {
	if p.BlobDir == "" {
		return "", errors.New("upload dir not configured")
	}
	srcName := videoShotFilename(project.OwnerUserID, project.ID, shotID)
	srcPath := filepath.Join(p.BlobDir, srcName)
	// downloadAndStoreVideo always lays the mp4 down at uploadDir before
	// chatStorage.Store, so reading from there is safe whether the final
	// URL is local or R2.
	if _, err := os.Stat(srcPath); err != nil {
		return "", err
	}
	posterName := strings.TrimSuffix(srcName, ".mp4") + "_poster.jpg"
	posterDst := filepath.Join(p.BlobDir, posterName)
	if err := generateVideoPoster(ctx, srcPath, posterDst); err != nil {
		return "", err
	}
	publicURL, err := p.chatStorage.Store(ctx, posterDst, posterName, "image/jpeg")
	if err != nil {
		removeLocalFile(posterDst)
		return "", err
	}
	return publicURL, nil
}

func itoa64(n int64) string {
	if n < 0 {
		return "-" + itoa(int(-n))
	}
	return itoa(int(n))
}

// downloadAndStoreVideo grabs the upstream MP4 to a temp file, then hands
// it off to chatStorage.Store (which copies to local /uploads or R2).
// Returns the public URL.
func (p *Plugin) downloadAndStoreVideo(ctx context.Context, upstreamURL, filename string) (string, error) {
	if p.BlobDir == "" {
		return "", errors.New("upload dir not configured")
	}
	if err := os.MkdirAll(p.BlobDir, 0o755); err != nil {
		return "", err
	}
	tmp, _, err := downloadVideoToTemp(ctx, &http.Client{Timeout: 5 * time.Minute}, upstreamURL, ".mp4")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	dst := filepath.Join(p.BlobDir, filename)
	// Move/copy temp -> uploadDir so chatStorage.Store sees a stable path.
	if err := os.Rename(tmp, dst); err != nil {
		// Cross-filesystem rename can fail; fall back to copy + remove.
		if err := copyFile(tmp, dst); err != nil {
			return "", err
		}
	}
	publicURL, err := p.chatStorage.Store(ctx, dst, filename, "video/mp4")
	if err != nil {
		removeLocalFile(dst)
		return "", err
	}
	return publicURL, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}
