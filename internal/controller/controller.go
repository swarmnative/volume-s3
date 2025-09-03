package controller

import (
	"context"
	"fmt"
	"math/rand"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"errors"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

type Config struct {
	MinioEndpointsCSV   string
	S3Provider          string
	S3Endpoint          string
	RcloneRemote        string
	RcloneExtraArgs     string
	Mountpoint          string
	AccessKeyFile       string
	SecretKeyFile       string
	MounterImage        string
	HelperImage         string
	ReadyFile           string
	PollInterval        time.Duration
	MounterUpdateMode   string // never | periodic | on_change
	MounterPullInterval time.Duration
	UnmountOnExit       bool
	AutoCreateBucket    bool
	AutoCreatePrefix    bool
	ReadOnly            bool
	EnableProxy         bool
	LocalLBEnabled      bool
	ProxyPort           string
	ProxyNetwork        string
	LabelPrefix         string
	LabelStrict         bool
	StrictReady         bool
	Preset              string
	// Optional image retention controls (no-op if unused)
	ImageCleanupEnabled bool
	ImageRetentionDays  int
	ImageKeepRecent     int
}

type Controller struct {
	ctx           context.Context
	cli           *client.Client
	cfg           Config
	lastImagePull time.Time
	lastImageID   string
	// metrics
	reconcileTotal      int64
	reconcileErrors     int64
	lastMounterRunning  bool
	lastMountWritable   bool
	lastReconcileMs     int64
	healAttemptsTotal   int64
	healSuccessTotal    int64
	orphanCleanupTotal  int64
	lastHealSuccessUnix int64
	mounterCreatedTotal int64
	// events
	eventCh chan struct{}
	// cache
	selfImageRef string
}

func New(ctx context.Context, cfg Config) (*Controller, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Controller{ctx: ctx, cli: cli, cfg: cfg, eventCh: make(chan struct{}, 1)}, nil
}

func (c *Controller) Run() {
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()
	go c.watchDockerEvents()
	for {
		start := time.Now()
		if err := c.reconcile(); err != nil {
			c.reconcileErrors++
			slog.Error("reconcile error", "error", err)
		}
		c.lastReconcileMs = time.Since(start).Milliseconds()
		select {
		case <-c.ctx.Done():
			return
		case <-c.eventCh:
			// trigger immediate next loop
			continue
		case <-ticker.C:
		}
	}
}

func (c *Controller) watchDockerEvents() {
	f := filters.NewArgs()
	f.Add("type", "service")
	f.Add("type", "container")
	msgs, errs := c.cli.Events(c.ctx, types.EventsOptions{Filters: f})
	backoff := time.Second
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-msgs:
			select {
			case c.eventCh <- struct{}{}:
			default:
			}
		case <-errs:
			// exponential backoff with jitter
			sleep := backoff + time.Duration(rand.Int63n(int64(backoff/2)))
			if sleep > 30*time.Second { sleep = 30 * time.Second }
			time.Sleep(sleep)
			if backoff < 30*time.Second { backoff *= 2 }
			msgs, errs = c.cli.Events(c.ctx, types.EventsOptions{Filters: f})
		}
	}
}

func (c *Controller) timeoutCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.ctx, d)
}

func (c *Controller) Ready() error {
	// mountpoint exists
	if err := os.MkdirAll(c.cfg.Mountpoint, 0o755); err != nil {
		return err
	}
	// in read-only mode, skip write probe
	if !c.cfg.ReadOnly {
		test := filepath.Join(c.cfg.Mountpoint, c.cfg.ReadyFile)
		if err := os.WriteFile(test, []byte(time.Now().Format(time.RFC3339)), 0o644); err != nil {
			return err
		}
		_ = os.Remove(test)
	}
	// optional strict remote check
	if c.cfg.StrictReady {
		u := strings.TrimSpace(c.resolveEndpointForMounter())
		if u != "" {
			ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil || (resp.StatusCode >= 500 && resp.StatusCode != 404) {
				if resp != nil && resp.Body != nil { _ = resp.Body.Close() }
				return fmt.Errorf("remote not ready: %v", err)
			}
			if resp != nil && resp.Body != nil { _ = resp.Body.Close() }
		}
	}
	return nil
}

