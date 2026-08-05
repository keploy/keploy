package app

// compose_sdk.go is the Docker-SDK compose orchestrator used ONLY by
// `keploy cloud replay` (the in-memory compose path, uniquely identified by
// len(a.composeContent) > 0). Instead of shelling out to `docker compose -f -
// up/down/ps`, it drives the Docker daemon directly through the moby client
// already embedded in docker.Client, faithfully reproducing what
// `docker compose up --abort-on-container-exit --exit-code-from <app>` does for
// the bounded stack cloud replay generates: the injected keploy-agent service
// plus the app container(s) reconstructed from a single Kubernetes deployment.
// The app's real infra dependencies are NOT run (they are served from recorded
// mocks by the keploy proxy), so there is no general dependency graph to
// resolve — only "start the agent, wait until it is healthy, then start the
// app sharing the agent's net/pid namespace, and wait for the app to exit".
//
// The normal file-based `keploy test`/`keploy record` compose replay path is
// untouched and keeps using the compose CLI.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// sdkComposeProject is the compose "project" label the SDK path stamps on the
// containers/volumes it creates, so logTargetContainer/recentAppLogs' label
// filter keeps working and the stack is easy to identify.
const sdkComposeProject = "keploy-cloud-replay"

// sdkTeardownBudget is the single hard deadline for the entire SDK teardown, so
// even a multi-container stack stays well inside the record/replay
// teardown-drain budget (utils.DrainErrGroup, 30s). Every per-container stop /
// remove derives its context from this parent.
const sdkTeardownBudget = 25 * time.Second

// sdkContainerRef tracks a container the orchestrator created so teardown can
// stop/remove it. isApp marks the app service(s) (they get a coverage-flush
// grace on stop, unlike the agent).
type sdkContainerRef struct {
	id      string
	name    string
	service string
	isApp   bool
}

// runComposeSDK brings the cloud-replay stack up via the Docker SDK and blocks
// until the app container exits (the --exit-code-from semantics), returning its
// exit code as a models.AppError. Teardown is handled by the caller's deferred
// ComposeDown -> composeSDKTeardown.
func (a *App) runComposeSDK(ctx context.Context) models.AppError {
	model, err := docker.ParseComposeForSDK(a.composeContent)
	if err != nil {
		return models.AppError{AppErrorType: models.ErrCommandError, Err: err}
	}

	agentSvc, appSvcs := a.classifyComposeServices(model)
	if agentSvc == nil {
		return models.AppError{AppErrorType: models.ErrCommandError,
			Err: fmt.Errorf("keploy-agent service not found in the generated cloud-replay compose")}
	}
	if len(appSvcs) == 0 {
		return models.AppError{AppErrorType: models.ErrCommandError,
			Err: fmt.Errorf("no application service found in the generated cloud-replay compose")}
	}

	// Free any container names left over by a prior run (SDK, no `docker rm`
	// CLI) so create can't hit "name already in use".
	a.sdkPreClean(ctx, model)

	// Pre-create the named volumes compose would (keploy-tls-certs, the shared
	// /tmp volume). Idempotent; docker would auto-create on mount anyway.
	a.sdkEnsureVolumes(ctx, model)

	a.logger.Info("Bringing up the cloud-replay stack via the Docker SDK (no docker compose CLI)",
		zap.String("agent", agentSvc.Key), zap.Int("app_services", len(appSvcs)))

	// 1. Agent first — it owns the net/pid namespace the app joins and carries
	//    the healthcheck the app's start is gated on.
	agentID, aerr := a.sdkCreateAndStart(ctx, *agentSvc, "")
	if aerr.Err != nil {
		return aerr
	}

	// Preserve the log line the CLI path emitted (utils.ExecuteCommand) and the
	// e2e guard asserts on ("Starting Application ?:").
	a.logger.Info("Starting Application :", zap.String("via", "docker-sdk"))

	// 2. Gate on agent readiness: container healthcheck (compose
	//    depends_on: service_healthy) + an HTTP /agent/ready fallback, bounded
	//    by the agent's own readiness budget so a broken health binary can never
	//    hang app start forever.
	if rerr := a.waitAgentReady(ctx, agentID); rerr != nil {
		return models.AppError{AppErrorType: models.ErrCommandError, Err: rerr, AppLogs: a.recentAppLogs(ctx)}
	}

	// 3. App service(s) — share the agent's net + pid namespace.
	for _, svc := range appSvcs {
		if _, cerr := a.sdkCreateAndStart(ctx, svc, agentID); cerr.Err != nil {
			return cerr
		}
	}

	// 4. Stream container logs to the terminal (compose up streams both the
	//    agent and app logs); stop the pumps when the app exits / we tear down.
	logCtx, cancelLogs := context.WithCancel(ctx)
	defer cancelLogs()
	a.startSDKLogPumps(logCtx)

	// 5. Wait for the app (the --exit-code-from target) to exit.
	return a.sdkWaitForApp(ctx)
}

