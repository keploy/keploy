// Package app provides functionality for managing applications.
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/pkg/stdcopy"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"

	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func NewApp(logger *zap.Logger, cmd string, client docker.Client, opts models.SetupOptions) *App {
	app := &App{
		logger:          logger,
		cmd:             cmd,
		docker:          client,
		kind:            utils.FindDockerCmd(cmd),
		opts:            opts,
		keployContainer: opts.KeployContainer,
		container:       opts.Container,
	}
	return app
}

type App struct {
	logger          *zap.Logger
	docker          docker.Client
	cmd             string
	kind            utils.CmdType
	opts            models.SetupOptions
	container       string
	composeService  string
	keployContainer string
	composeFile     string // path to the temp compose file (set during SetupCompose)
	composeContent  []byte // in-memory compose YAML; set when InMemoryCompose is used
	EnableTesting   bool
	Mode            models.Mode
}

func (a *App) Setup(ctx context.Context) error {

	if utils.IsDockerCmd(a.kind) && isDetachMode(a.logger, a.cmd, a.kind) {
		return fmt.Errorf("application could not be started in detached mode")
	}

	switch a.kind {
	case utils.DockerRun, utils.DockerStart:
		err := a.SetupDocker()
		if err != nil {
			return err
		}
	case utils.DockerCompose:
		extraArgs := agent.StartupAgentHook.GetArgs(ctx)
		err := a.SetupCompose(extraArgs)
		if err != nil {
			return err
		}
	default:
		// setup native binary
	}
	return nil
}

func (a *App) SetupDocker() error {

	if a.kind == utils.DockerStart {
		running, err := a.docker.IsContainerRunning(a.container)
		if err != nil {
			return err
		}
		if running {
			return fmt.Errorf("docker container is already in running state")
		}
	}

	a.logger.Debug("inside setup docker", zap.String("cmd", a.cmd))

	if HookImpl != nil {
		newCmd, err := HookImpl.BeforeDockerSetup(context.Background(), a.cmd)
		if err != nil {
			utils.LogError(a.logger, err, "hook failed during docker setup")
			return err
		}
		a.cmd = newCmd
	}

	a.logger.Debug("after before docker setup hook", zap.String("cmd", a.cmd))

	// attaching the init container's PID namespace to the app container
	err := a.modifyDockerRun(context.Background())
	if err != nil {
		utils.LogError(a.logger, err, "failed to attach init pid")
		return err
	}
	return nil
}

// ModifyDockerRun modifies the existing Docker command to attach the init container's PID namespace
func (a *App) modifyDockerRun(_ context.Context) error {
	if a.cmd == "" {
		return fmt.Errorf("no command provided to modify")
	}
	// Attach the keploy agent container's PID namespace and network namespace to the app container
	// Sharing network namespace so that the docker container IPs of both agent and app remain same
	// sharing pid namespace so that they share the same process tree (common ancestor) in docker
	pidMode := fmt.Sprintf("--pid=container:%s", a.keployContainer)
	networkMode := fmt.Sprintf("--network=container:%s", a.keployContainer)

	keployTLSVolumeName := "keploy-tls-certs"
	keployTLSMountPath := "/tmp/keploy-tls"
	certPath := fmt.Sprintf("%s/ca.crt", keployTLSMountPath)
	trustStorePath := fmt.Sprintf("%s/truststore.jks", keployTLSMountPath)

	tlsFlags := fmt.Sprintf("-v %s:%s:ro ", keployTLSVolumeName, keployTLSMountPath)
	tlsFlags += fmt.Sprintf("-e NODE_EXTRA_CA_CERTS=%s ", certPath)
	tlsFlags += fmt.Sprintf("-e REQUESTS_CA_BUNDLE=%s ", certPath)
	tlsFlags += fmt.Sprintf("-e SSL_CERT_FILE=%s ", certPath)
	tlsFlags += fmt.Sprintf("-e CARGO_HTTP_CAINFO=%s ", certPath)
	// For Java, we append to existing options if possible, or just set it.
	// In CLI args, setting it blindly is usually safe as it overrides or adds.
	// Ideally we would check if -e JAVA_TOOL_OPTIONS exists, but for now:
	javaOpts := fmt.Sprintf("-Djavax.net.ssl.trustStore=%s -Djavax.net.ssl.trustStorePassword=changeit", trustStorePath)
	tlsFlags += fmt.Sprintf("-e JAVA_TOOL_OPTIONS='%s' ", javaOpts)

	// Inject the pidMode flag after 'docker run' in the command
	parts := strings.SplitN(a.cmd, " ", 3) // Split by first two spaces to isolate "docker run"
	if len(parts) < 3 {
		return fmt.Errorf("invalid command structure: %s", a.cmd)
	}

	injection := fmt.Sprintf("%s %s %s", pidMode, networkMode, tlsFlags)

	// Modify the command to insert the pidMode and environment variables
	a.cmd = fmt.Sprintf("%s %s %s %s", parts[0], parts[1], injection, parts[2])
	a.logger.Debug("added network namespace and pid to docker command", zap.String("cmd", a.cmd))
	return nil
}