func (c *Controller) reconcile() error {
	c.reconcileTotal++
	// Ensure mountpoint directory exists
	_ = os.MkdirAll(c.cfg.Mountpoint, 0o755)

	// Try to ensure rshared on host (best-effort)
	if err := c.ensureRShared(); err != nil {
		slog.Warn("ensure rshared failed", "error", err)
	}

	// Auto-pull mounter image according to mode
	switch c.cfg.MounterUpdateMode {
	case "periodic":
		if err := c.pullMounterImageIfDue(); err != nil {
			slog.Warn("pull mounter image", "error", err)
		}
	case "on_change":
		if err := c.pullMounterImageIfChanged(); err != nil {
			slog.Warn("pull mounter image (on_change)", "error", err)
		}
	}

	// Ensure mounter container exists
	if err := c.ensureMounter(); err != nil {
		return err
	}

	// If mount is stuck, try cleanup (best-effort)
	if err := c.checkAndHealMount(); err != nil {
		slog.Warn("heal mount", "error", err)
	} else {
		c.healAttemptsTotal++
		if testRW(c.cfg.Mountpoint) == nil {
			c.healSuccessTotal++
			c.lastHealSuccessUnix = time.Now().Unix()
		}
	}

	// Declarative claim provisioning: create requested prefixes under mountpoint
	if err := c.provisionClaims(); err != nil {
		slog.Warn("provision claims", "error", err)
	}

	// Emit status to logs
	c.logStatus()
	// Cleanup orphaned rclone containers (best-effort)
	if err := c.cleanupOrphanedMounters(); err != nil {
		slog.Warn("cleanup orphaned mounters", "error", err)
	}
	return nil
}

// ensureImagePresent makes sure the given image reference is available locally.
func (c *Controller) ensureImagePresent(img string) error {
	img = strings.TrimSpace(img)
	if img == "" {
		return fmt.Errorf("empty image reference")
	}
	if _, _, err := c.cli.ImageInspectWithRaw(c.ctx, img); err == nil {
		return nil
	}
	ctx, cancel := c.timeoutCtx(60 * time.Second)
	rc, err := c.cli.ImagePull(ctx, img, image.PullOptions{})
	if err != nil {
		cancel()
		return err
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc)
	cancel()
	// verify
	if _, _, err := c.cli.ImageInspectWithRaw(c.ctx, img); err != nil {
		return err
	}
	return nil
}

func (c *Controller) ensureMounter() error {
	name := c.mounterName()
	// find by name
	args := filters.NewArgs()
	args.Add("name", name)
	ctx, cancel := c.timeoutCtx(10 * time.Second)
	defer cancel()
	conts, err := c.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return err
	}
	// If exists and image changed (after pull), recreate
	desiredImageID := c.cachedImageID()
	if len(conts) > 0 {
		id := conts[0].ID
		ictx, icancel := c.timeoutCtx(5 * time.Second)
		inspect, err := c.cli.ContainerInspect(ictx, id)
		icancel()
		if err == nil {
			if desiredImageID != "" && inspect.Image != desiredImageID {
				rctx, rcancel := c.timeoutCtx(10 * time.Second)
				_ = c.cli.ContainerRemove(rctx, id, container.RemoveOptions{Force: true})
				rcancel()
			} else if inspect.State != nil && inspect.State.Running {
				return nil
			} else {
				sctx, scancel := c.timeoutCtx(10 * time.Second)
				if err := c.cli.ContainerStart(sctx, id, container.StartOptions{}); err == nil {
					scancel()
					return nil
				}
				scancel()
				r2ctx, r2cancel := c.timeoutCtx(10 * time.Second)
				_ = c.cli.ContainerRemove(r2ctx, id, container.RemoveOptions{Force: true})
				r2cancel()
			}
		}
	}

	// read secrets
	_, _ = os.ReadFile(c.cfg.AccessKeyFile)
	_, _ = os.ReadFile(c.cfg.SecretKeyFile)

	env := c.buildRcloneEnv()

	cmd := []string{"mount", c.cfg.RcloneRemote, c.cfg.Mountpoint, "--allow-other", "--vfs-cache-mode=writes", "--dir-cache-time=12h"}
	// presets first
	cmd = append(cmd, c.buildPresetArgs()...)
	if c.cfg.ReadOnly {
		cmd = append(cmd, "--read-only")
	}
	if strings.TrimSpace(c.cfg.RcloneExtraArgs) != "" {
		cmd = append(cmd, parseArgs(c.cfg.RcloneExtraArgs)...)
	}

	// Attach to overlay network when provided; add node-local alias if local LB is enabled
	var netCfg *network.NetworkingConfig
	if strings.TrimSpace(c.cfg.ProxyNetwork) != "" {
		es := &network.EndpointSettings{}
		if c.cfg.EnableProxy && c.cfg.LocalLBEnabled {
			es.Aliases = []string{c.localAlias()}
		}
		netCfg = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			c.cfg.ProxyNetwork: es,
		}}
	} else {
		netCfg = &network.NetworkingConfig{}
	}

	// ensure mounter image exists
	if err := c.ensureImagePresent(c.cfg.MounterImage); err != nil {
		return fmt.Errorf("ensure mounter image: %w", err)
	}

	cctx, ccancel := c.timeoutCtx(20 * time.Second)
	resp, err := c.cli.ContainerCreate(cctx,
		&container.Config{
			Image: c.cfg.MounterImage,
			Env:   env,
			Cmd:   cmd,
			Labels: map[string]string{
				"swarmnative.mounter": "managed",
			},
		},
		&container.HostConfig{
			Privileged:  false,
			CapAdd:      []string{"SYS_ADMIN"},
			NetworkMode: c.selfNetworkMode(),
			RestartPolicy: container.RestartPolicy{
				Name: "always",
			},
			Binds: []string{
				"/dev/fuse:/dev/fuse",
				fmt.Sprintf("%s:%s:rshared", c.cfg.Mountpoint, c.cfg.Mountpoint),
			},
			SecurityOpt: []string{"apparmor=unconfined", "seccomp=unconfined"},
			Resources: container.Resources{
				Devices: []container.DeviceMapping{{PathOnHost: "/dev/fuse", PathInContainer: "/dev/fuse", CgroupPermissions: "mrw"}},
			},
		},
		netCfg,
		nil,
		name,
	)
	ccancel()
	if err != nil {
		return fmt.Errorf("create mounter: %w", err)
	}
	sctx2, scancel2 := c.timeoutCtx(15 * time.Second)
	if err := c.cli.ContainerStart(sctx2, resp.ID, container.StartOptions{}); err != nil {
		scancel2()
		return fmt.Errorf("start mounter: %w", err)
	}
	scancel2()
	c.mounterCreatedTotal++
	return nil
}

