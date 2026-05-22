// Package video is the video-studio (Seedance + ffmpeg) plugin.
//
// Phase 2-W1 video-skeleton ships the architecture, not the handlers.
// Dock still owns the /api/video/* + video poll worker; video-svc
// stands up alongside, owns polar_video + its own blob root.
//
// Next PR moves the 3K LOC across (video_studio.go, video_render.go,
// video_seedance.go, video_export.go, video_store.go, video_provider.go,
// video_processing.go, video_events.go, video_poll_worker.go),
// mirroring Phase 1-D-1's wg move.
//
// Plugin owns:
//   - DB pool to polar_video
//   - Dock SDK client (cross-domain user/team/llm-config lookups)
//   - Local blob root: $POLAR_VIDEO_BLOB_DIR/{shots,exports,bgm,voice}
//   - ffmpeg / Seedance credentials are env-driven (handler PR wires)
//   - Heartbeat goroutine (60s)
package video

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/networkextension/polar-sdk"
)

type Plugin struct {
	DB         *sql.DB
	Dock       *sdk.Client
	Name       string
	Listen     string
	Ver        string
	BlobDir    string
	MetricsTok string

	// SystemWorkspaceID is dock's "system" user's personal team. Used by
	// the seedance auto-bootstrap path that ensures a default
	// video.seedance LLM config exists.
	SystemWorkspaceID string

	// Seedance defaults — env-driven, used by the provider/poll worker.
	videoSeedanceBaseURL string
	videoSeedanceModel   string
	videoSeedanceAPIKey  string
	videoPollIntervalSec int
	videoProvider        *videoProviderClient
	videoProviderCtx     context.Context
	videoRenderQueue     chan int64

	// chatStorage is the upload-store shim — see stubs.go::chatStorageStub.
	// In dock this was the polar-attachment client; the extracted svc
	// uses a local-blobdir stub until polar-attachment is its own plugin.
	chatStorage *chatStorageStub

	metrics   *videoMetrics
	startedAt time.Time
}

type Config struct {
	DBDSN        string
	DockBase     string
	PluginName   string
	PluginToken  string
	Listen       string
	BuildVersion string
	BlobDir      string
	MetricsToken string

	SeedanceBaseURL     string
	SeedanceModel       string
	SeedanceAPIKey      string
	PollIntervalSeconds int
}

func New(ctx context.Context, cfg Config) (*Plugin, error) {
	cfg.PluginName = strings.TrimSpace(cfg.PluginName)
	if cfg.PluginName == "" {
		cfg.PluginName = "video"
	}
	if strings.TrimSpace(cfg.DBDSN) == "" {
		return nil, errors.New("video.New: DBDSN required")
	}
	if strings.TrimSpace(cfg.DockBase) == "" {
		return nil, errors.New("video.New: DockBase required")
	}
	if strings.TrimSpace(cfg.PluginToken) == "" {
		return nil, errors.New("video.New: PluginToken required (plaintext from /admin-plugins.html)")
	}
	if strings.TrimSpace(cfg.BlobDir) == "" {
		return nil, errors.New("video.New: BlobDir required (video-svc-local data root)")
	}

	db, err := sql.Open("postgres", cfg.DBDSN)
	if err != nil {
		return nil, fmt.Errorf("open polar_video: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping polar_video: %w", err)
	}

	dock := sdk.NewClient(cfg.DockBase, cfg.PluginName, sdk.DeriveHMACKey(cfg.PluginToken))
	resp, err := dock.Do(http.MethodGet, "/internal/v1/ping", nil)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("dock ping: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = db.Close()
		return nil, fmt.Errorf("dock /ping rejected: HTTP %d (check PLUGIN_TOKEN + plugin_modules row)", resp.StatusCode)
	}

	// Pre-fetch system workspace ID — mirrors projects/packtunnel pattern.
	sysWS, sysErr := dock.Do(http.MethodGet, "/internal/v1/users/system/workspace", nil)
	systemWorkspaceID := ""
	if sysErr == nil {
		var body struct {
			WorkspaceID string `json:"workspace_id"`
		}
		if err := readJSON(sysWS, &body); err == nil {
			systemWorkspaceID = body.WorkspaceID
		}
	}
	if systemWorkspaceID == "" {
		log.Printf("video: WARN system workspace lookup failed; seedance auto-bootstrap may skip (err=%v)", sysErr)
	}

	pollSec := cfg.PollIntervalSeconds
	if pollSec <= 0 {
		pollSec = 10
	}

	return &Plugin{
		DB:                   db,
		Dock:                 dock,
		Name:                 cfg.PluginName,
		Listen:               cfg.Listen,
		Ver:                  cfg.BuildVersion,
		BlobDir:              cfg.BlobDir,
		MetricsTok:           cfg.MetricsToken,
		SystemWorkspaceID:    systemWorkspaceID,
		videoSeedanceBaseURL: cfg.SeedanceBaseURL,
		videoSeedanceModel:   cfg.SeedanceModel,
		videoSeedanceAPIKey:  cfg.SeedanceAPIKey,
		videoPollIntervalSec: pollSec,
		videoProvider:        newVideoProviderClient(),
		videoRenderQueue:     make(chan int64, 16),
		chatStorage:          &chatStorageStub{blobDir: cfg.BlobDir},
		metrics:              newVideoMetrics(),
		startedAt:            time.Now(),
	}, nil
}

