package video

// Bulk-export handler: stream every succeeded shot of a project as a zip.
// Mode-2 workflow: user pastes a script, hits Submit all, waits, then
// wants ONE click to grab all the rendered shots into CapCut. This
// endpoint serves that need without writing the zip to disk first.

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const videoExportShotDownloadTimeout = 5 * time.Minute

func (p *Plugin) handleVideoProjectShotsZip(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	projectID, ok := parseInt64Param(c, "id")
	if !ok {
		return
	}
	project, err := p.getVideoProject(workspaceID, projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if project == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
		return
	}
	shots, err := p.listVideoShotsForProject(project.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	ready := make([]VideoShot, 0, len(shots))
	for _, sh := range shots {
		if sh.Status == VideoShotStatusSucceeded && sh.VideoURL != "" {
			ready = append(ready, sh)
		}
	}
	if len(ready) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "没有可下载的镜头"})
		return
	}
	sort.Slice(ready, func(i, j int) bool {
		if ready[i].Ord != ready[j].Ord {
			return ready[i].Ord < ready[j].Ord
		}
		return ready[i].ID < ready[j].ID
	})

	filename := zipFilenameForProject(project)
	c.Writer.Header().Set("Content-Type", "application/zip")
	c.Writer.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	zw := zip.NewWriter(c.Writer)
	defer zw.Close()

	// Write a manifest first so users opening the zip see prompt context
	// alongside the mp4 files. UTF-8 BOM helps Windows Notepad render
	// Chinese prompts correctly without the user toggling encoding.
	manifest, err := zw.Create("prompts.txt")
	if err == nil {
		_, _ = manifest.Write([]byte{0xEF, 0xBB, 0xBF})
		_, _ = manifest.Write([]byte(buildPromptsManifest(project, ready)))
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), videoExportShotDownloadTimeout)
	defer cancel()
	for i, sh := range ready {
		entryName := fmt.Sprintf("shot_%03d.mp4", i+1)
		entry, werr := zw.Create(entryName)
		if werr != nil {
			// Headers already sent; nothing useful to surface back to the
			// client beyond the truncated zip. Log and bail.
			fmt.Fprintf(os.Stderr, "video export: zip create %s failed: %v\n", entryName, werr)
			return
		}
		if err := p.streamShotIntoZip(ctx, sh.VideoURL, entry); err != nil {
			fmt.Fprintf(os.Stderr, "video export: stream shot %d failed: %v\n", sh.ID, err)
			return
		}
	}
}

// streamShotIntoZip resolves a stored video URL — local /uploads/<file> or
// remote http(s) — and copies the bytes straight into the zip writer.
// Mirrors the resolver used by the render worker so local + R2 paths work
// identically; we don't import that helper directly because it expects a
// destination file path rather than an io.Writer.
func (p *Plugin) streamShotIntoZip(ctx context.Context, storedURL string, dst io.Writer) error {
	parsed, err := url.Parse(storedURL)
	if err == nil && parsed.Scheme == "" && strings.HasPrefix(parsed.Path, "/uploads/") {
		filename := strings.TrimPrefix(parsed.Path, "/uploads/")
		src, openErr := os.Open(filepath.Join(p.BlobDir, filename))
		if openErr != nil {
			return openErr
		}
		defer src.Close()
		_, err := io.Copy(dst, src)
		return err
	}
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, storedURL, nil)
		if rerr != nil {
			return rerr
		}
		client := &http.Client{Timeout: videoExportShotDownloadTimeout}
		resp, dlErr := client.Do(req)
		if dlErr != nil {
			return dlErr
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("upstream http %d", resp.StatusCode)
		}
		_, err := io.Copy(dst, resp.Body)
		return err
	}
	if !strings.Contains(storedURL, "://") {
		filename := strings.TrimPrefix(storedURL, "/")
		filename = strings.TrimPrefix(filename, "uploads/")
		src, openErr := os.Open(filepath.Join(p.BlobDir, filename))
		if openErr != nil {
			return openErr
		}
		defer src.Close()
		_, err := io.Copy(dst, src)
		return err
	}
	return errors.New("unsupported url scheme")
}

func buildPromptsManifest(project *VideoProject, shots []VideoShot) string {
	var b strings.Builder
	title := strings.TrimSpace(project.Title)
	if title == "" {
		title = fmt.Sprintf("Project %d", project.ID)
	}
	b.WriteString(title)
	b.WriteString("\n")
	b.WriteString(strings.Repeat("=", 60))
	b.WriteString("\n\n")
	for i, sh := range shots {
		fmt.Fprintf(&b, "Shot %03d (id=%d, ratio=%s, duration=%ds):\n", i+1, sh.ID, sh.Ratio, sh.Duration)
		prompt := strings.TrimSpace(sh.Prompt)
		if prompt == "" {
			prompt = "(no prompt)"
		}
		b.WriteString(prompt)
		b.WriteString("\n\n")
	}
	return b.String()
}

// zipFilenameForProject sanitizes the project title into a filename
// browsers will accept across Windows/macOS/Linux. Reserved chars get
// replaced with underscores; the .zip suffix is always appended.
func zipFilenameForProject(project *VideoProject) string {
	name := strings.TrimSpace(project.Title)
	if name == "" {
		name = fmt.Sprintf("project-%d", project.ID)
	}
	replacer := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
	)
	name = replacer.Replace(name)
	if len(name) > 80 {
		name = name[:80]
	}
	return name + ".zip"
}