func (c *Controller) pullMounterImageIfDue() error {
	if time.Since(c.lastImagePull) < c.cfg.MounterPullInterval {
		return nil
	}
	ictx, icancel := c.timeoutCtx(60 * time.Second)
	rc, err := c.cli.ImagePull(ictx, c.cfg.MounterImage, image.PullOptions{})
	if err != nil {
		icancel()
		return err
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc)
	c.lastImagePull = time.Now()
	if ii, _, err := c.cli.ImageInspectWithRaw(ictx, c.cfg.MounterImage); err == nil {
		c.lastImageID = ii.ID
	}
	icancel()
	return nil
}

func (c *Controller) pullMounterImageIfChanged() error {
	// Check current image id
	current := c.cachedImageID()
	// Pull new
	ipctx, ipcancel := c.timeoutCtx(60 * time.Second)
	rc, err := c.cli.ImagePull(ipctx, c.cfg.MounterImage, image.PullOptions{})
	if err != nil {
		ipcancel()
		return err
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc)
	c.lastImagePull = time.Now()
	// Inspect new id
	if ii, _, err := c.cli.ImageInspectWithRaw(ipctx, c.cfg.MounterImage); err == nil {
		if current != "" && ii.ID == current {
			// unchanged
			ipcancel()
			return nil
		}
		c.lastImageID = ii.ID
	}
	ipcancel()
	return nil
}

func (c *Controller) cachedImageID() string {
	if c.lastImageID != "" {
		return c.lastImageID
	}
	if ii, _, err := c.cli.ImageInspectWithRaw(c.ctx, c.cfg.MounterImage); err == nil {
		c.lastImageID = ii.ID
		return c.lastImageID
	}
	return ""
}

func (c *Controller) ensureRShared() error {
	// Use host namespace via nsenter available in main image (util-linux preinstalled)
	sh := fmt.Sprintf("nsenter -t 1 -m -- mount --make-rshared %s || mount --make-rshared %s", c.cfg.Mountpoint, c.cfg.Mountpoint)
	// ensure helper image exists
	if err := c.ensureImagePresent(c.helperImageRef()); err != nil {
		return err
	}
	cont, err := c.cli.ContainerCreate(c.ctx,
		&container.Config{Image: c.helperImageRef(), Cmd: []string{"sh", "-c", sh}},
		&container.HostConfig{Privileged: true, PidMode: "host", Binds: []string{fmt.Sprintf("%s:%s", c.cfg.Mountpoint, c.cfg.Mountpoint)}},
		&network.NetworkingConfig{}, nil, c.helperName("rshared-helper"))
	if err != nil {
		return err
	}
	defer func() { _ = c.cli.ContainerRemove(c.ctx, cont.ID, container.RemoveOptions{Force: true}) }()
	_ = c.cli.ContainerStart(c.ctx, cont.ID, container.StartOptions{})
	time.Sleep(1 * time.Second)
	return nil
}