// readJSON — local helper; SDK's equivalent is unexported.
func readJSON(resp *http.Response, out any) error {
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// RegisterRoutes wires /api/video-* under a Bearer-auth group that
// matches dock's AuthMiddleware contract (sets user_id / user_role /
// workspace_id on the gin context).
func (p *Plugin) RegisterRoutes(r gin.IRouter) {
	r.GET("/healthz", p.handleHealthz)
	r.GET("/metrics", p.handleMetricsExposition)

	api := r.Group("/api", p.requireAuthViaDock())
	{
		api.GET("/video-llm-configs", p.handleVideoLLMConfigList)
		api.GET("/video-projects", p.handleVideoProjectList)
		api.POST("/video-projects", p.handleVideoProjectCreate)
		api.GET("/video-projects/:id", p.handleVideoProjectDetail)
		api.PATCH("/video-projects/:id", p.handleVideoProjectUpdate)
		api.DELETE("/video-projects/:id", p.handleVideoProjectDelete)
		api.POST("/video-projects/:id/shots", p.handleVideoShotCreate)
		api.PATCH("/video-projects/:id/shots/:shotId", p.handleVideoShotUpdate)
		api.DELETE("/video-projects/:id/shots/:shotId", p.handleVideoShotDelete)
		api.POST("/video-projects/:id/shots/:shotId/submit", p.handleVideoShotSubmit)
		api.POST("/video-projects/:id/shots/:shotId/retry", p.handleVideoShotRetry)
		api.POST("/video-projects/:id/shots/:shotId/duplicate", p.handleVideoShotDuplicate)
		api.POST("/video-projects/:id/shots/:shotId/extract-frame", p.handleVideoShotExtractFrame)
		api.POST("/video-projects/:id/submit-all", p.handleVideoProjectSubmitAll)
		api.POST("/video-projects/:id/assets", p.handleVideoAssetUpload)
		api.PATCH("/video-projects/:id/assets/:assetId", p.handleVideoAssetUpdate)
		api.DELETE("/video-projects/:id/assets/:assetId", p.handleVideoAssetDelete)
		api.POST("/video-projects/:id/render", p.handleVideoProjectRender)
		api.GET("/video-projects/:id/download", p.handleVideoProjectDownload)
		api.GET("/video-projects/:id/shots.zip", p.handleVideoProjectShotsZip)
	}
}

func (p *Plugin) Start(ctx context.Context) {
	p.videoProviderCtx = ctx
	go p.heartbeatLoop(ctx)
	go p.runVideoPollWorker(ctx)
	go p.runVideoRenderWorker(ctx)
}

func (p *Plugin) Close() error {
	if p.DB != nil {
		return p.DB.Close()
	}
	return nil
}

func (p *Plugin) handleHealthz(c *gin.Context) {
	dbOK := true
	if err := p.DB.PingContext(c.Request.Context()); err != nil {
		dbOK = false
	}
	status := http.StatusOK
	if !dbOK {
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, gin.H{
		"plugin":         p.Name,
		"version":        p.Ver,
		"uptime_seconds": int64(time.Since(p.startedAt).Seconds()),
		"db_ok":          dbOK,
		"blob_dir":       p.BlobDir,
		"go":             runtime.Version(),
	})
}

func (p *Plugin) handleMetricsExposition(c *gin.Context) {
	if p.MetricsTok == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if c.GetHeader("Authorization") != "Bearer "+p.MetricsTok {
		c.Header("WWW-Authenticate", `Bearer realm="metrics"`)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	promhttp.HandlerFor(p.metrics.registry, promhttp.HandlerOpts{}).ServeHTTP(c.Writer, c.Request)
}

func (p *Plugin) heartbeatLoop(ctx context.Context) {
	p.beat(ctx)
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.beat(ctx)
		}
	}
}

func (p *Plugin) beat(_ context.Context) {
	err := p.Dock.Heartbeat(sdk.HeartbeatOpts{
		Version:       p.Ver,
		Endpoint:      p.Listen,
		UptimeSeconds: int64(time.Since(p.startedAt).Seconds()),
	})
	if err != nil {
		log.Printf("video: heartbeat failed: %v", err)
	}
}

// ── metrics — placeholder; handler PR adds shot/export counters ────

type videoMetrics struct {
	registry *prometheus.Registry
	upGauge  prometheus.Gauge
}

func newVideoMetrics() *videoMetrics {
	m := &videoMetrics{registry: prometheus.NewRegistry()}
	m.upGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "polar_video_up",
		Help: "Always 1 while video-svc is serving. Phase 2-W1 placeholder.",
	})
	m.registry.MustRegister(m.upGauge)
	m.upGauge.Set(1)
	return m
}
