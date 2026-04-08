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
	// For Java, check if JAVA_TOOL_OPTIONS is already set in the docker run
	// command. If so, append the truststore flags to the existing value.
	// If not, add it as a new -e flag.
	javaOpts := fmt.Sprintf("-Djavax.net.ssl.trustStore=%s -Djavax.net.ssl.trustStorePassword=changeit", trustStorePath)
	if !strings.Contains(a.cmd, "-Djavax.net.ssl.trustStore=") {
		if strings.Contains(a.cmd, "JAVA_TOOL_OPTIONS=") {
			// Append truststore flags to the existing JAVA_TOOL_OPTIONS value
			a.cmd = strings.Replace(a.cmd, "JAVA_TOOL_OPTIONS=", fmt.Sprintf("JAVA_TOOL_OPTIONS=%s ", javaOpts), 1)
		} else {
			tlsFlags += fmt.Sprintf("-e JAVA_TOOL_OPTIONS='%s' ", javaOpts)
		}
	}

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
	timeout := time.NewTimer(30 * time.Second)
	logTicker := time.NewTicker(1 * time.Second)
	defer logTicker.Stop()
	defer timeout.Stop()

	containerID := a.container
	for {
		select {
		case <-logTicker.C:
			// Inspect the container status
			containerJSON, err := a.docker.ContainerInspect(context.Background(), containerID)
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
			a.logger.Warn("timeout waiting for the container to stop", zap.String("containerID", containerID))
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

// composeDown runs docker compose down to remove all containers and networks
// created by the compose stack. Without this, stopped containers retain
// references to image layers; a subsequent docker image prune can delete
// those layers and corrupt Docker Desktop's overlayfs snapshots.
func (a *App) composeDown() {
	if a.composeFile == "" {
		return
	}
	a.logger.Debug("Running docker compose down to clean up containers and networks",
		zap.String("composeFile", a.composeFile))
	downCmd := exec.Command("docker", "compose", "-f", a.composeFile, "down")
	if output, err := downCmd.CombinedOutput(); err != nil {
		a.logger.Debug("docker compose down finished with error (may be expected if containers already removed)",
			zap.Error(err), zap.String("output", string(output)))
	}
}

func (a *App) run(ctx context.Context) models.AppError {
	userCmd := a.cmd

	if a.kind == utils.DockerCompose {
		defer a.composeDown()
	}

	if utils.FindDockerCmd(a.cmd) == utils.DockerRun {
		userCmd = utils.EnsureRmBeforeName(userCmd)
	}

	// Define the function to cancel the command
	cmdCancel := func(cmd *exec.Cmd) func() error {
		return func() error {
			if utils.IsDockerCmd(a.kind) {
				a.logger.Debug("sending SIGINT to the container", zap.Any("cmd.Process.Pid", cmd.Process.Pid))
				err := utils.SendSignal(a.logger, -cmd.Process.Pid, syscall.SIGINT)
				if err != nil {
					warning := fmt.Sprintf("error sending SIGINT: %s", err)
					a.logger.Warn(warning)
				}
				gracePeriod := 5
				for i := 0; i < gracePeriod; i++ {
					time.Sleep(1 * time.Second)

					// Check if the 'docker run' command has exited
					if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
						a.logger.Debug("docker client process exited")
						return nil
					}

					// Check the actual container status via Docker API
					// This handles cases where 'docker run' is dead but container is alive
					info, err := a.docker.ContainerInspect(context.Background(), a.container)
					if err == nil && (info.State.Status == "exited" || info.State.Status == "dead") {
						a.logger.Debug("container stopped gracefully")
						return nil
					}
				}

				// Force Kill using Docker API
				// We tell the Docker Daemon explicitly to kill this container.
				a.logger.Warn("container did not stop gracefully, killing it forcefully", zap.String("containerID", a.container))

				// "SIGKILL" string is standard for Docker API to force kill
				err = a.docker.ContainerKill(context.Background(), a.container, "SIGKILL")
				if err != nil {
					warning := fmt.Sprintf("error killing container: %s", err)
					a.logger.Warn(warning)
				}
				// Clean up the CLI process as well
				err = utils.SendSignal(a.logger, -cmd.Process.Pid, syscall.SIGKILL)
				return err
			}
			return utils.InterruptProcessTree(a.logger, cmd.Process.Pid, syscall.SIGINT)
		}
	}

	var err error
	cmdErr := utils.ExecuteCommand(ctx, a.logger, userCmd, cmdCancel, 25*time.Second)
	if cmdErr.Err != nil {
		switch cmdErr.Type {
		case utils.Init:
			return models.AppError{AppErrorType: models.ErrCommandError, Err: cmdErr.Err, AppLogs: a.recentAppLogs(ctx)}
		case utils.Runtime:
			err = cmdErr.Err
		}
	}

	if utils.IsDockerCmd(a.kind) {
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