func (c *Controller) checkAndHealMount() error {
	// If mountpoint exists but not usable, try lazy unmount via helper
	if err := testRW(c.cfg.Mountpoint); err == nil {
		return nil
	}
	sh := fmt.Sprintf("(nsenter -t 1 -m -- fusermount -uz %[1]s || true); (nsenter -t 1 -m -- umount -l %[1]s || true)", c.cfg.Mountpoint)
	// ensure helper image exists
	if err := c.ensureImagePresent(c.helperImageRef()); err != nil {
		return err
	}
	cont, err := c.cli.ContainerCreate(c.ctx,
		&container.Config{Image: c.helperImageRef(), Cmd: []string{"sh", "-c", sh}},
		&container.HostConfig{Privileged: true, PidMode: "host", Binds: []string{fmt.Sprintf("%s:%s", c.cfg.Mountpoint, c.cfg.Mountpoint)}},
		&network.NetworkingConfig{}, nil, c.helperName("umount-helper"))
	if err != nil {
		return err
	}
	defer func() { _ = c.cli.ContainerRemove(c.ctx, cont.ID, container.RemoveOptions{Force: true}) }()
	_ = c.cli.ContainerStart(c.ctx, cont.ID, container.StartOptions{})
	time.Sleep(1 * time.Second)
	return nil
}

func parseArgs(s string) []string {
	// simple split by spaces respecting quotes
	var out []string
	s = strings.TrimSpace(s)
	if s == "" {
		return out
	}
	re := regexp.MustCompile(`\s+`)
	// naive: split by spaces; for complex quoting users can pass via array env not supported here
	parts := re.Split(s, -1)
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c *Controller) logStatus() {
	// container state
	name := c.mounterName()
	args := filters.NewArgs()
	args.Add("name", name)
	conts, err := c.cli.ContainerList(c.ctx, container.ListOptions{All: true, Filters: args})
	running := false
	if err == nil && len(conts) > 0 {
		id := conts[0].ID
		if inspect, err := c.cli.ContainerInspect(c.ctx, id); err == nil && inspect.State != nil {
			running = inspect.State.Running
		}
	}
	mountOK := testRW(c.cfg.Mountpoint) == nil
	c.lastMounterRunning = running
	c.lastMountWritable = mountOK
	slog.Info("status", "mounter_running", running, "mount_writable", mountOK, "last_image_pull", c.lastImagePull.Format(time.RFC3339))
}

// --- Declarative volume (prefix) provisioning via service/container labels ---

// const labelPrefix = "s3.mounter.swarmnative.io/" // deprecated: no longer used

// parseLabels applies STANDARDS label parsing: default no-prefix keys (s3.*)
// plus optional prefix (c.cfg.LabelPrefix). When LabelPrefix is empty, both
// prefixed and unprefixed keys are accepted; prefixed wins on conflicts with a
// warning. When LabelPrefix is set, only that prefix and no-prefix are accepted;
// other prefixes are ignored with warning.
func (c *Controller) parseLabels(labels map[string]string) map[string]string {
	allowed := map[string]struct{}{
		"s3.enabled": {},
		"s3.bucket":  {},
		"s3.prefix":  {},
		"s3.class":   {},
		"s3.reclaim": {},
		"s3.access":  {},
		"s3.args":    {},
	}
	values := map[string]struct {
		v       string
		pref    bool
		pfxName string
	}{}
	specified := strings.TrimSpace(c.cfg.LabelPrefix)
	for k, v := range labels {
		base := k
		prefix := ""
		if i := strings.Index(k, "/"); i >= 0 {
			prefix = k[:i]
			base = k[i+1:]
		}
		if _, ok := allowed[base]; !ok {
			if c.cfg.LabelStrict {
				slog.Error("unknown label key", "key", k)
			} else {
				slog.Warn("unknown label key", "key", k)
			}
			continue
		}
		if specified != "" {
			if prefix != "" && prefix != specified {
				slog.Warn("ignore label from other prefix", "key", k)
				continue
			}
		}
		fromPref := prefix != ""
		if old, ok := values[base]; ok {
			if old.pref && fromPref {
				if c.cfg.LabelStrict {
					slog.Error("conflicting prefixed labels", "key", base)
				} else {
					slog.Warn("conflicting prefixed labels", "key", base)
				}
				continue
			}
			if !old.pref && fromPref {
				slog.Warn("prefixed label overrides unprefixed", "key", base, "prefix", prefix)
				values[base] = struct {
					v       string
					pref    bool
					pfxName string
				}{v: v, pref: true, pfxName: prefix}
				continue
			}
			continue
		}
		values[base] = struct {
			v       string
			pref    bool
			pfxName string
		}{v: v, pref: fromPref, pfxName: prefix}
	}
	out := make(map[string]string, len(values))
	for k, meta := range values {
		out[k] = meta.v
	}
	return out
}

type claimSpec struct {
	enabled bool
	bucket  string
	prefix  string
	class   string
	reclaim string // Retain|Delete
	access  string // rw|ro
	args    string // extra rclone args suggestion (not enforced per-service)
}