func (a *App) SetupCompose(extraArgs []string) error {
	if a.container == "" {
		utils.LogError(a.logger, nil, "container name not found", zap.String("AppCmd", a.cmd))
		return errors.New("container name not found")
	}

	// In-memory path: compose content was provided directly (e.g. from the enterprise
	// cloud command) to avoid writing sensitive env vars to disk.
	if len(a.opts.InMemoryCompose) > 0 {
		return a.setupComposeInMemory(extraArgs)
	}

	// In SetupCompose, first we try to find all the docker compose file paths in the current context.
	// Then, we find the compose file which contains the user app container.
	// After that, we add the keploy agent service in a copy of the found user app compose file (by creating docker-compose-tmp.yaml).
	// Finally, we use this modified docker compose file in place of the user's original compose file to run the application with keploy integration.

	paths := findComposeFile(a.cmd)
	if len(paths) == 0 {
		return errors.New("can't find the docker compose file of user. Are you in the right directory? ")
	}

	a.logger.Debug(fmt.Sprintf("Found docker compose file paths: %v", paths))

	newPath := "docker-compose-tmp.yaml"

	serviceInfo, err := a.docker.FindContainerInComposeFiles(paths, a.container)
	if err != nil {
		utils.LogError(a.logger, err, "failed to find container in compose files")
		return err
	}

	if serviceInfo == nil {
		utils.LogError(a.logger, nil, "container not found in any of the compose files", zap.Strings("composePaths", paths), zap.String("container", a.container))
		return fmt.Errorf("container:%v not found in any of the compose files", a.container)
	}

	a.opts.AppPorts = serviceInfo.Ports
	a.opts.AppNetworks = serviceInfo.Networks
	a.opts.ExtraArgs = extraArgs
	a.composeService = serviceInfo.AppServiceName
	compose := serviceInfo.Compose

	err = a.docker.ModifyComposeForAgent(compose, a.opts, a.container)
	if err != nil {
		utils.LogError(a.logger, err, "failed to modify compose for keploy integration")
		return err
	}
	if HookImpl != nil {
		changed, err := HookImpl.BeforeDockerComposeSetup(context.Background(), compose, a.container)
		if err != nil {
			utils.LogError(a.logger, err, "hook failed during docker compose setup")
			return err
		}
		if changed {
			a.logger.Debug("Successfully ran BeforeDockerComposeSetup hook and modified volumes")
		}
	}
	err = a.docker.WriteComposeFile(compose, newPath)
	if err != nil {
		utils.LogError(a.logger, nil, "failed to write the compose file", zap.String("path", newPath))
	}
	a.composeFile = newPath
	a.logger.Debug("Created new temporary docker-compose for keploy internal use", zap.String("path", newPath))

	// Now replace the running command to run the docker-compose-tmp.yaml file instead of user docker compose file.
	a.cmd = modifyDockerComposeCommand(a.cmd, newPath, serviceInfo.ComposePath, serviceInfo.AppServiceName)

	a.logger.Info(
		"Running application using a temporary Keploy-generated Docker Compose file (will be cleaned up automatically)",
		zap.String("cmd", a.cmd),
		zap.String("composePath", newPath),
	)

	return nil
}

// setupComposeInMemory handles the in-memory variant of SetupCompose. It parses the
// compose YAML that was passed via opts.InMemoryCompose (never written to disk),
// injects the keploy-agent service, serialises the result back to bytes, and
// configures the command to pipe the content via stdin ("-f -").
func (a *App) setupComposeInMemory(extraArgs []string) error {
	var compose docker.Compose
	if err := yaml.Unmarshal(a.opts.InMemoryCompose, &compose); err != nil {
		return fmt.Errorf("failed to parse in-memory compose YAML: %w", err)
	}

	serviceInfo, err := a.docker.FindContainerInCompose(&compose, a.container)
	if err != nil {
		utils.LogError(a.logger, err, "failed to find container in in-memory compose")
		return err
	}

	a.opts.AppPorts = serviceInfo.Ports
	a.opts.AppNetworks = serviceInfo.Networks
	a.opts.ExtraArgs = extraArgs
	a.composeService = serviceInfo.AppServiceName

	if err := a.docker.ModifyComposeForAgent(&compose, a.opts, a.container); err != nil {
		utils.LogError(a.logger, err, "failed to modify in-memory compose for keploy integration")
		return err
	}

	if HookImpl != nil {
		changed, err := HookImpl.BeforeDockerComposeSetup(context.Background(), &compose, a.container)
		if err != nil {
			utils.LogError(a.logger, err, "hook failed during in-memory docker compose setup")
			return err
		}
		if changed {
			a.logger.Debug("Successfully ran BeforeDockerComposeSetup hook and modified volumes")
		}
	}

	content, err := a.docker.MarshalCompose(&compose)
	if err != nil {
		return fmt.Errorf("failed to serialise modified compose to YAML: %w", err)
	}
	a.composeContent = content

	// Ensure the command uses stdin ("-f -") and has the exit-code-from flags.
	a.cmd = ensureInMemoryComposeFlags(a.cmd, a.composeService)

	a.logger.Info("Running application using in-memory Keploy-generated Docker Compose (no file written to disk)",
		zap.String("cmd", a.cmd))

	return nil
}

