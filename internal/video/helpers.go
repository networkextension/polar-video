package video

// helpers.go — small functions copied from dock that the moved
// handlers depend on. Kept here so video-svc has no compile-time
// dependency on the dock package.

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// generateSessionID — random base64-URL token. Copied from dock's
// store.go::generateSessionID (32 random bytes). Used by
// buildUploadFilename so stored filenames are unique even on filename
// collision.
func generateSessionID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}

// buildUploadFilename mirrors dock's handler_helpers.go function:
// timestamp + 8-char random + lower-case extension. Validates the
// extension is ASCII alnum or falls back to .img.
func buildUploadFilename(original string) string {
	ext := strings.ToLower(filepath.Ext(original))
	if ext == "" || len(ext) > 8 {
		ext = ".img"
	} else {
		for _, r := range ext[1:] {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				ext = ".img"
				break
			}
		}
	}
	return fmt.Sprintf("%s_%s%s", time.Now().Format("20060102_150405"), generateSessionID()[:8], ext)
}