func (c *Controller) provisionClaims() error {
	// ensure mount is writable first
	if err := testRW(c.cfg.Mountpoint); err != nil {
		return err
	}
	conts, err := c.cli.ContainerList(c.ctx, container.ListOptions{All: false})
	if err != nil {
		return err
	}
	specs := c.collectClaimSpecs(conts)
	for _, s := range specs {
		if !s.enabled || s.prefix == "" {
			continue
		}
		// Ensure remote bucket/prefix exists if configured
		if err := c.ensureRemotePaths(s); err != nil {
			slog.Warn("claim ensure remote", "bucket", s.bucket, "prefix", s.prefix, "error", err)
		}
		// Create prefix directory under mount (idempotent)
		p := filepath.Join(c.cfg.Mountpoint, filepath.Clean("/"+s.prefix))
		if err := os.MkdirAll(p, 0o755); err != nil {
			slog.Warn("claim mkdir", "path", p, "error", err)
		}
	}
	return nil
}

func (c *Controller) collectClaimSpecs(conts []types.Container) []claimSpec {
	var out []claimSpec
	for _, ct := range conts {
		if len(ct.Labels) == 0 {
			continue
		}
		var cs claimSpec
		m := c.parseLabels(ct.Labels)
		if v, ok := m["s3.enabled"]; ok {
			cs.enabled = strings.EqualFold(v, "true")
		}
		if v, ok := m["s3.bucket"]; ok {
			cs.bucket = v
		}
		if v, ok := m["s3.prefix"]; ok {
			cs.prefix = strings.Trim(v, "/")
		}
		if v, ok := m["s3.class"]; ok {
			cs.class = v
		}
		if v, ok := m["s3.reclaim"]; ok {
			cs.reclaim = v
		}
		if v, ok := m["s3.access"]; ok {
			cs.access = v
		}
		if v, ok := m["s3.args"]; ok {
			cs.args = v
		}
		if cs.enabled {
			out = append(out, cs)
		}
	}
	return out
}

func (c *Controller) buildRcloneEnv() []string {
	// credentials: env overrides file
	access := strings.TrimSpace(os.Getenv("VOLS3_ACCESS_KEY"))
	secret := strings.TrimSpace(os.Getenv("VOLS3_SECRET_KEY"))
	token := strings.TrimSpace(os.Getenv("VOLS3_SESSION_TOKEN"))
	if access == "" {
		if b, err := os.ReadFile(c.cfg.AccessKeyFile); err == nil {
			access = strings.TrimSpace(string(b))
		}
	}
	if secret == "" {
		if b, err := os.ReadFile(c.cfg.SecretKeyFile); err == nil {
			secret = strings.TrimSpace(string(b))
		}
	}
	env := []string{
		"RCLONE_CONFIG_S3_TYPE=s3",
		fmt.Sprintf("RCLONE_CONFIG_S3_ACCESS_KEY_ID=%s", access),
		fmt.Sprintf("RCLONE_CONFIG_S3_SECRET_ACCESS_KEY=%s", secret),
		fmt.Sprintf("RCLONE_CONFIG_S3_ENDPOINT=%s", c.resolveEndpointForMounter()),
	}
	if token != "" {
		env = append(env, fmt.Sprintf("RCLONE_CONFIG_S3_SESSION_TOKEN=%s", token))
	}
	if strings.TrimSpace(c.cfg.S3Provider) != "" {
		env = append(env, fmt.Sprintf("RCLONE_CONFIG_S3_PROVIDER=%s", c.cfg.S3Provider))
	}
	return env
}

func (c *Controller) ensureRemotePaths(s claimSpec) error {
	// Only act when configured
	if !(c.cfg.AutoCreateBucket || c.cfg.AutoCreatePrefix) {
		return nil
	}
	// read-only mode skips remote creation
	if c.cfg.ReadOnly {
		return nil
	}
	// require bucket name for remote operations
	if strings.TrimSpace(s.bucket) == "" {
		return nil
	}
	// mkdir bucket
	if c.cfg.AutoCreateBucket {
		if err := c.runRcloneCmd([]string{"mkdir", fmt.Sprintf("S3:%s", s.bucket)}); err != nil {
			// ignore errors like already exists
			slog.Warn("mkdir bucket", "bucket", s.bucket, "error", err)
		}
	}
	if c.cfg.AutoCreatePrefix && strings.TrimSpace(s.prefix) != "" {
		remotePath := fmt.Sprintf("S3:%s/%s", s.bucket, strings.Trim(s.prefix, "/"))
		if err := c.runRcloneCmd([]string{"mkdir", remotePath}); err != nil {
			slog.Warn("mkdir prefix", "path", remotePath, "error", err)
		}
	}
	return nil
}

