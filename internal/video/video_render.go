package video

// Final video rendering. Concatenates a project's succeeded shots in order
// and optionally mixes in a single BGM track and a single voiceover.
// Implementation deliberately leans on ffmpeg's filter_complex rather than
// rolling our own NLE — the goal of this module is "produce edit-ready
// material", not duplicate CapCut. Renders run on a single goroutine
// reading from a buffered channel; concurrent renders would hammer the
// CPU on small VPSes and the user-perceived latency of a sequential
// queue is fine for typical 30-second videos.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// renderQueueSize lets up to N project IDs back up before submission blocks;
// in practice 16 is plenty (rendering is faster than the user can queue).
const renderQueueSize = 16

func (p *Plugin) startVideoRenderWorker(ctx context.Context) {
	if p == nil {
		return
	}
	if p.videoRenderQueue == nil {
		p.videoRenderQueue = make(chan int64, renderQueueSize)
	}
	go p.runVideoRenderWorker(ctx)
}

// enqueueVideoRender hands a project id to the render worker. Non-blocking
// fast path; if the queue is somehow full we surface an error so the
// caller can return 503 instead of silently swallowing the request.
func (p *Plugin) enqueueVideoRender(projectID int64) error {
	if p == nil || p.videoRenderQueue == nil {
		return errors.New("render worker not initialized")
	}
	select {
	case p.videoRenderQueue <- projectID:
		return nil
	default:
		return errors.New("render queue full, please try again shortly")
	}
}

func (p *Plugin) runVideoRenderWorker(ctx context.Context) {
	log.Printf("video render worker started")
	for {
		select {
		case <-ctx.Done():
			return
		case projectID, ok := <-p.videoRenderQueue:
			if !ok {
				return
			}
			if err := p.renderVideoProject(ctx, projectID); err != nil {
				log.Printf("render project %d failed: %v", projectID, err)
				_ = p.updateVideoProjectStatus(projectID, VideoProjectStatusFailed, "", err.Error(), time.Now())
				if project, perr := p.getVideoProjectByID(projectID); perr == nil && project != nil {
					p.broadcastVideoRenderEvent(project, VideoProjectStatusFailed, "", err.Error())
				}
			}
		}
	}
}

func (p *Plugin) renderVideoProject(ctx context.Context, projectID int64) error {
	project, err := p.getVideoProjectByID(projectID)
	if err != nil {
		return err
	}
	if project == nil {
		return errors.New("project not found")
	}
	shots, err := p.listVideoShotsForProject(project.ID)
	if err != nil {
		return err
	}
	ready := make([]VideoShot, 0, len(shots))
	for _, sh := range shots {
		if sh.Status == VideoShotStatusSucceeded && sh.VideoURL != "" {
			ready = append(ready, sh)
		}
	}
	if len(ready) == 0 {
		return errors.New("no completed shots to render")
	}
	assets, err := p.listVideoAssetsForProject(project.ID)
	if err != nil {
		return err
	}
	bgm, voice := pickAudioAssets(assets)

	workDir, err := os.MkdirTemp("", fmt.Sprintf("video-render-%d-", project.ID))
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)

	// Stage the shot videos onto local disk so ffmpeg's concat demuxer can
	// read them by file path. Stored URLs may be remote (R2) or local
	// /uploads/ paths; we resolve both via downloadOrCopyToTemp.
	stagedShotPaths := make([]string, 0, len(ready))
	for i, sh := range ready {
		dst := filepath.Join(workDir, fmt.Sprintf("shot_%03d.mp4", i))
		if err := p.downloadOrCopy(ctx, sh.VideoURL, dst); err != nil {
			return fmt.Errorf("stage shot %d: %w", sh.ID, err)
		}
		stagedShotPaths = append(stagedShotPaths, dst)
	}

	var bgmPath, voicePath string
	var bgmVolume, voiceVolume float64 = 0.3, 1.0
	if bgm != nil {
		bgmPath = filepath.Join(workDir, "bgm"+audioExt(bgm.MimeType))
		if err := p.downloadOrCopy(ctx, bgm.URL, bgmPath); err != nil {
			return fmt.Errorf("stage bgm: %w", err)
		}
		if bgm.BGMVolume > 0 {
			bgmVolume = float64(bgm.BGMVolume)
		}
	}
	if voice != nil {
		voicePath = filepath.Join(workDir, "voice"+audioExt(voice.MimeType))
		if err := p.downloadOrCopy(ctx, voice.URL, voicePath); err != nil {
			return fmt.Errorf("stage voice: %w", err)
		}
		if voice.VoiceVolume > 0 {
			voiceVolume = float64(voice.VoiceVolume)
		}
	}

	output := filepath.Join(workDir, "final.mp4")
	if err := runFFmpegRender(ctx, ready, stagedShotPaths, bgmPath, voicePath, bgmVolume, voiceVolume, output); err != nil {
		return err
	}

	finalName := fmt.Sprintf("video_final_%s_%d_%d.mp4", strings.ReplaceAll(project.OwnerUserID, "/", "_"), project.ID, time.Now().Unix())
	finalDst := filepath.Join(p.BlobDir, finalName)
	if err := os.Rename(output, finalDst); err != nil {
		// Cross-fs rename can fail; fall back to copy.
		if err := copyFile(output, finalDst); err != nil {
			return err
		}
	}
	publicURL, err := p.chatStorage.Store(ctx, finalDst, finalName, "video/mp4")
	if err != nil {
		removeLocalFile(finalDst)
		return err
	}
	now := time.Now()
	if err := p.updateVideoProjectStatus(project.ID, VideoProjectStatusRendered, publicURL, "", now); err != nil {
		return err
	}
	p.broadcastVideoRenderEvent(project, VideoProjectStatusRendered, publicURL, "")
	return nil
}