// classifyComposeServices splits the parsed model into the keploy-agent service
// (matched by the fixed service key or the agent container_name) and the app
// service(s) (everything else).
func (a *App) classifyComposeServices(model *docker.ComposeModel) (*docker.ServiceSpec, []docker.ServiceSpec) {
	var agent *docker.ServiceSpec
	var apps []docker.ServiceSpec
	for i := range model.Services {
		s := model.Services[i]
		if s.Key == keployAgentComposeService || (a.keployContainer != "" && s.ContainerName == a.keployContainer) {
			cp := s
			agent = &cp
			continue
		}
		apps = append(apps, s)
	}
	return agent, apps
}

// sdkCreateAndStart translates a compose service to a container and starts it.
// agentID is empty for the agent itself (standalone networking, publishes
// ports); for app services it is the started agent's container id, so the app
// shares the agent's net/pid namespace (compose `service:keploy-agent`).
func (a *App) sdkCreateAndStart(ctx context.Context, svc docker.ServiceSpec, agentID string) (string, models.AppError) {
	cfg, hostCfg, err := buildContainerSpec(svc, agentID)
	if err != nil {
		return "", models.AppError{AppErrorType: models.ErrCommandError, Err: err}
	}

	name := svc.ContainerName
	if name == "" {
		name = svc.Key
	}

	if err := a.sdkEnsureImage(ctx, svc.Image); err != nil {
		return "", models.AppError{AppErrorType: models.ErrCommandError, Err: err}
	}

	// networkingConfig is nil: the agent uses the default bridge (its published
	// ports are reachable on the host regardless of the user network, and cloud
	// replay has no cross-container networking because dependencies are mocked),
	// and the app shares the agent's namespace.
	created, err := a.docker.ContainerCreate(ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return "", models.AppError{AppErrorType: models.ErrCommandError,
			Err: fmt.Errorf("failed to create container %q: %w", name, err)}
	}
	// Track before start so teardown removes it even if start fails. Guarded:
	// a concurrent ComposeDownOnSetupFailure may snapshot+clear the stack.
	a.sdkTrack(sdkContainerRef{id: created.ID, name: name, service: svc.Key, isApp: agentID != ""})

	if err := a.docker.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", models.AppError{AppErrorType: models.ErrCommandError,
			Err: fmt.Errorf("failed to start container %q: %w", name, err)}
	}
	a.logger.Debug("started cloud-replay container via SDK",
		zap.String("service", svc.Key), zap.String("name", name), zap.String("id", created.ID))
	return created.ID, models.AppError{}
}

