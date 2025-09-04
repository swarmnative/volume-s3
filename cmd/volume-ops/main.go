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

	"github.com/swarmnative/volume-s3/internal/controller"
)

func main() {
	// init JSON slog
	level := parseLogLevel(getenv("VOLS3_LOG_LEVEL", "info"))
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	cfg := controller.Config{
		MinioEndpointsCSV:   getenv("VOLS3_ENDPOINTS", "http://s3.local:9000"),
		S3Provider:          getenv("VOLS3_PROVIDER", ""),
		S3Endpoint:          getenv("VOLS3_ENDPOINT", "http://s3.local:9000"),
		RcloneRemote:        getenv("VOLS3_RCLONE_REMOTE", "S3:bucket"),
		RcloneExtraArgs:     getenv("VOLS3_RCLONE_ARGS", ""),
		Mountpoint:          getenv("VOLS3_MOUNTPOINT", "/mnt/s3"),
		AccessKeyFile:       getenv("VOLS3_ACCESS_KEY_FILE", "/run/secrets/s3_access_key"),
		SecretKeyFile:       getenv("VOLS3_SECRET_KEY_FILE", "/run/secrets/s3_secret_key"),
		MounterImage:        getenv("VOLS3_RCLONE_IMAGE", getenv("VOLS3_DEFAULT_RCLONE_IMAGE", "rclone/rclone:latest")),
		HelperImage:         getenv("VOLS3_NSENTER_HELPER_IMAGE", ""),
		ReadyFile:           ".ready",
		PollInterval:        15 * time.Second,
		MounterUpdateMode:   getenv("VOLS3_RCLONE_UPDATE_MODE", defaultUpdateMode()),
		MounterPullInterval: parseDurationOr("24h"),
		UnmountOnExit:       getenv("VOLS3_UNMOUNT_ON_EXIT", "true") == "true",
		AutoCreateBucket:    getenv("VOLS3_AUTOCREATE_BUCKET", "false") == "true",
		AutoCreatePrefix:    getenv("VOLS3_AUTOCREATE_PREFIX", "false") == "true",
		ReadOnly:            getenv("VOLS3_READ_ONLY", "false") == "true",
		AllowOther:          getenv("VOLS3_ALLOW_OTHER", "false") == "true",
		EnableProxy:         getenv("VOLS3_PROXY_ENABLE", "false") == "true",
		LocalLBEnabled:      getenv("VOLS3_PROXY_LOCAL_LB", "false") == "true",
		ProxyPort:           getenv("VOLS3_PROXY_PORT", "8081"),
		ProxyNetwork:        getenv("VOLS3_PROXY_NETWORK", ""),
		LabelPrefix:         getenv("VOLS3_LABEL_PREFIX", getenv("LABEL_PREFIX", "")),
		LabelStrict:         getenv("VOLS3_LABEL_STRICT", "false") == "true",
		StrictReady:         getenv("VOLS3_STRICT_READY", "false") == "true",
		Preset:              getenv("VOLS3_PRESET", ""),
		ReadServiceLabels:   getenv("VOLS3_READ_SERVICE_LABELS", "true") == "true",
		AutoClaimFromMounts: getenv("VOLS3_AUT_CLAIM_FROM_MOUNTS", "false") == "true",
		ClaimAllowlistRegex: getenv("VOLS3_CLAIM_ALLOWLIST_REGEX", ""),
		ImageCleanupEnabled: getenv("VOLS3_IMAGE_CLEANUP_ENABLED", "true") == "true",
		ImageRetentionDays:  getenvInt("VOLS3_IMAGE_RETENTION_DAYS", 14),
		ImageKeepRecent:     getenvInt("VOLS3_IMAGE_KEEP_RECENT", 2),
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
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ctrl.Snapshot())
	})
	mux.HandleFunc("/preflight", func(w http.ResponseWriter, r *http.Request) {
		if err := ctrl.Preflight(); err != nil {
			http.Error(w, err.Error(), http.StatusPreconditionFailed)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("reconcile scheduled"))
		go ctrl.Nudge()
	})
	if getenv("VOLS3_ENABLE_METRICS", "false") == "true" {
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
					"s3mounter_orphan_cleanup_total " + itoa(s.OrphanCleanupTotal) + "\n" +
					"# HELP s3mounter_reconcile_duration_milliseconds Last reconcile duration in ms\n" +
					"# TYPE s3mounter_reconcile_duration_milliseconds gauge\n" +
					"s3mounter_reconcile_duration_milliseconds " + itoa(ctrl.Snapshot().ReconcileDurationMs) + "\n" +
					"# HELP s3mounter_mounter_created_total Total mounter containers created\n" +
					"# TYPE s3mounter_mounter_created_total counter\n" +
					"s3mounter_mounter_created_total " + itoa(ctrl.Snapshot().MounterCreatedTotal) + "\n"))
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
	v := os.Getenv("VOLS3_RCLONE_PULL_INTERVAL")
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
	if v := os.Getenv("VOLS3_RCLONE_UPDATE_MODE"); v != "" {
		return v
	}
	return "never"
}