func pickAudioAssets(assets []VideoAsset) (bgm, voice *VideoAsset) {
	for i := range assets {
		a := assets[i]
		if a.Kind == VideoAssetKindBGM && bgm == nil {
			x := a
			bgm = &x
		} else if a.Kind == VideoAssetKindVoiceover && voice == nil {
			x := a
			voice = &x
		}
	}
	return
}

// runFFmpegRender drives the ffmpeg process. Builds two filter graphs
// independently — video concat is always required; audio is opt-in based
// on what the user actually attached. Reason: Seedance shots can come back
// with no audio track, so a concat that demands a=1 from every input
// fails the whole render. Splitting the graphs lets a silent shot feed
// only its video, while BGM / voice supply audio if present.
func runFFmpegRender(ctx context.Context, shots []VideoShot, shotPaths []string, bgmPath, voicePath string, bgmVolume, voiceVolume float64, output string) error {
	if len(shots) == 0 || len(shots) != len(shotPaths) {
		return errors.New("shots/shot paths mismatch")
	}
	args := []string{"-y"}
	for i, sh := range shots {
		// trim markers are stored in ms; convert to seconds for ffmpeg.
		if sh.TrimStartMs > 0 {
			args = append(args, "-ss", fmt.Sprintf("%.3f", float64(sh.TrimStartMs)/1000.0))
		}
		if sh.TrimEndMs > 0 && sh.TrimEndMs > sh.TrimStartMs {
			args = append(args, "-to", fmt.Sprintf("%.3f", float64(sh.TrimEndMs)/1000.0))
		}
		args = append(args, "-i", shotPaths[i])
	}
	bgmIdx := -1
	voiceIdx := -1
	if bgmPath != "" {
		bgmIdx = len(shots)
		args = append(args, "-i", bgmPath)
	}
	if voicePath != "" {
		if bgmIdx >= 0 {
			voiceIdx = bgmIdx + 1
		} else {
			voiceIdx = len(shots)
		}
		args = append(args, "-i", voicePath)
	}

	// Video graph: concatenate the shots' video streams only. v=1 a=0
	// means we don't require an audio track on every input — we'll
	// build audio separately if the user attached BGM / voice.
	filter := strings.Builder{}
	for i := range shots {
		filter.WriteString(fmt.Sprintf("[%d:v:0]", i))
	}
	filter.WriteString(fmt.Sprintf("concat=n=%d:v=1:a=0[cv];", len(shots)))

	// Audio sub-graph. The video concat duration is the source of truth
	// for the final clip length; audio is decoration. We append `apad`
	// (infinite silence padding) to the audio output and rely on
	// `-shortest` at the output stage to cut off when the video ends.
	// Effects:
	//   BGM longer than video → -shortest clips audio at video end
	//   BGM shorter than video → apad supplies silence to the gap
	//   so the last shot doesn't stretch and the video ends on time.
	finalAudioLabel := ""
	switch {
	case bgmIdx >= 0 && voiceIdx >= 0:
		filter.WriteString(fmt.Sprintf("[%d:a]volume=%.3f[bgm];", bgmIdx, bgmVolume))
		filter.WriteString(fmt.Sprintf("[%d:a]volume=%.3f[voice];", voiceIdx, voiceVolume))
		filter.WriteString("[bgm][voice]amix=inputs=2:duration=longest:dropout_transition=0,apad[mixed];")
		finalAudioLabel = "[mixed]"
	case bgmIdx >= 0:
		filter.WriteString(fmt.Sprintf("[%d:a]volume=%.3f,apad[mixed];", bgmIdx, bgmVolume))
		finalAudioLabel = "[mixed]"
	case voiceIdx >= 0:
		filter.WriteString(fmt.Sprintf("[%d:a]volume=%.3f,apad[mixed];", voiceIdx, voiceVolume))
		finalAudioLabel = "[mixed]"
	}
	// Trim trailing semicolon for cleanliness; ffmpeg tolerates it but logs are easier to read without.
	filterStr := strings.TrimSuffix(filter.String(), ";")

	args = append(args, "-filter_complex", filterStr, "-map", "[cv]")
	if finalAudioLabel != "" {
		args = append(args, "-map", finalAudioLabel,
			"-c:a", "aac",
			"-b:a", "192k",
			// -shortest cuts the output when the shortest mapped stream
			// ends. With apad'd audio that's effectively infinite, the
			// video concat (fixed duration) wins — long BGM is trimmed,
			// short BGM is padded with silence, neither stretches the
			// last shot.
			"-shortest",
		)
	} else {
		// No user-supplied audio — output a silent video. If shots had
		// embedded audio, it's intentionally dropped. This keeps the
		// render predictable instead of randomly inheriting whichever
		// shot's audio happens to exist.
		args = append(args, "-an")
	}
	args = append(args,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "23",
		"-movflags", "+faststart",
		output,
	)

	bin := videoFFmpegBin()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// ffmpeg writes its config banner first, then the actual error at
		// the tail of stderr. The previous head-truncation chopped off
		// the part we needed; tailLines keeps the last ~30 lines so the
		// real diagnostic surfaces.
		log.Printf("ffmpeg full output for failed render:\n%s", string(out))
		log.Printf("ffmpeg filter_complex was: %s", filterStr)
		return fmt.Errorf("ffmpeg failed: %w: %s", err, tailLines(string(out), 30))
	}
	return nil
}