// buildContainerSpec translates a parsed compose service into the Docker SDK
// container.Config + container.HostConfig, faithfully reproducing what
// `docker compose up` would create for it. It is pure (no daemon) so the
// translation is unit-testable. agentID is empty for the agent (standalone:
// publishes ports, keeps DNS); for app services it is the agent's container id,
// so the app joins the agent's net/pid namespace and carries no ports/DNS.
func buildContainerSpec(svc docker.ServiceSpec, agentID string) (*container.Config, *container.HostConfig, error) {
	cfg := &container.Config{
		Image:      svc.Image,
		Env:        svc.Env,
		Labels:     composeSDKLabels(svc),
		WorkingDir: svc.WorkingDir,
	}
	if len(svc.Entrypoint) > 0 {
		cfg.Entrypoint = svc.Entrypoint
	}
	if len(svc.Command) > 0 {
		cfg.Cmd = svc.Command
	}
	if hc := svc.Healthcheck; hc != nil && len(hc.Test) > 0 {
		cfg.Healthcheck = &container.HealthConfig{
			Test:        hc.Test,
			Interval:    hc.Interval,
			Timeout:     hc.Timeout,
			Retries:     hc.Retries,
			StartPeriod: hc.StartPeriod,
		}
	}

	hostCfg := &container.HostConfig{
		Binds:       svc.Binds,
		CapAdd:      svc.CapAdd,
		SecurityOpt: svc.SecurityOpt,
		Tmpfs:       svc.Tmpfs,
		// Force restart=no. cloud replay emits `restart: unless-stopped`, but
		// under --abort-on-container-exit the daemon must NOT auto-restart the
		// app: it would break exit-code capture and leave the stack up.
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
	}

	if _, ok := docker.IsShareServiceNamespace(svc.PidMode); ok {
		hostCfg.PidMode = container.PidMode("container:" + agentID)
	}
	if _, shareNet := docker.IsShareServiceNamespace(svc.NetworkMode); shareNet {
		// Joining another container's network namespace forbids ports, DNS and
		// network attachment on this container (the daemon rejects them). All of
		// that lives on the agent, which owns the namespace.
		hostCfg.NetworkMode = container.NetworkMode("container:" + agentID)
	} else {
		hostCfg.DNS = svc.DNS
		hostCfg.DNSSearch = svc.DNSSearch
		hostCfg.DNSOptions = svc.DNSOpt
		if len(svc.Ports) > 0 {
			exposed, bindings, perr := nat.ParsePortSpecs(svc.Ports)
			if perr != nil {
				return nil, nil, fmt.Errorf("service %s: invalid ports %v: %w", svc.Key, svc.Ports, perr)
			}
			cfg.ExposedPorts = exposed
			hostCfg.PortBindings = bindings
		}
	}
	return cfg, hostCfg, nil
}

// waitAgentReady blocks until the keploy-agent reports ready, reproducing
// compose's `depends_on: keploy-agent: condition: service_healthy` gate: it
// polls the container's healthcheck AND, as a safety net, an HTTP /agent/ready
// probe, bounded by the agent's readiness budget (pkg.AgentReadyTimeout).
func (a *App) waitAgentReady(ctx context.Context, agentID string) error {
	deadline := time.Now().Add(pkg.AgentReadyTimeout())
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		info, err := a.docker.ContainerInspect(ctx, agentID)
		if err == nil && info.State != nil {
			if info.State.Status == container.StateExited || info.State.Status == container.StateDead {
				return fmt.Errorf("keploy-agent exited before becoming ready (status=%s, exitCode=%d)",
					info.State.Status, info.State.ExitCode)
			}
			if info.State.Health != nil && info.State.Health.Status == container.Healthy {
				return nil
			}
		} else if err != nil {
			a.logger.Debug("failed to inspect keploy-agent while waiting for readiness", zap.Error(err))
		}

		// Fallback: a 200 from /agent/ready means the agent is genuinely ready,
		// so this unblocks even if the container health binary is unavailable.
		if a.agentHTTPReady(ctx) {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for keploy-agent to become ready", pkg.AgentReadyTimeout())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// agentHTTPReady POSTs the agent readiness endpoint and reports whether it
// returned 200. Best-effort: any transport/URL error is treated as "not ready".
func (a *App) agentHTTPReady(ctx context.Context) bool {
	if a.opts.AgentPort == 0 {
		return false
	}
	client := &http.Client{Timeout: 2 * time.Second}
	// The router mounts the agent routes under /agent, so the readiness handler
	// (registered as POST /agent/ready) is reachable at /agent/agent/ready; try
	// the unprefixed path too in case the mount changes.
	for _, path := range []string{"/agent/agent/ready", "/agent/ready"} {
		url := fmt.Sprintf("http://127.0.0.1:%d%s", a.opts.AgentPort, path)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return true
		}
	}
	return false
}

// sdkWaitForApp blocks until the app container (the --exit-code-from target)
// exits, mapping its exit code to a models.AppError the same way the CLI path
// maps `docker compose up`'s exit.
func (a *App) sdkWaitForApp(ctx context.Context) models.AppError {
	waitName := a.container
	if waitName == "" {
		// Defensive: fall back to the first app container if --container-name was
		// somehow unset (cloud replay always sets it). Snapshot under the lock —
		// a concurrent ComposeDownOnSetupFailure may drain the stack.
		for _, c := range a.sdkSnapshotStack() {
			if c.isApp {
				waitName = c.name
				break
			}
		}
	}

	waitCh, errCh := a.docker.ContainerWait(ctx, waitName, container.WaitConditionNotRunning)
	select {
	case <-ctx.Done():
		a.logger.Debug("context cancelled while waiting for the cloud-replay app to exit", zap.Error(ctx.Err()))
		return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: ctx.Err()}
	case werr := <-errCh:
		if ctx.Err() != nil {
			return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: ctx.Err()}
		}
		return models.AppError{AppErrorType: models.ErrUnExpected,
			Err:     fmt.Errorf("failed while waiting for the cloud-replay app container: %w", werr),
			AppLogs: a.recentAppLogs(ctx)}
	case res := <-waitCh:
		appLogs := a.recentAppLogs(ctx)
		if res.Error != nil && res.Error.Message != "" {
			return models.AppError{AppErrorType: models.ErrUnExpected,
				Err: fmt.Errorf("cloud-replay app wait reported an error: %s", res.Error.Message), AppLogs: appLogs}
		}
		if res.StatusCode != 0 {
			return models.AppError{AppErrorType: models.ErrUnExpected,
				Err: fmt.Errorf("cloud-replay app container exited with code %d", res.StatusCode), AppLogs: appLogs}
		}
		return models.AppError{AppErrorType: models.ErrAppStopped, Err: nil, AppLogs: appLogs}
	}
}