func (a *App) SetAppCommand(appCommand string) {
	a.logger.Debug("Setting App Command", zap.String("cmd", appCommand))
	a.cmd = appCommand
}

func (a *App) GetAppCommand() string {
	return a.cmd
}

func (a *App) Kind(_ context.Context) utils.CmdType {
	return a.kind
}

func (a *App) Run(ctx context.Context) models.AppError {
	return a.run(ctx)
}

func (a *App) RecentLogs(ctx context.Context) string {
	return a.recentAppLogs(ctx)
}

func (a *App) waitTillExit() {
	// Bounded overall (waitTillExitBudget) AND per-inspect (dockerInspectBudget)
	// so a saturated daemon can't wedge this on the teardown goroutine — an
	// unbounded ContainerInspect here would also block the select so even the
	// overall timeout couldn't fire. Only reached on the non-compose docker path.
	timeout := time.NewTimer(waitTillExitBudget)
	logTicker := time.NewTicker(1 * time.Second)
	defer logTicker.Stop()
	defer timeout.Stop()

	containerID := a.container
	for {
		select {
		case <-logTicker.C:
			// Inspect the container status (bounded; see the note above)
			ictx, icancel := context.WithTimeout(context.Background(), dockerInspectBudget)
			containerJSON, err := a.docker.ContainerInspect(ictx, containerID)
			icancel()
			if err != nil {
				a.logger.Debug("failed to inspect container", zap.String("containerID", containerID), zap.Error(err))
				return
			}

			a.logger.Debug("container status", zap.String("status", containerJSON.State.Status), zap.String("containerName", a.container))
			// Check if container is stopped or dead
			if containerJSON.State.Status == "exited" || containerJSON.State.Status == "dead" {
				return
			}
		case <-timeout.C:
			a.logger.Debug("timeout waiting for the container to stop", zap.String("containerID", containerID))
			return
		}
	}
}

func (a *App) recentAppLogs(ctx context.Context) string {
	if !utils.IsDockerCmd(a.kind) {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	logTarget := a.logTargetContainer(ctx)
	if logTarget == "" {
		return ""
	}

	logReader, err := a.docker.ContainerLogs(ctx, logTarget, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       "50",
	})
	if err != nil {
		a.logger.Debug("failed to fetch recent app logs", zap.String("container", logTarget), zap.Error(err))
		return ""
	}
	defer func() {
		if closeErr := logReader.Close(); closeErr != nil {
			a.logger.Debug("failed to close recent app logs reader", zap.String("container", logTarget), zap.Error(closeErr))
		}
	}()

	rawLogs, err := io.ReadAll(logReader)
	if err != nil {
		a.logger.Debug("failed to read recent app logs", zap.String("container", logTarget), zap.Error(err))
		return ""
	}

	var stdoutBuf, stderrBuf bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdoutBuf, &stderrBuf, bytes.NewReader(rawLogs)); err != nil {
		return trimRecentAppLogs(string(rawLogs), 20)
	}

	combined := strings.TrimSpace(stdoutBuf.String())
	stderrLogs := strings.TrimSpace(stderrBuf.String())
	if stderrLogs != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += stderrLogs
	}

	return trimRecentAppLogs(combined, 20)
}

func (a *App) logTargetContainer(ctx context.Context) string {
	if !utils.IsDockerCmd(a.kind) {
		return ""
	}
	if a.kind != utils.DockerCompose || a.composeService == "" {
		return a.container
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if projectLabel := a.composeProjectLabel(ctx); projectLabel != "" {
		projectArgs := filters.NewArgs(
			filters.Arg("label", "com.docker.compose.service="+a.composeService),
			filters.Arg("label", "com.docker.compose.project="+projectLabel),
		)
		containers, err := a.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: projectArgs})
		if err != nil {
			a.logger.Debug(
				"failed to list compose containers for recent app logs using compose project label; falling back to service-only matching",
				zap.String("service", a.composeService),
				zap.String("project", projectLabel),
				zap.Error(err),
			)
		} else if len(containers) > 0 {
			sort.Slice(containers, func(i, j int) bool {
				return containers[i].Created > containers[j].Created
			})
			return containers[0].ID
		}
	}

	args := filters.NewArgs(filters.Arg("label", "com.docker.compose.service="+a.composeService))
	containers, err := a.docker.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		a.logger.Debug("failed to list compose containers for recent app logs", zap.String("service", a.composeService), zap.Error(err))
		return a.container
	}
	if len(containers) == 0 {
		return a.container
	}
	sort.Slice(containers, func(i, j int) bool {
		return containers[i].Created > containers[j].Created
	})

	return containers[0].ID
}

