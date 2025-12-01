// Package app provides functionality for managing applications.
package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"go.keploy.io/server/v3/pkg/models"

	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

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
	keployContainer string
	EnableTesting   bool
	Mode            models.Mode
}

func (a *App) Setup(_ context.Context) error {

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
		err := a.SetupCompose()
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
	err := a.attachInitPid(context.Background())
	if err != nil {
		utils.LogError(a.logger, err, "failed to attach init pid")
		return err
	}
	return nil
}

// AttachInitPid modifies the existing Docker command to attach the init container's PID namespace
func (a *App) attachInitPid(_ context.Context) error {
	if a.cmd == "" {
		return fmt.Errorf("no command provided to modify")
	}
	// Attach the keploy agent container's PID namespace and network namespace to the app container
	// Sharing network namespace so that the docker container IPs of both agent and app remain same
	// sharing pid namespace so that they share the same process tree (common ancestor) in docker
	pidMode := fmt.Sprintf("--pid=container:%s", a.keployContainer)
	networkMode := fmt.Sprintf("--network=container:%s", a.keployContainer)

	// Inject the pidMode flag after 'docker run' in the command
	parts := strings.SplitN(a.cmd, " ", 3) // Split by first two spaces to isolate "docker run"
	if len(parts) < 3 {
		return fmt.Errorf("invalid command structure: %s", a.cmd)
	}

	// Modify the command to insert the pidMode
	a.cmd = fmt.Sprintf("%s %s %s %s %s", parts[0], parts[1], pidMode, networkMode, parts[2])
	a.logger.Debug("added network namespace and pid to docker command", zap.String("cmd", a.cmd))
	return nil
}

func (a *App) SetupCompose() error {
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

	a.logger.Info(fmt.Sprintf("Found docker compose file paths: %v", paths))

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
	compose := serviceInfo.Compose

	if HookImpl != nil {
		_, err := HookImpl.BeforeDockerComposeSetup(context.Background(), compose, a.container)
		if err != nil {
			utils.LogError(a.logger, err, "hook failed during docker compose setup")
			return err
		}
		a.logger.Debug("Successfully ran BeforeDockerComposeSetup hook")
	}

	err = a.docker.ModifyComposeForAgent(compose, a.opts, a.container)
	if err != nil {
		utils.LogError(a.logger, err, "failed to modify compose for keploy integration")
		return err
	}

	err = a.docker.WriteComposeFile(compose, newPath)
	if err != nil {
		utils.LogError(a.logger, nil, "failed to write the compose file", zap.String("path", newPath))
	}
	a.logger.Info("Created new docker-compose for keploy internal use", zap.String("path", newPath))

	// Now replace the running command to run the docker-compose-tmp.yaml file instead of user docker compose file.
	a.cmd = modifyDockerComposeCommand(a.cmd, newPath, serviceInfo.ComposePath)

	a.logger.Info("Modified docker compose command to run keploy compose file", zap.String("cmd", a.cmd))

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

func (a *App) run(ctx context.Context) models.AppError {
	userCmd := a.cmd

	if utils.FindDockerCmd(a.cmd) == utils.DockerRun {
		userCmd = utils.EnsureRmBeforeName(userCmd)
	}

	// Define the function to cancel the command
	cmdCancel := func(cmd *exec.Cmd) func() error {
		return func() error {
			if utils.IsDockerCmd(a.kind) {
				a.logger.Debug("sending SIGINT to the container", zap.Any("cmd.Process.Pid", cmd.Process.Pid))
				err := utils.SendSignal(a.logger, -cmd.Process.Pid, syscall.SIGINT)

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
			return models.AppError{AppErrorType: models.ErrCommandError, Err: cmdErr.Err}
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
		if a.Mode == models.MODE_RECORD && a.EnableTesting {
			a.logger.Info("waiting for some time before returning the error to allow recording of test cases when testing keploy with itself")
			time.Sleep(3 * time.Second)
			a.logger.Debug("test binary stopped", zap.Error(err))
			return models.AppError{AppErrorType: models.ErrTestBinStopped, Err: context.Canceled}
		}

		if err != nil {
			return models.AppError{AppErrorType: models.ErrUnExpected, Err: err}
		}
		return models.AppError{AppErrorType: models.ErrAppStopped, Err: nil}
	}
}