func (c *Controller) runRcloneCmd(cmd []string) error {
	name := c.helperName("rclone-run")
	env := c.buildRcloneEnv()
	// Ensure helper can reach the S3 endpoint: attach to overlay network if provided
	var netCfg *network.NetworkingConfig
	if strings.TrimSpace(c.cfg.ProxyNetwork) != "" {
		netCfg = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			c.cfg.ProxyNetwork: {},
		}}
	} else {
		netCfg = &network.NetworkingConfig{}
	}
	// ensure mounter image exists (used to run rclone cmd)
	if err := c.ensureImagePresent(c.cfg.MounterImage); err != nil {
		return err
	}
	cont, err := c.cli.ContainerCreate(c.ctx,
		&container.Config{Image: c.cfg.MounterImage, Env: env, Cmd: cmd},
		&container.HostConfig{NetworkMode: c.selfNetworkMode()},
		netCfg, nil, name)
	if err != nil {
		return err
	}
	defer func() { _ = c.cli.ContainerRemove(context.Background(), cont.ID, container.RemoveOptions{Force: true}) }()
	if err := c.cli.ContainerStart(c.ctx, cont.ID, container.StartOptions{}); err != nil {
		return err
	}
	// wait for completion
	_, errCh := c.cli.ContainerWait(c.ctx, cont.ID, container.WaitConditionNotRunning)
	if err := <-errCh; err != nil {
		return err
	}
	return nil
}

// cleanupOrphanedMounters removes exited/created rclone mounter containers that
// are managed by this controller (identified by name prefix and label).
func (c *Controller) cleanupOrphanedMounters() error {
	// Identify by name prefix and label
	args := filters.NewArgs()
	args.Add("name", "rclone-mounter-")
	args.Add("label", "swarmnative.mounter=managed")
	// Include non-running containers
	conts, err := c.cli.ContainerList(c.ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return err
	}
	removed := 0
	for _, ct := range conts {
		if ct.State == "running" || ct.State == "restarting" {
			continue
		}
		// best-effort remove
		_ = c.cli.ContainerRemove(c.ctx, ct.ID, container.RemoveOptions{Force: true})
		removed++
	}
	if removed > 0 {
		c.orphanCleanupTotal += int64(removed)
	}
	return nil
}

// MetricsSnapshot is a read-only copy of controller metrics/state for exposition.
type MetricsSnapshot struct {
	ReconcileTotal      int64
	ReconcileErrors     int64
	MounterRunning      bool
	MountWritable       bool
	HealAttemptsTotal   int64
	HealSuccessTotal    int64
	LastHealSuccessUnix int64
	OrphanCleanupTotal  int64
	ReconcileDurationMs int64
	MounterCreatedTotal int64
}

func (c *Controller) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		ReconcileTotal:      c.reconcileTotal,
		ReconcileErrors:     c.reconcileErrors,
		MounterRunning:      c.lastMounterRunning,
		MountWritable:       c.lastMountWritable,
		HealAttemptsTotal:   c.healAttemptsTotal,
		HealSuccessTotal:    c.healSuccessTotal,
		LastHealSuccessUnix: c.lastHealSuccessUnix,
		OrphanCleanupTotal:  c.orphanCleanupTotal,
		ReconcileDurationMs: c.lastReconcileMs,
		MounterCreatedTotal: c.mounterCreatedTotal,
	}
}

// Cleanup attempts to lazy-unmount and remove the mounter container on shutdown
func (c *Controller) Cleanup() {
	if !c.cfg.UnmountOnExit {
		return
	}
	// lazy unmount via helper
	_ = c.checkAndHealMount()
	// stop & remove mounter if exists
	args := filters.NewArgs()
	args.Add("name", c.mounterName())
	conts, err := c.cli.ContainerList(c.ctx, container.ListOptions{All: true, Filters: args})
	if err == nil && len(conts) > 0 {
		id := conts[0].ID
		_ = c.cli.ContainerRemove(context.Background(), id, container.RemoveOptions{Force: true})
	}
}

func (c *Controller) Nudge() {
	select {
	case c.eventCh <- struct{}{}:
	default:
	}
}

