// Command video-svc is the video-studio (Seedance + ffmpeg) plugin
// binary. Phase 2-W1 skeleton — DB pool + dock heartbeat + /healthz.
// Next PR moves the /api/video/* handlers + poll worker.
//
// Env vars:
//
//	POLAR_VIDEO_DB_DSN        postgres://…/polar_video?sslmode=disable
//	POLAR_DOCK_BASE           http://127.0.0.1:8080
//	POLAR_PLUGIN_NAME         video
//	POLAR_PLUGIN_TOKEN        polar_plugin_…
//	POLAR_VIDEO_LISTEN        127.0.0.1:8092   (8090=wg, 8091=iosdist)
//	POLAR_VIDEO_VERSION       git sha
//	POLAR_VIDEO_BLOB_DIR      /Users/local/video-svc-data
//	POLAR_VIDEO_METRICS_TOKEN bearer for /metrics; unset = 404
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"

	"github.com/networkextension/polar-video/internal/video"
)

func main() {
	cfg := video.Config{
		DBDSN:           envOrDefault("POLAR_VIDEO_DB_DSN", "postgres://ideamesh:test123456@127.0.0.1:5432/polar_video?sslmode=disable"),
		DockBase:        envOrDefault("POLAR_DOCK_BASE", "http://127.0.0.1:8080"),
		PluginName:      envOrDefault("POLAR_PLUGIN_NAME", "video"),
		PluginToken:     os.Getenv("POLAR_PLUGIN_TOKEN"),
		Listen:          envOrDefault("POLAR_VIDEO_LISTEN", "127.0.0.1:8092"),
		BuildVersion:    envOrDefault("POLAR_VIDEO_VERSION", "0.0.1"),
		BlobDir:         envOrDefault("POLAR_VIDEO_BLOB_DIR", "/Users/local/video-svc-data"),
		MetricsToken:    os.Getenv("POLAR_VIDEO_METRICS_TOKEN"),
		SeedanceBaseURL: os.Getenv("VIDEO_SEEDANCE_BASE_URL"),
		SeedanceModel:   os.Getenv("VIDEO_SEEDANCE_MODEL"),
		SeedanceAPIKey:  os.Getenv("VIDEO_SEEDANCE_API_KEY"),
	}
	if strings.TrimSpace(cfg.PluginToken) == "" {
		log.Fatal("POLAR_PLUGIN_TOKEN unset — get plaintext from /admin-plugins.html (one-time print at row creation)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	plugin, err := video.New(ctx, cfg)
	if err != nil {
		log.Fatalf("video.New: %v", err)
	}
	defer plugin.Close()

	gin.SetMode(envOrDefault("GIN_MODE", gin.ReleaseMode))
	r := gin.New()
	r.Use(gin.Recovery())
	plugin.RegisterRoutes(r)
	plugin.Start(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("video-svc listening on %s (dock=%s, name=%s, ver=%s, blob=%s)",
			cfg.Listen, cfg.DockBase, cfg.PluginName, cfg.BuildVersion, cfg.BlobDir)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("video-svc: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("video-svc: shutdown: %v", err)
	}
}

func envOrDefault(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}