// composeSDKTeardown stops and removes the containers the SDK orchestrator
// created, reproducing `docker compose down` (WITHOUT -v, so named volumes are
// kept). It is idempotent and bounded on its own deadlines so it never overruns
// the record/replay teardown-drain budget, even on a saturated daemon. Routed
// to from ComposeDown's in-memory branch.
func (a *App) composeSDKTeardown() {
	// One hard deadline for the WHOLE teardown so an O(N-container) stack (a
	// multi-container deployment) can never overrun the record/replay
	// teardown-drain budget (utils.DrainErrGroup, 30s). Every per-container op
	// derives its context from this parent, so once the deadline passes the
	// remaining ops fail fast rather than each burning its own budget.
	ctx, cancel := context.WithTimeout(context.Background(), sdkTeardownBudget)
	defer cancel()

	// Snapshot + clear atomically so a concurrent ComposeDownOnSetupFailure and
	// the run()-deferred ComposeDown don't double-process or race the slice; the
	// second caller sees an empty stack and falls through to the name backstop.
	stack := a.sdkDrainStack()

	// Stop the app container(s) first with a coverage-flush grace (SIGINT +
	// grace), so Go's GOCOVERDIR / Java's jacoco land on the host-mounted paths
	// before removal. Mirrors the CLI path's cmdCancel grace.
	graceSecs := int(graceBudget / time.Second)
	for _, c := range stack {
		if c.isApp {
			a.sdkStopContainer(ctx, c.id, "SIGINT", &graceSecs)
		}
	}

	// Then stop (fast) the non-app containers and remove everything. The app is
	// already stopped (with grace) above, so it is only removed here.
	zero := 0
	for _, c := range stack {
		if !c.isApp {
			a.sdkStopContainer(ctx, c.id, "", &zero)
		}
		a.sdkRemoveContainer(ctx, c.id)
	}

	// Backstop: force-remove the agent + app by name. This is the only cleanup
	// when teardown ran before a successful up (empty stack), and it also closes
	// the setup-failure race window where runComposeSDK started a container AFTER
	// this teardown snapshotted the stack (that container would otherwise leak
	// and collide with the next run's create). Idempotent.
	a.sdkForceRemoveByName(ctx, a.container)
	a.sdkForceRemoveByName(ctx, a.keployContainer)
}

func (a *App) sdkStopContainer(parent context.Context, id, signal string, timeoutSecs *int) {
	ctx, cancel := context.WithTimeout(parent, composeDownCmdBudget)
	defer cancel()
	if err := a.docker.ContainerStop(ctx, id, container.StopOptions{Signal: signal, Timeout: timeoutSecs}); err != nil {
		a.logger.Debug("SDK container stop finished (may already be stopped, or the bounded deadline elapsed)",
			zap.String("id", id), zap.Error(err))
	}
}

func (a *App) sdkRemoveContainer(parent context.Context, id string) {
	ctx, cancel := context.WithTimeout(parent, forceRemoveBudget)
	defer cancel()
	if err := a.docker.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		a.logger.Debug("SDK container remove finished (may already be gone, or the bounded deadline elapsed)",
			zap.String("id", id), zap.Error(err))
	}
}