func (c *Controller) Preflight() error {
	var errs []string
	// Docker API reachable
	if _, err := c.cli.Ping(c.ctx); err != nil {
		errs = append(errs, fmt.Sprintf("docker ping failed: %v", err))
	}
	// Credentials resolved
	env := c.buildRcloneEnv()
	hasAK := false
	hasSK := false
	for _, e := range env {
		if strings.HasPrefix(e, "RCLONE_CONFIG_S3_ACCESS_KEY_ID=") { hasAK = true }
		if strings.HasPrefix(e, "RCLONE_CONFIG_S3_SECRET_ACCESS_KEY=") { hasSK = true }
	}
	if !hasAK || !hasSK {
		errs = append(errs, "missing access/secret credentials (set VOLS3_ACCESS_KEY/SECRET_KEY or mount secret files)")
	}
	// Helper image nsenter availability (best-effort)
	name := c.helperName("nsenter-check")
	cont, err := c.cli.ContainerCreate(c.ctx,
		&container.Config{Image: c.helperImageRef(), Cmd: []string{"sh", "-lc", "nsenter --version >/dev/null 2>&1 || exit 1"}},
		&container.HostConfig{}, &network.NetworkingConfig{}, nil, name)
	if err == nil {
		_ = c.cli.ContainerStart(c.ctx, cont.ID, container.StartOptions{})
		_, werr := c.cli.ContainerWait(c.ctx, cont.ID, container.WaitConditionNotRunning)
		if werr != nil {
			errs = append(errs, "helper image may lack nsenter (set VOLS3_NSENTER_HELPER_IMAGE to this image)")
		}
		_ = c.cli.ContainerRemove(c.ctx, cont.ID, container.RemoveOptions{Force: true})
	} else {
		errs = append(errs, fmt.Sprintf("cannot create helper for nsenter check: %v", err))
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (c *Controller) mounterName() string {
	return "rclone-mounter-" + sanitizeHostname()
}

func (c *Controller) helperName(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, sanitizeHostname())
}

func sanitizeHostname() string {
	hn, err := os.Hostname()
	if err != nil || hn == "" {
		hn = "unknown"
	}
	// replace non-alphanumeric with hyphen
	b := make([]rune, 0, len(hn))
	for _, r := range hn {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b = append(b, r)
		} else {
			b = append(b, '-')
		}
	}
	return strings.Trim(btoa(b), "-")
}

func btoa(b []rune) string { return string(b) }

// selfNetworkMode returns a network mode that allows the mounter to reach the
// in-controller HAProxy on 127.0.0.1 if proxy is enabled; otherwise none.
func (c *Controller) selfNetworkMode() container.NetworkMode {
	// If proxy is enabled, run mounter in the same network namespace as controller
	// so that S3_ENDPOINT=http://127.0.0.1:8081 works. We achieve this by sharing
	// the network stack via container:SELF, but Docker API requires an ID; as a
	// simpler alternative we can use host loopback by setting network to bridge
	// and using controller container IP. For simplicity keep "bridge"; recommend
	// users set S3_ENDPOINT to a resolvable service (tasks.*) when proxy disabled.
	return "bridge"
}

func (c *Controller) resolveEndpointForMounter() string {
	// If local LB is enabled and proxy network is set, advertise node-local
	// alias on that overlay network so mounter can address the local HAProxy.
	if c.cfg.EnableProxy && c.cfg.LocalLBEnabled && strings.TrimSpace(c.cfg.ProxyNetwork) != "" {
		return fmt.Sprintf("http://%s:%s", c.localAlias(), strings.TrimSpace(c.cfg.ProxyPort))
	}
	return c.cfg.S3Endpoint
}

func (c *Controller) localAlias() string {
	return "volume-s3-lb-" + sanitizeHostname()
}

func testRW(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	f := filepath.Join(path, ".rw-test")
	if err := os.WriteFile(f, []byte("ok"), fs.FileMode(0o644)); err != nil {
		return err
	}
	_ = os.Remove(f)
	return nil
}

// ValidationResult provides structured validation outcome.
type ValidationResult struct {
	OK       bool              `json:"ok"`
	Errors   []string          `json:"errors"`
	Warnings []string          `json:"warnings"`
	Summary  map[string]string `json:"summary"`
}