// tailLines returns at most n lines from the end of s. Used for ffmpeg
// error reporting: the first ~50 lines are version + configuration noise,
// while the actionable error sits in the last few lines.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// videoFFmpegBin honors the FFMPEG_BIN env override, otherwise relies on
// PATH lookup so the existing video_processing.go behavior is preserved.
func videoFFmpegBin() string {
	if v := os.Getenv("FFMPEG_BIN"); v != "" {
		return v
	}
	return "ffmpeg"
}

// downloadOrCopy resolves a stored URL into a local file. Local URLs that
// land in /uploads/ are copied directly from disk; remote URLs are
// downloaded over HTTP to keep the render path agnostic to storage
// backend (local disk vs R2).
func (p *Plugin) downloadOrCopy(ctx context.Context, storedURL, dst string) error {
	parsed, err := url.Parse(storedURL)
	if err == nil && parsed.Scheme == "" && strings.HasPrefix(parsed.Path, "/uploads/") {
		// Local /uploads/<file>; reach into the uploadDir.
		filename := strings.TrimPrefix(parsed.Path, "/uploads/")
		src := filepath.Join(p.BlobDir, filename)
		return copyFile(src, dst)
	}
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		return httpDownload(ctx, storedURL, dst)
	}
	// Bare path? assume relative to uploadDir as a last resort.
	if !strings.Contains(storedURL, "://") {
		filename := strings.TrimPrefix(storedURL, "/")
		filename = strings.TrimPrefix(filename, "uploads/")
		return copyFile(filepath.Join(p.BlobDir, filename), dst)
	}
	return fmt.Errorf("unsupported url scheme: %s", storedURL)
}

func httpDownload(ctx context.Context, src, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download http %d", resp.StatusCode)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

// audioExt returns a sensible filename extension for ffmpeg based on the
// recorded mime type. ffmpeg can usually figure it out from content, but
// giving it a hint avoids autodetection failures on edge codecs.
func audioExt(mimeType string) string {
	switch mimeType {
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/aac":
		return ".aac"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/webm":
		return ".webm"
	case "audio/mp4", "audio/x-m4a":
		return ".m4a"
	case "audio/ogg":
		return ".ogg"
	}
	return ".bin"
}