func (a *App) composeProjectLabel(ctx context.Context) string {
	if a.container == "" {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}

	containerInfo, err := a.docker.ContainerInspect(ctx, a.container)
	if err != nil {
		a.logger.Debug(
			"failed to inspect compose container for project label when fetching recent app logs; falling back to service-only matching",
			zap.String("service", a.composeService),
			zap.String("container", a.container),
			zap.Error(err),
		)
		return ""
	}
	if containerInfo.Config == nil {
		return ""
	}

	return strings.TrimSpace(containerInfo.Config.Labels["com.docker.compose.project"])
}

func trimRecentAppLogs(logs string, maxLines int) string {
	if logs == "" || maxLines <= 0 {
		return ""
	}

	lines := strings.Split(strings.ReplaceAll(logs, "\r\n", "\n"), "\n")
	meaningful := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(sanitizeAppLogLine(line))
		if trimmed == "" {
			continue
		}
		meaningful = append(meaningful, trimmed)
	}

	if len(meaningful) > maxLines {
		meaningful = meaningful[len(meaningful)-maxLines:]
	}

	return strings.Join(meaningful, "\n")
}

func sanitizeAppLogLine(line string) string {
	line = ansiEscapePattern.ReplaceAllString(line, "")
	line = strings.ReplaceAll(line, "\t", "  ")
	return strings.TrimSpace(line)
}

// ComposeDown runs docker compose down to remove all containers and networks
// created by the compose stack. Without this, stopped containers retain
// references to image layers; a subsequent docker image prune can delete
// those layers and corrupt Docker Desktop's overlayfs snapshots. Exported so the
// replay loop can tear the stack down on a per-test-set setup failure
// (agent-readiness timeout) before a retry, not only via run()'s defer.
func (a *App) ComposeDown() {
	// Bound each docker CLI call below on its OWN deadline so a daemon saturated
	// by a high-stress CI run cannot wedge the teardown. The record/replay
	// teardown drains the app-runner goroutine under utils.DrainErrGroup's 30s
	// budget; an UNBOUNDED `docker compose down` / `docker rm -f` blocked that
	// goroutine well past 30s, tripping a spurious "teardown drain timed out … a
	// goroutine is ignoring context cancellation" error on an otherwise-green
	// run. (`--timeout 1` only bounds compose's per-container *stop* grace, not
	// the `docker compose down` process itself.) Per-call budgets — not one
	// shared deadline — so the force-remove + reap barrier still free the
	// agent/app container names even when the down consumes its full budget
	// (that name-freeing is exactly what avoids "container name already in use"
	// on the next compose up).
	downCtx, downCancel := context.WithTimeout(context.Background(), composeDownCmdBudget)
	defer downCancel()

	var downCmd *exec.Cmd

	switch {
	case len(a.composeContent) > 0:
		// In-memory mode: pipe compose YAML via stdin, no file on disk.
		// Preserve project-scoping flags (-p/--project-name, --project-directory)
		// from the original command so teardown targets the correct project.
		a.logger.Debug("Running docker compose down using in-memory compose content")
		args := []string{"compose", "-f", "-"}
		args = append(args, extractProjectFlags(a.cmd)...)
		// --timeout 1: stop containers fast instead of the default 10s graceful
		// wait PER container. Between test-sets `keploy test` restarts the whole
		// compose stack; under loaded CI a slow `down` was exceeding the
		// per-test-set teardown drain budget and being abandoned, leaving a
		// half-removed agent container that the next test-set's `up` then
		// collided with ("container name already in use" -> dependency
		// keploy-agent failed to start -> test-set abandoned, no report).
		args = append(args, "down", "--timeout", "1")
		downCmd = exec.CommandContext(downCtx, "docker", args...)
		downCmd.Stdin = bytes.NewReader(a.composeContent)
	case a.composeFile != "":
		a.logger.Debug("Running docker compose down to clean up containers and networks",
			zap.String("composeFile", a.composeFile))
		downCmd = exec.CommandContext(downCtx, "docker", "compose", "-f", a.composeFile, "down", "--timeout", "1")
	default:
		return
	}

	if output, err := downCmd.CombinedOutput(); err != nil {
		a.logger.Debug("docker compose down finished with error (may be expected if containers already removed, or the bounded teardown deadline elapsed under load)",
			zap.Error(err), zap.String("output", string(output)))
	}

	// Coverage safety: this runs only AFTER the app has already been stopped by
	// run()'s cmdCancel (SIGINT + grace period, force-kill only if it doesn't
	// exit), so the app's coverage flush has already happened — Go writes
	// GOCOVERDIR, Java its jacoco .exec (both to host-mounted paths that survive
	// container removal), and the Java SDK streams coverage over a socket during
	// the run. The --timeout 1 above and the force-remove below therefore only
	// reclaim already-stopped containers; they never shorten a runtime's
	// coverage-flush window (that window is cmdCancel's grace, not down).
	//
	// Belt-and-suspenders: guarantee the agent + app container names are free
	// before the next test-set's compose up reuses them. `compose down` already
	// removes them, but if it errored or was cut short under load the names
	// could linger and the next up would hit "container name already in use".
	// A force-remove by name is near-instant and idempotent.
	a.forceRemoveContainerByName(a.keployContainer)
	a.forceRemoveContainerByName(a.container)

	// Reap-barrier: `compose down` / `rm -f` can return before the daemon has
	// finished reaping under load, so the next compose up's same-name create
	// races the async reap and stalls at "Creating". Poll until the names are
	// gone, BOUNDED so it never reintroduces the unbounded teardown that
	// --timeout 1 removed. Largely a no-op once the stack is reused across
	// test-sets (one down per lane), but defends the first/last and
	// non-keep-alive boundaries.
	a.waitContainersRemoved([]string{a.keployContainer, a.container}, reapBarrierBudget)
}

