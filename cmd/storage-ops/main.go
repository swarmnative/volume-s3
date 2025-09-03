package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/swarmnative/swarm-s3-mounter/internal/controller"
)

func main() {
	// init JSON slog
	level := parseLogLevel(getenv("S3_MOUNTER_LOG_LEVEL", "info"))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	cfg := controller.Config{
		MinioEndpointsCSV:   getenv("S3_MOUNTER_S3_ENDPOINTS", "http://s3.local:9000"),
		S3Provider:          getenv("S3_MOUNTER_S3_PROVIDER", ""),
		S3Endpoint:          getenv("S3_MOUNTER_S3_ENDPOINT", "http://s3.local:9000"),
		RcloneRemote:        getenv("S3_MOUNTER_RCLONE_REMOTE", "S3:bucket"),
		RcloneExtraArgs:     getenv("S3_MOUNTER_RCLONE_ARGS", ""),
		Mountpoint:          getenv("S3_MOUNTER_MOUNTPOINT", "/mnt/s3"),
		AccessKeyFile:       getenv("S3_MOUNTER_S3_ACCESS_KEY_FILE", "/run/secrets/s3_access_key"),
		SecretKeyFile:       getenv("S3_MOUNTER_S3_SECRET_KEY_FILE", "/run/secrets/s3_secret_key"),
		MounterImage:        getenv("S3_MOUNTER_MOUNTER_IMAGE", getenv("S3_MOUNTER_DEFAULT_MOUNTER_IMAGE", "rclone/rclone:latest")),
		HelperImage:         getenv("S3_MOUNTER_NSENTER_HELPER_IMAGE", "alpine:3.20"),
		ReadyFile:           ".ready",
		PollInterval:        15 * time.Second,
		MounterUpdateMode:   getenv("S3_MOUNTER_MOUNTER_UPDATE_MODE", defaultUpdateMode()),
		MounterPullInterval: parseDurationOr("24h"),
		UnmountOnExit:       getenv("S3_MOUNTER_UNMOUNT_ON_EXIT", "true") == "true",
		AutoCreateBucket:    getenv("S3_MOUNTER_AUTOCREATE_BUCKET", "false") == "true",
		AutoCreatePrefix:    getenv("S3_MOUNTER_AUTOCREATE_PREFIX", "false") == "true",
		EnableProxy:         getenv("S3_MOUNTER_ENABLE_PROXY", "false") == "true",
		LocalLBEnabled:      getenv("S3_MOUNTER_LOCAL_LB", "false") == "true",
		ProxyPort:           getenv("S3_MOUNTER_PROXY_PORT", "8081"),
		ProxyNetwork:        getenv("S3_MOUNTER_PROXY_NETWORK", ""),
		LabelPrefix:         getenv("S3_MOUNTER_LABEL_PREFIX", getenv("LABEL_PREFIX", "")),
		LabelStrict:         getenv("S3_MOUNTER_LABEL_STRICT", "false") == "true",
		ImageCleanupEnabled: getenv("S3_MOUNTER_IMAGE_CLEANUP_ENABLED", "true") == "true",
		ImageRetentionDays:  getenvInt("S3_MOUNTER_IMAGE_RETENTION_DAYS", 14),
		ImageKeepRecent:     getenvInt("S3_MOUNTER_IMAGE_KEEP_RECENT", 2),
	}

	// --validate-config fast path
	if hasArg("--validate-config") {
		vr := controller.ValidateConfig(cfg)
		_ = json.NewEncoder(os.Stdout).Encode(vr)
		if vr.OK {
			os.Exit(0)
		} else {
			os.Exit(1)
		}
	}

	// effective config summary (masked)
	vr := controller.ValidateConfig(cfg)
	slog.Info("effective_config", slog.String("summary", mustJSON(vr.Summary)))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ctrl, err := controller.New(ctx, cfg)
	if err != nil {
		slog.Error("init controller", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if ctrl.Ready() == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	if getenv("S3_MOUNTER_ENABLE_METRICS", "false") == "true" {
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			// minimal text exposition
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			s := ctrl.Snapshot()
			_, _ = w.Write([]byte(
				"# HELP s3mounter_reconcile_total Total reconcile loops\n" +
					"# TYPE s3mounter_reconcile_total counter\n" +
					"s3mounter_reconcile_total " + itoa(s.ReconcileTotal) + "\n" +
					"# HELP s3mounter_reconcile_errors Total reconcile errors\n" +
					"# TYPE s3mounter_reconcile_errors counter\n" +
					"s3mounter_reconcile_errors " + itoa(s.ReconcileErrors) + "\n" +
					"# HELP s3mounter_mounter_running Whether rclone mounter is running\n" +
					"# TYPE s3mounter_mounter_running gauge\n" +
					"s3mounter_mounter_running " + bool01(s.MounterRunning) + "\n" +
					"# HELP s3mounter_mount_writable Whether mountpoint is writable\n" +
					"# TYPE s3mounter_mount_writable gauge\n" +
					"s3mounter_mount_writable " + bool01(s.MountWritable) + "\n" +
					"# HELP s3mounter_heal_attempts_total Total heal attempts\n" +
					"# TYPE s3mounter_heal_attempts_total counter\n" +
					"s3mounter_heal_attempts_total " + itoa(s.HealAttemptsTotal) + "\n" +
					"# HELP s3mounter_heal_success_total Total heal success\n" +
					"# TYPE s3mounter_heal_success_total counter\n" +
					"s3mounter_heal_success_total " + itoa(s.HealSuccessTotal) + "\n" +
					"# HELP s3mounter_last_heal_success_timestamp Seconds since epoch of last heal success\n" +
					"# TYPE s3mounter_last_heal_success_timestamp gauge\n" +
					"s3mounter_last_heal_success_timestamp " + itoa(s.LastHealSuccessUnix) + "\n" +
					"# HELP s3mounter_orphan_cleanup_total Total orphaned mounters cleaned\n" +
					"# TYPE s3mounter_orphan_cleanup_total counter\n" +
					"s3mounter_orphan_cleanup_total " + itoa(s.OrphanCleanupTotal) + "\n"))
		})
	}
	mux.HandleFunc("/validate", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(controller.ValidateConfig(cfg))
	})

	srv := &http.Server{Addr: ":8080", Handler: mux}
	go func() {
		slog.Info("http listening", slog.String("addr", ":8080"))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server", "error", err)
			os.Exit(1)
		}
	}()

	go ctrl.Run()

	<-ctx.Done()
	_ = srv.Shutdown(context.Background())
	// best-effort cleanup
	ctrl.Cleanup()
}

func getenv(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
func bool01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func getenvInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}

func hasArg(flag string) bool {
	for _, a := range os.Args[1:] {
		if a == flag {
			return true
		}
	}
	return false
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func parseDurationOr(def string) time.Duration {
	v := os.Getenv("S3_MOUNTER_MOUNTER_PULL_INTERVAL")
	if v == "" {
		d, _ := time.ParseDuration(def)
		return d
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	d, _ := time.ParseDuration(def)
	return d
}

func defaultUpdateMode() string {
	// default to never for stability; user can enable periodic/on_change
	if v := os.Getenv("S3_MOUNTER_MOUNTER_UPDATE_MODE"); v != "" {
		return v
	}
	return "never"
}