func (a *App) sdkForceRemoveByName(parent context.Context, name string) {
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, forceRemoveBudget)
	defer cancel()
	if err := a.docker.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil {
		a.logger.Debug("SDK force-remove by name finished (may already be gone)",
			zap.String("container", name), zap.Error(err))
	}
}

// sdkPreClean force-removes any leftover containers (by name) from a prior run
// so create cannot hit a name conflict. The SDK equivalent of the CLI path's
// removeStaleComposeAgentWithin/ensureContainerNameFreeWithin, minus the
// compose-`ps` dance (there is no compose project to reconcile).
func (a *App) sdkPreClean(ctx context.Context, model *docker.ComposeModel) {
	seen := map[string]bool{}
	remove := func(n string) {
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		a.sdkForceRemoveByName(ctx, n)
	}
	remove(a.container)
	remove(a.keployContainer)
	for _, s := range model.Services {
		remove(s.ContainerName)
	}
}

// sdkEnsureVolumes pre-creates the top-level named volumes (idempotent). Docker
// auto-creates named volumes referenced in Binds, but creating them up front
// avoids a create race between the agent and app mounting the same volume.
func (a *App) sdkEnsureVolumes(ctx context.Context, model *docker.ComposeModel) {
	for name, spec := range model.Volumes {
		if spec.External {
			continue
		}
		volName := name
		if spec.Name != "" {
			volName = spec.Name
		}
		opts := volumetypes.CreateOptions{Name: volName, Labels: map[string]string{
			"com.docker.compose.project": sdkComposeProject,
			"com.docker.compose.volume":  name,
		}}
		if spec.Driver != "" {
			opts.Driver = spec.Driver
		}
		if _, err := a.docker.VolumeCreate(ctx, opts); err != nil {
			a.logger.Debug("failed to pre-create named volume (continuing; docker auto-creates it on mount)",
				zap.String("volume", volName), zap.Error(err))
		}
	}
}

// sdkEnsureImage pulls the service image if it is not already present locally,
// resolving registry credentials from the host docker config (matching what the
// compose CLI does for `up`'s implicit pull). If the image is present it is a
// no-op.
func (a *App) sdkEnsureImage(ctx context.Context, ref string) error {
	if ref == "" {
		return fmt.Errorf("service has no image")
	}
	images, err := a.docker.ImageList(ctx, imagetypes.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", ref)),
	})
	if err == nil && len(images) > 0 {
		return nil
	}

	a.logger.Info("Pulling image for cloud replay", zap.String("image", ref))
	rc, perr := a.docker.ImagePull(ctx, ref, imagetypes.PullOptions{RegistryAuth: a.sdkResolveAuth(ref)})
	if perr != nil {
		return fmt.Errorf("failed to pull image %q (pre-pull it or check registry credentials): %w", ref, perr)
	}
	defer func() { _ = rc.Close() }()
	// Draining the stream blocks until the pull completes (or fails).
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("failed while pulling image %q: %w", ref, err)
	}
	return nil
}

