package video

// Assets resolution (doc/arch/blob-storage-to-assets-migration.md in
// polar-dock). Generated media (shots, posters, renders, source assets)
// lives in the central polar-assets catalog; DB columns store the
// "/api/media/<id>" handle. This file resolves that handle (+ remote
// http(s) source URLs) to a byte stream for the render / export / studio
// read paths. The transitional /uploads-fallback + boot backfill were
// removed at cutover — assets is the single source of truth.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdk "github.com/networkextension/polar-sdk"
)

// parseMediaRef extracts the catalog asset id from a stored
// "/api/media/<id>" URL. (0,false) for any other form.
func parseMediaRef(s string) (int64, bool) {
	const pfx = "/api/media/"
	if !strings.HasPrefix(s, pfx) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(s, pfx), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// openStoredBlob resolves a stored video URL to a byte stream the caller
// must Close. Two forms, post-cutover:
//   - "/api/media/<id>" — central assets catalog (internal AssetDownload)
//   - http(s)://        — remote provider / source URL
// Legacy "/uploads/<file>" local reads were removed at cutover (rows are
// migrated to /api/media/<id> before deploy).
func (p *Plugin) openStoredBlob(ctx context.Context, storedURL string) (io.ReadCloser, error) {
	if id, ok := parseMediaRef(storedURL); ok {
		resp, err := p.Dock.AssetDownload(&sdk.AssetMeta{ID: id})
		if err != nil {
			return nil, err
		}
		if resp.StatusCode/100 != 2 {
			resp.Body.Close()
			return nil, fmt.Errorf("asset %d download: HTTP %d", id, resp.StatusCode)
		}
		return resp.Body, nil
	}
	if strings.HasPrefix(storedURL, "http://") || strings.HasPrefix(storedURL, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, storedURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, fmt.Errorf("download %q: HTTP %d", storedURL, resp.StatusCode)
		}
		return resp.Body, nil
	}
	return nil, fmt.Errorf("unsupported video url: %q", storedURL)
}
