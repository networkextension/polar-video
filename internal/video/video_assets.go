package video

// Assets migration (doc/arch/blob-storage-to-assets-migration.md in
// polar-dock). chatStorageStub already uploads to the assets catalog and
// returns /api/media/<id>, but falls back to /uploads/<file> when an
// AssetUpload hiccups (the render/poll pipeline is long + expensive, so a
// transient assets failure must not kill a job). This boot backfill
// drains any /uploads stragglers into assets and rewrites the DB url, so
// in steady state every blob lives in the central catalog.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	sdk "github.com/networkextension/polar-sdk"
)

func videoRandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func videoMimeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a", ".aac":
		return "audio/aac"
	case ".wav":
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

// uploadVideoBlobToAssets uploads a local /uploads file into the assets
// catalog and returns the "/api/media/<id>" url (platform-owned, matching
// the chatStorage convention).
func (p *Plugin) uploadVideoBlobToAssets(localURL string) (string, error) {
	name := strings.TrimPrefix(localURL, "/uploads/")
	abs := filepath.Join(p.BlobDir, name)
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	ext := filepath.Ext(name)
	meta, err := p.Dock.AssetUpload(sdk.AssetUploadInput{
		Kind:    "video",
		Name:    "video/" + videoRandHex(6) + ext,
		Version: "v1",
		Mime:    videoMimeForExt(ext),
	}, f)
	if err != nil {
		return "", err
	}
	return "/api/media/" + strconv.FormatInt(meta.ID, 10), nil
}

// backfillVideoAssetsOnce rewrites any /uploads/* blob urls across the
// video tables to assets-backed /api/media/<id>. Idempotent goroutine
// from Start().
func (p *Plugin) backfillVideoAssetsOnce(ctx context.Context) {
	cols := []struct{ table, col, id string }{
		{"video_shots", "video_url", "id"},
		{"video_shots", "poster_url", "id"},
		{"video_projects", "final_video_url", "id"},
		{"video_assets", "url", "id"},
	}
	total := 0
	for _, c := range cols {
		total += p.backfillVideoColumn(ctx, c.table, c.col, c.id)
	}
	if total > 0 {
		log.Printf("video: asset backfill: migrated %d blob(s) to assets", total)
	}
}

func (p *Plugin) backfillVideoColumn(ctx context.Context, table, col, idcol string) int {
	rows, err := p.DB.QueryContext(ctx,
		`SELECT `+idcol+`, `+col+` FROM `+table+` WHERE `+col+` LIKE '/uploads/%'`)
	if err != nil {
		log.Printf("video: asset backfill: %s.%s query: %v", table, col, err)
		return 0
	}
	type row struct {
		id  int64
		url string
	}
	var pending []row
	for rows.Next() {
		var r row
		var u sql.NullString
		if err := rows.Scan(&r.id, &u); err != nil {
			continue
		}
		r.url = u.String
		pending = append(pending, r)
	}
	rows.Close()

	migrated := 0
	for _, r := range pending {
		newURL, err := p.uploadVideoBlobToAssets(r.url)
		if err != nil {
			log.Printf("video: asset backfill: %s.%s id=%d: %v (skip)", table, col, r.id, err)
			continue
		}
		if _, err := p.DB.ExecContext(ctx,
			`UPDATE `+table+` SET `+col+` = $2 WHERE `+idcol+` = $1`, r.id, newURL); err != nil {
			log.Printf("video: asset backfill: %s.%s id=%d update: %v", table, col, r.id, err)
			continue
		}
		migrated++
		log.Printf("video: asset backfill: %s.%s id=%d -> %s", table, col, r.id, newURL)
	}
	return migrated
}