// sdkResolveAuth returns the base64 X-Registry-Auth header for ref's registry,
// resolved from the host docker config's static `auths` entries. Credential
// helpers (credsStore/credHelpers) are intentionally not invoked (they require
// executing helper binaries, which the shell-free cloud image cannot do); a
// private image behind a cred helper must be pre-pulled. Returns "" when no
// static credential is found (anonymous pull).
func (a *App) sdkResolveAuth(ref string) string {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return ""
	}
	domain := reference.Domain(named)

	cfgDir := os.Getenv("DOCKER_CONFIG")
	if cfgDir == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return ""
		}
		cfgDir = filepath.Join(home, ".docker")
	}
	data, err := os.ReadFile(filepath.Join(cfgDir, "config.json"))
	if err != nil {
		return ""
	}

	var cfg struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			Username      string `json:"username"`
			Password      string `json:"password"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}

	entry, ok := cfg.Auths[domain]
	if !ok {
		for _, alt := range dockerHubAuthAliases(domain) {
			if e, found := cfg.Auths[alt]; found {
				entry, ok = e, true
				break
			}
		}
	}
	if !ok {
		return ""
	}

	ac := registry.AuthConfig{
		ServerAddress: domain,
		Username:      entry.Username,
		Password:      entry.Password,
		IdentityToken: entry.IdentityToken,
	}
	if entry.Auth != "" && (ac.Username == "" || ac.Password == "") {
		if dec, derr := base64.StdEncoding.DecodeString(entry.Auth); derr == nil {
			if u, p, found := strings.Cut(string(dec), ":"); found {
				ac.Username, ac.Password = u, p
			}
		}
	}
	encoded, err := registry.EncodeAuthConfig(ac)
	if err != nil {
		return ""
	}
	return encoded
}

// dockerHubAuthAliases lists the config.json keys Docker Hub credentials are
// stored under, since reference.Domain normalises hub images to "docker.io".
func dockerHubAuthAliases(domain string) []string {
	if domain == "docker.io" || domain == "registry-1.docker.io" || domain == "index.docker.io" {
		return []string{"https://index.docker.io/v1/", "index.docker.io", "registry-1.docker.io", "docker.io"}
	}
	return nil
}

func composeSDKLabels(svc docker.ServiceSpec) map[string]string {
	labels := map[string]string{}
	for k, v := range svc.Labels {
		labels[k] = v
	}
	// Compose-parity labels so logTargetContainer/recentAppLogs' label filter
	// keeps resolving the app container.
	labels["com.docker.compose.project"] = sdkComposeProject
	labels["com.docker.compose.service"] = svc.Key
	labels["io.keploy.cloud-replay"] = "true"
	return labels
}

// startSDKLogPumps streams each stack container's logs to the terminal (compose
// up streams both the agent and app). Pumps exit when logCtx is cancelled.
func (a *App) startSDKLogPumps(logCtx context.Context) {
	for _, c := range a.sdkSnapshotStack() {
		go a.pumpContainerLogs(logCtx, c.id, c.service)
	}
}

// sdkTrack appends a created container to the tracked stack under the lock.
func (a *App) sdkTrack(ref sdkContainerRef) {
	a.sdkStackMu.Lock()
	a.sdkStack = append(a.sdkStack, ref)
	a.sdkStackMu.Unlock()
}

// sdkSnapshotStack returns a copy of the tracked stack under the lock.
func (a *App) sdkSnapshotStack() []sdkContainerRef {
	a.sdkStackMu.Lock()
	defer a.sdkStackMu.Unlock()
	return append([]sdkContainerRef(nil), a.sdkStack...)
}

// sdkDrainStack atomically returns the tracked stack and clears it, so only one
// teardown caller ever processes a given container.
func (a *App) sdkDrainStack() []sdkContainerRef {
	a.sdkStackMu.Lock()
	defer a.sdkStackMu.Unlock()
	stack := a.sdkStack
	a.sdkStack = nil
	return stack
}

func (a *App) pumpContainerLogs(ctx context.Context, id, service string) {
	rc, err := a.docker.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Tail:       "all",
	})
	if err != nil {
		a.logger.Debug("failed to stream cloud-replay container logs", zap.String("service", service), zap.Error(err))
		return
	}
	defer func() { _ = rc.Close() }()

	var mu sync.Mutex
	prefix := service + " | "
	outW := newPrefixWriter(os.Stdout, prefix, &mu)
	errW := newPrefixWriter(os.Stdout, prefix, &mu)
	// StdCopy demuxes docker's multiplexed stdout/stderr stream.
	if _, err := stdcopy.StdCopy(outW, errW, rc); err != nil && ctx.Err() == nil {
		a.logger.Debug("cloud-replay container log stream ended", zap.String("service", service), zap.Error(err))
	}
}

// prefixWriter prepends a fixed prefix at the start of every line it writes,
// so interleaved multi-container logs stay readable (like compose's
// "<service> | " prefixing). The shared mutex serialises the container's stdout
// and stderr writers onto the same underlying os.Stdout.
type prefixWriter struct {
	w      io.Writer
	prefix []byte
	mu     *sync.Mutex
	atBOL  bool
}

func newPrefixWriter(w io.Writer, prefix string, mu *sync.Mutex) *prefixWriter {
	return &prefixWriter{w: w, prefix: []byte(prefix), mu: mu, atBOL: true}
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := len(b)
	for len(b) > 0 {
		if p.atBOL {
			if _, err := p.w.Write(p.prefix); err != nil {
				return total - len(b), err
			}
			p.atBOL = false
		}
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			if _, err := p.w.Write(b[:i+1]); err != nil {
				return total - len(b), err
			}
			p.atBOL = true
			b = b[i+1:]
			continue
		}
		if _, err := p.w.Write(b); err != nil {
			return total - len(b), err
		}
		break
	}
	return total, nil
}