// Teardown budgets. The record/replay teardown drains the app-runner goroutine
// under utils.DrainErrGroup's 30s budget (pkg/service/record/record.go,
// pkg/service/replay/replay.go). Every docker CLI call on the teardown path is
// bounded so the goroutine returns well inside 30s even when the daemon is
// saturated. Worst-case compose teardown (run()'s cmdCancel): the grace loop
// (graceBudget + up to one last-iteration overshoot of sleep+inspect ≈ 2.5s,
// since the deadline is checked at the loop top) + composeDownCmdBudget +
// 2×forceRemoveBudget + reapBarrierBudget ≈ (5+2.5) + 8 + 2×2 + 3 ≈ 22.5s;
// waitTillExit is then skipped on the compose path and the ctx-cancelled run
// returns before recentAppLogs — comfortably under 30s.
const (
	composeDownCmdBudget = 8 * time.Second // `docker compose down`
	forceRemoveBudget    = 2 * time.Second // each `docker rm -f` in the teardown drain path
	reapBarrierBudget    = 3 * time.Second // waitContainersRemoved poll
	graceBudget          = 5 * time.Second // cmdCancel coverage-flush grace
	waitTillExitBudget   = 8 * time.Second // non-compose post-cancel container wait
	dockerInspectBudget  = 2 * time.Second // any single teardown ContainerInspect
	// preRunRemoveBudget bounds the startup-time force-remove of a leftover
	// --name container before a docker-run. Unlike the teardown force-remove it
	// is NOT in the SIGINT drain path, so it can afford to wait for a still-
	// running prior container to actually go away under daemon contention; the
	// 2s teardown cap is too short here and lets "name already in use" through.
	preRunRemoveBudget = 30 * time.Second
	// dockerRunNameConflictRetries bounds how many times a user-app `docker run`
	// is re-issued after a transient container-name conflict (docker exit 125)
	// before the error is surfaced; the name is force-removed between attempts.
	dockerRunNameConflictRetries = 3
)

// waitContainersRemoved polls until the named containers no longer exist (or the
// budget elapses), converting the daemon's async reap into a bounded synchronous
// barrier so the next compose up cannot race a still-removing same-named
// container.
func (a *App) waitContainersRemoved(names []string, budget time.Duration) {
	// Bounded on its own deadline so the reap barrier can never push ComposeDown
	// past the teardown budget.
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		allGone := true
		for _, n := range names {
			if n == "" {
				continue
			}
			// ContainerInspect returns a non-nil error once the container is
			// gone; treat any error as "no longer blocking" (best-effort barrier).
			if _, err := a.docker.ContainerInspect(ctx, n); err == nil {
				allGone = false // still present
				break
			}
		}
		if allGone {
			return
		}
		select {
		case <-ctx.Done():
			a.logger.Debug("waitContainersRemoved: timed out waiting for reap", zap.Strings("containers", names))
			return
		case <-ticker.C:
		}
	}
}

// forceRemoveContainerByName removes a container by name, ignoring the
// "no such container" case. Used after compose down to guarantee a name is free
// for reuse on the next per-test-set compose up.
func (a *App) forceRemoveContainerByName(name string) {
	a.forceRemoveContainerByNameWithin(name, forceRemoveBudget)
}

// forceRemoveContainerByNameWithin is forceRemoveContainerByName with a caller-
// chosen deadline. Teardown callers pass the tight forceRemoveBudget (drain
// path); the startup pre-run cleanup passes preRunRemoveBudget so it can wait
// out a still-running prior container under contention.
func (a *App) forceRemoveContainerByNameWithin(name string, budget time.Duration) {
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput(); err != nil {
		a.logger.Debug("force-remove container finished (may already be gone, or the bounded deadline elapsed)",
			zap.String("container", name), zap.Error(err), zap.String("output", string(output)))
	}
}