// ValidateConfig performs static checks on configuration and returns a structured result.
func ValidateConfig(cfg Config) ValidationResult {
	var errs []string
	var warns []string

	if strings.TrimSpace(cfg.Mountpoint) == "" {
		errs = append(errs, "mountpoint is required")
	}
	if strings.TrimSpace(cfg.S3Endpoint) == "" {
		errs = append(errs, "S3 endpoint is required")
	} else {
		if u, err := url.Parse(cfg.S3Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, "S3 endpoint must be a valid URL (e.g. http(s)://host:port)")
		}
	}
	if strings.TrimSpace(cfg.MounterImage) == "" {
		errs = append(errs, "mounter image is required")
	}
	// Treat zero PollInterval as "use default" and do not error
	if cfg.PollInterval < 0 {
		errs = append(errs, "poll interval must be >= 0")
	}
	// Allow empty MounterUpdateMode to imply default "never"; only validate when non-empty
	if strings.TrimSpace(cfg.MounterUpdateMode) != "" {
		switch cfg.MounterUpdateMode {
		case "never", "periodic", "on_change":
		default:
			errs = append(errs, "mounter update mode must be one of never|periodic|on_change")
		}
	}
	if _, err := os.Stat(cfg.AccessKeyFile); err != nil {
		warns = append(warns, fmt.Sprintf("access key file not readable: %v", err))
	}
	if _, err := os.Stat(cfg.SecretKeyFile); err != nil {
		warns = append(warns, fmt.Sprintf("secret key file not readable: %v", err))
	}
	if strings.TrimSpace(cfg.ProxyPort) != "" {
		if _, err := strconv.Atoi(cfg.ProxyPort); err != nil {
			errs = append(errs, "proxy port must be a number")
		}
	}
	if cfg.ReadOnly && (cfg.AutoCreateBucket || cfg.AutoCreatePrefix) {
		warns = append(warns, "read-only mode: auto-create bucket/prefix is ignored")
	}
	sum := map[string]string{
		"mountpoint":            cfg.Mountpoint,
		"s3_endpoint":           cfg.S3Endpoint,
		"s3_provider":           cfg.S3Provider,
		"rclone_remote":         cfg.RcloneRemote,
		"mounter_image":         cfg.MounterImage,
		"helper_image":          cfg.HelperImage,
		"poll_interval":         cfg.PollInterval.String(),
		"mounter_update_mode":   cfg.MounterUpdateMode,
		"mounter_pull_interval": cfg.MounterPullInterval.String(),
		"unmount_on_exit":       fmt.Sprintf("%t", cfg.UnmountOnExit),
		"auto_create_bucket":    fmt.Sprintf("%t", cfg.AutoCreateBucket),
		"auto_create_prefix":    fmt.Sprintf("%t", cfg.AutoCreatePrefix),
		"read_only":             fmt.Sprintf("%t", cfg.ReadOnly),
		"enable_proxy":          fmt.Sprintf("%t", cfg.EnableProxy),
		"local_lb_enabled":      fmt.Sprintf("%t", cfg.LocalLBEnabled),
		"proxy_port":            cfg.ProxyPort,
		"proxy_network":         cfg.ProxyNetwork,
		"label_prefix":          cfg.LabelPrefix,
		"access_key_file":       cfg.AccessKeyFile,
		"secret_key_file":       cfg.SecretKeyFile,
	}
	return ValidationResult{OK: len(errs) == 0, Errors: errs, Warnings: warns, Summary: sum}
}

func (c *Controller) buildPresetArgs() []string {
	p := strings.ToLower(strings.TrimSpace(c.cfg.Preset))
	switch p {
	case "aws":
		return []string{"--s3-region=us-east-1"}
	case "minio", "ceph":
		return []string{"--s3-force-path-style=true"}
	case "wasabi":
		return []string{"--s3-region=us-east-1", "--s3-force-path-style=true"}
	case "aliyun":
		return []string{"--s3-provider=Alibaba", "--s3-force-path-style=true"}
	default:
		return nil
	}
}

// helperImageRef returns the image to use for helper containers.
// If cfg.HelperImage is empty, it tries to use the current controller's image reference.
func (c *Controller) helperImageRef() string {
	if strings.TrimSpace(c.cfg.HelperImage) != "" {
		return c.cfg.HelperImage
	}
	// cached
	if c.selfImageRef != "" {
		return c.selfImageRef
	}
	// Strategy 1: Inspect by container hostname (Docker sets hostname = container ID)
	if hn, err := os.Hostname(); err == nil && strings.TrimSpace(hn) != "" {
		if insp, err := c.cli.ContainerInspect(c.ctx, hn); err == nil && insp.Config != nil {
			if img := strings.TrimSpace(insp.Config.Image); img != "" {
				c.selfImageRef = img
				return c.selfImageRef
			}
		}
	}
	// Strategy 2: Parse /proc/self/cgroup to extract container ID
	data, err := os.ReadFile("/proc/self/cgroup")
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for _, ln := range lines {
			if ln == "" { continue }
			// pick the path part after the last ':'
			parts := strings.SplitN(ln, ":", 3)
			path := ln
			if len(parts) == 3 { path = parts[2] }
			if i := strings.LastIndex(path, "/"); i >= 0 {
				id := strings.TrimSpace(path[i+1:])
				id = strings.TrimSuffix(id, ".scope")
				id = strings.TrimPrefix(id, "docker-")
				if len(id) >= 12 {
					if insp, err := c.cli.ContainerInspect(c.ctx, id); err == nil && insp.Config != nil {
						if img := strings.TrimSpace(insp.Config.Image); img != "" {
							c.selfImageRef = img
							return c.selfImageRef
						}
					}
				}
			}
		}
	}
	// Fallback: use our published default image (may require pull)
	return "ghcr.io/swarmnative/volume-s3:latest"
}