// ensureContainerNameFreeWithin force-removes the named container and then
// VERIFIES the name is actually free before returning, retrying within the
// budget. A bare `docker rm -f` returns as soon as it has *initiated* removal,
// but under docker-daemon contention the `--rm` async reaper (kicked off when
// the prior per-test-set container exits) can keep holding the name for a brief
// window after that — long enough for the immediately following
// `docker run --name` to hit "Conflict. The container name ... is already in
// use" (docker exit 125), which fails the test set. Polling `docker ps` until
// the name is gone closes that window deterministically instead of racing into
// the conflict. Best-effort: if the name still isn't free at the deadline we
// warn and proceed (a still-stuck name is a deeper docker problem the run will
// surface anyway).
func (a *App) ensureContainerNameFreeWithin(name string, budget time.Duration) {
	if name == "" {
		return
	}
	deadline := time.Now().Add(budget)
	// Each force-remove gets a generous slice of the overall budget rather than
	// the tight teardown-drain forceRemoveBudget. Under heavy docker-daemon
	// contention a single `docker rm -f` of a still-running prior container can
	// run well past 2s; SIGKILLing it there (CommandContext) tears the docker
	// CLI down mid-request, so the removal may not complete, the name never
	// frees, and the whole budget is burned on repeated stillborn 2s attempts
	// (the observed "container name still in use after the pre-run remove
	// budget" → next `docker run --name` Conflict → got=0). Size the per-attempt
	// deadline to ~1/3 of the budget (floored at forceRemoveBudget) so the rm
	// can actually finish while still leaving room for a couple of retries
	// against the async --rm reaper.
	perAttempt := budget / 3
	if perAttempt < forceRemoveBudget {
		perAttempt = forceRemoveBudget
	}
	for {
		a.forceRemoveContainerByNameWithin(name, perAttempt)
		if a.containerNameFree(name) {
			return
		}
		if time.Now().After(deadline) {
			a.logger.Warn("container name still in use after the pre-run remove budget; the next docker run may conflict",
				zap.String("container", name), zap.Duration("budget", budget))
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// containerNameFree reports whether no container (in any state, including one
// the `--rm` reaper is mid-way through removing) currently holds the given
// name. Docker stores container names with a leading slash, so the filter is
// anchored to an exact `^/<name>$` match to avoid matching name substrings. A
// query error is treated as "not free" so the caller keeps waiting rather than
// racing into a `docker run --name` conflict.
func (a *App) containerNameFree(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "name=^/"+name+"$").CombinedOutput()
	if err != nil {
		a.logger.Debug("could not query container-name availability; treating as still-in-use",
			zap.String("container", name), zap.Error(err), zap.String("output", string(out)))
		return false
	}
	return len(bytes.TrimSpace(out)) == 0
}

// isExit125 reports whether a finished user-app command failed at runtime with
// docker's exit code 125 — the code docker returns when the daemon refuses to
// create/start the container (a name conflict, but also a bad image reference,
// an unsatisfiable mount, a malformed flag, …). It is a NECESSARY-but-not-
// sufficient signal for the name-conflict retry; isDockerRunNameConflict layers
// the name-occupied check on top to separate the conflict from those other 125s.
func isExit125(runErr error, errType utils.ErrType) bool {
	if runErr == nil || errType != utils.Runtime {
		return false
	}
	return strings.Contains(runErr.Error(), "exit status 125")
}

// isDockerRunNameConflict reports whether a failed user-app `docker run` failed
// SPECIFICALLY because its --name was still taken by a leftover container — the
// only exit-125 cause that force-removing the name + retrying can clear. It
// guards against blanket-retrying every exit 125: a genuinely broken run (bad
// image, missing mount, bad flag) also exits 125 but must fail fast, not be
// re-issued repeatedly.
//
// docker's name-conflict message ("Conflict. The container name ... is already
// in use by container ...") is written to the run's stderr, which keploy streams
// straight to the terminal rather than capturing, so the returned CmdError only
// carries the exit code ("exit status 125"). The conflict is instead confirmed
// positively from docker's own state: nameOccupied is true exactly when a
// container still holds the name at the moment the run failed — which is the
// precondition docker reports as the name conflict. Requiring BOTH the 125 exit
// and an occupied name keeps the retry strictly scoped to the recreate race.
func isDockerRunNameConflict(runErr error, errType utils.ErrType, nameOccupied bool) bool {
	return isExit125(runErr, errType) && nameOccupied
}

// extractProjectFlags returns any project-scoping flags (-p/--project-name,
// --project-directory) found in the given docker compose command.
func extractProjectFlags(cmd string) []string {
	parts := strings.Fields(cmd)
	var flags []string
	for i := 0; i < len(parts); i++ {
		switch {
		case (parts[i] == "-p" || parts[i] == "--project-name" || parts[i] == "--project-directory") && i+1 < len(parts):
			flags = append(flags, parts[i], parts[i+1])
			i++
		case strings.HasPrefix(parts[i], "-p=") || strings.HasPrefix(parts[i], "--project-name=") || strings.HasPrefix(parts[i], "--project-directory="):
			flags = append(flags, parts[i])
		}
	}
	return flags
}

func (a *App) run(ctx context.Context) models.AppError {
	userCmd := a.cmd

	// ComposeDown can fire on two paths within a single run() — cmdCancel below
	// (SIGINT/teardown) and this deferred call (normal exit / the
	// --abort-on-container-exit path). Collapse them with a run-local Once so a
	// bounded ComposeDown never runs twice and risks overrunning the drain
	// budget; whichever path fires first does the teardown, the other is a
	// no-op. External callers (the replay per-test-set loop) invoke
	// a.ComposeDown directly and are unaffected by this run-local guard.
	var downOnce sync.Once
	composeDown := func() { downOnce.Do(a.ComposeDown) }
	if a.kind == utils.DockerCompose {
		defer composeDown()
	}

	// dockerRunName is the resolved user-app container name; reused by the
	// post-run container-name-conflict retry below.
	var dockerRunName string
	if utils.FindDockerCmd(a.cmd) == utils.DockerRun {
		userCmd = utils.EnsureRmBeforeName(userCmd)
		// Clear any container left over from a prior run with the same --name
		// before starting. --rm removes the container on exit, but under heavy
		// docker-daemon contention that reap can lag past the next
		// `docker run --name`, so a re-run (e.g. a dedup lane re-running the app
		// per test-set) hits "Conflict. The container name ... is already in use"
		// (docker exit 125). Resolve the name from --container-name (a.container)
		// or, when that isn't set, the --name in the command itself, then
		// force-remove it with preRunRemoveBudget — generous because this is a
		// startup cleanup, and a still-running prior container can take more than
		// the 2s teardown cap to remove under contention. Best-effort: a no-op
		// when nothing is lingering.
		dockerRunName = a.container
		if dockerRunName == "" {
			dockerRunName = utils.ContainerNameFromDockerRun(userCmd)
		}
		a.ensureContainerNameFreeWithin(dockerRunName, preRunRemoveBudget)
	}

	// Compose mode: before `docker compose up`, make sure the keploy agent
	// container from a prior compose up/phase is fully gone. keploy injects the
	// agent as a compose service AND force-removes it by name on teardown, so a
	// fresh up that starts while the previous compose is still "Stopping
	// Gracefully" (the keep-app-alive recovery, and the atg sandbox's per-phase
	// re-record) reads the stale project state and tries to Recreate the
	// being-removed agent container — failing with "Error response from daemon:
	// No such container: <stale-id>" and aborting the whole run (the
	// atg-with-mocks flake). Polling the agent name free first (force-remove +
	// wait) serializes teardown -> up so the up always creates a fresh agent.
	// Mirrors the docker-run pre-run guard above; a no-op on the first up.
	if a.kind == utils.DockerCompose && a.keployContainer != "" {
		a.ensureContainerNameFreeWithin(a.keployContainer, preRunRemoveBudget)
	}

	// Define the function to cancel the command
	cmdCancel := func(cmd *exec.Cmd) func() error {
		return func() error {
			if utils.IsDockerCmd(a.kind) {
				a.logger.Debug("sending SIGINT to the container", zap.Any("cmd.Process.Pid", cmd.Process.Pid))
				err := utils.SendSignal(a.logger, -cmd.Process.Pid, syscall.SIGINT)
				if err != nil {
					warning := fmt.Sprintf("error sending SIGINT: %s", err)
					a.logger.Debug(warning)
				}

				// Docker Compose teardown differs from a single `docker run`: the
				// stack has several containers (the app, its dependencies, and the
				// injected keploy-agent). The SIGINT above makes `docker compose up`
				// stop them gracefully in the foreground, but the app container
				// usually exits first — e.g. a Go server with no SIGTERM handler
				// exits immediately on Go's default signal disposition — while the
				// SLOWER services keep stopping, most notably the eBPF agent.
				// `docker compose up` does not return until every service has
				// stopped, and os/exec's WaitDelay then blocks the teardown for ~25s
				// waiting on it. When the app is kept alive on the outer replay
				// errgroup (compose + mocking), that teardown runs under
				// utils.DrainErrGroup's 30s budget, so the overrun trips a spurious
				// "teardown drain timed out … a goroutine is ignoring context
				// cancellation" error and fails an otherwise-green run.
				//
				// So for compose: give the app a short grace to flush coverage during
				// the graceful stop, then bring the WHOLE project down fast via
				// ComposeDown (`docker compose down --timeout 1`) so `docker compose
				// up` returns promptly instead of blocking on the slow agent. The
				// run()-deferred ComposeDown then becomes an idempotent no-op.
				if a.kind == utils.DockerCompose {
					// Wall-clock-bounded grace (not iteration×inspect): a saturated
					// daemon must not let the per-tick inspect sum past the teardown
					// budget. Break as soon as the compose process or the app
					// container has exited (coverage flush done).
					graceDeadline := time.Now().Add(graceBudget)
					for time.Now().Before(graceDeadline) {
						time.Sleep(500 * time.Millisecond)
						// `docker compose up` already returned (all services stopped).
						if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
							a.logger.Debug("docker compose process exited")
							break
						}
						// App container has exited -> its coverage flush is done; no
						// need to keep waiting on the slower sibling services. Bound the
						// inspect so a saturated daemon can't stall the grace loop.
						ictx, icancel := context.WithTimeout(context.Background(), dockerInspectBudget)
						info, ierr := a.docker.ContainerInspect(ictx, a.container)
						icancel()
						if ierr == nil && (info.State.Status == "exited" || info.State.Status == "dead") {
							a.logger.Debug("app container stopped gracefully; tearing down the rest of the compose stack")
							break
						}
					}
					composeDown()
					return utils.SendSignal(a.logger, -cmd.Process.Pid, syscall.SIGKILL)
				}

				graceDeadline := time.Now().Add(graceBudget)
				for time.Now().Before(graceDeadline) {
					time.Sleep(1 * time.Second)

					// Check if the 'docker run' command has exited
					if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
						a.logger.Debug("docker client process exited")
						return nil
					}

					// Check the actual container status via Docker API
					// This handles cases where 'docker run' is dead but container is alive.
					// Bound the inspect so a saturated daemon can't stall the grace loop.
					ictx, icancel := context.WithTimeout(context.Background(), dockerInspectBudget)
					info, err := a.docker.ContainerInspect(ictx, a.container)
					icancel()
					if err == nil && (info.State.Status == "exited" || info.State.Status == "dead") {
						a.logger.Debug("container stopped gracefully")
						return nil
					}
				}

				// Force Kill using Docker API
				// We tell the Docker Daemon explicitly to kill this container.
				a.logger.Debug("container did not stop gracefully, killing it forcefully", zap.String("containerID", a.container))

				// "SIGKILL" string is standard for Docker API to force kill.
				// Bounded so a saturated daemon can't wedge the teardown past the
				// record/replay drain budget.
				killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
				err = a.docker.ContainerKill(killCtx, a.container, "SIGKILL")
				killCancel()
				if err != nil {
					warning := fmt.Sprintf("error killing container: %s", err)
					a.logger.Debug(warning)
				}
				// Clean up the CLI process as well
				err = utils.SendSignal(a.logger, -cmd.Process.Pid, syscall.SIGKILL)
				return err
			}
			return utils.InterruptProcessTree(a.logger, cmd.Process.Pid, syscall.SIGINT)
		}
	}

	var err error
	cmdErr := utils.ExecuteCommand(ctx, a.logger, userCmd, cmdCancel, 25*time.Second, a.composeContent)
	// A user-app `docker run --name X` can still lose the container-name race to
	// the prior test-set's --rm reaper on a saturated CI daemon even after the
	// pre-run ensureContainerNameFreeWithin verified the name was free — the
	// reaper can re-touch the name in the narrow window before the run, and
	// docker then exits 125 without starting the app ("Conflict. The container
	// name ... is already in use"). Force-remove the name and retry rather than
	// failing the whole test-set on a transient race.
	//
	// The retry is gated STRICTLY on the name-conflict signature: an exit-125
	// run whose --name is still held by a leftover container (isDockerRunNameConflict).
	// Every other exit-125 cause (bad image, missing mount, malformed flag) leaves
	// the name free, so it is NOT retried and fails fast with its real error —
	// re-issuing it would only burn the remove budget and bury the root cause.
	for attempt := 1; dockerRunName != "" && attempt <= dockerRunNameConflictRetries &&
		ctx.Err() == nil && isExit125(cmdErr.Err, cmdErr.Type) &&
		isDockerRunNameConflict(cmdErr.Err, cmdErr.Type, !a.containerNameFree(dockerRunName)); attempt++ {
		a.logger.Warn("docker run exited 125 with the --name still in use (container-name conflict); force-removing the name and retrying",
			zap.String("container", dockerRunName), zap.Int("attempt", attempt))
		a.ensureContainerNameFreeWithin(dockerRunName, preRunRemoveBudget)
		cmdErr = utils.ExecuteCommand(ctx, a.logger, userCmd, cmdCancel, 25*time.Second, a.composeContent)
	}
	if cmdErr.Err != nil {
		switch cmdErr.Type {
		case utils.Init:
			return models.AppError{AppErrorType: models.ErrCommandError, Err: cmdErr.Err, AppLogs: a.recentAppLogs(ctx)}
		case utils.Runtime:
			err = cmdErr.Err
		}
	}

	// Skip on the compose path: cmdCancel's ComposeDown already brought the
	// whole stack down and waitContainersRemoved confirmed the app/agent
	// containers are gone, so waiting again here would only add an inspect loop
	// to the teardown and risk overrunning the drain budget.
	if utils.IsDockerCmd(a.kind) && a.kind != utils.DockerCompose {
		a.waitTillExit()
	}

	select {
	case <-ctx.Done():
		a.logger.Debug("context cancelled, error while waiting for the app to exit", zap.Error(ctx.Err()))
		return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: ctx.Err()}
	default:
		appLogs := a.recentAppLogs(ctx)

		if a.Mode == models.MODE_RECORD && a.EnableTesting {
			a.logger.Info("waiting for some time before returning the error to allow recording of test cases when testing keploy with itself")
			time.Sleep(3 * time.Second)
			a.logger.Debug("test binary stopped", zap.Error(err))
			return models.AppError{AppErrorType: models.ErrTestBinStopped, Err: context.Canceled, AppLogs: appLogs}
		}

		if err != nil {
			return models.AppError{AppErrorType: models.ErrUnExpected, Err: err, AppLogs: appLogs}
		}
		return models.AppError{AppErrorType: models.ErrAppStopped, Err: nil, AppLogs: appLogs}
	}
}
