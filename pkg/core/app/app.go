// Package app provides functionality for managing applications.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/pkg/models"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"go.keploy.io/server/v2/pkg/core/app/docker"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func NewApp(logger *zap.Logger, id uint64, cmd string, opts Options) *App {
	app := &App{
		logger:           logger,
		id:               id,
		cmd:              cmd,
		kind:             utils.FindDockerCmd(cmd),
		keployContainer:  "keploy-v2",
		container:        opts.Container,
		containerDelay:   opts.DockerDelay,
		containerNetwork: opts.DockerNetwork,
	}
	return app
}

type App struct {
	logger           *zap.Logger
	docker           docker.Client
	id               uint64
	cmd              string
	kind             utils.CmdType
	containerDelay   uint64
	container        string
	containerNetwork string
	containerIPv4    string
	keployNetwork    string
	keployContainer  string
	keployIPv4       string
	inodeChan        chan uint64
	EnableTesting    bool
	Mode             models.Mode
}

type Options struct {
	// canExit disables any error returned if the app exits by itself.
	//CanExit       bool
	Container     string
	DockerDelay   uint64
	DockerNetwork string
}

func (a *App) Setup(_ context.Context) error {
	d, err := docker.New(a.logger)
	if err != nil {
		return err
	}
	a.docker = d

	if utils.IsDockerKind(a.kind) && isDetachMode(a.logger, a.cmd, a.kind) {
		return fmt.Errorf("application could not be started in detached mode")
	}

	switch a.kind {
	case utils.DockerRun, utils.DockerStart:
		err := a.SetupDocker()
		if err != nil {
			return err
		}
	case utils.DockerCompose:
		err = a.SetupCompose()
		if err != nil {
			return err
		}
	default:
		// setup native binary
	}
	return nil
}

func (a *App) KeployIPv4Addr() string {
	return a.keployIPv4
}

func (a *App) ContainerIPv4Addr() string {
	return a.containerIPv4
}

func (a *App) SetupDocker() error {
	var err error

	cont, net, err := ParseDockerCmd(a.cmd, a.kind, a.docker)

	if err != nil {
		utils.LogError(a.logger, err, "failed to parse container name from given docker command", zap.String("cmd", a.cmd))
		return err
	}
	if a.container == "" {
		a.container = cont
	} else if a.container != cont {
		a.logger.Warn(fmt.Sprintf("given app container:(%v) is different from parsed app container:(%v)", a.container, cont))
	}

	if a.containerNetwork == "" {
		a.containerNetwork = net
	} else if a.containerNetwork != net {
		a.logger.Warn(fmt.Sprintf("given docker network:(%v) is different from parsed docker network:(%v)", a.containerNetwork, net))
	}

	if a.kind == utils.DockerStart {
		running, err := a.docker.IsContainerRunning(cont)
		if err != nil {
			return err
		}
		if running {
			return fmt.Errorf("docker container is already in running state")
		}
	}

	//injecting appNetwork to keploy.
	err = a.injectNetwork(a.containerNetwork)
	if err != nil {
		utils.LogError(a.logger, err, fmt.Sprintf("failed to inject network:%v to the keploy container", a.containerNetwork))
		return err
	}
	return nil
}

func (a *App) SetupCompose() error {
	if a.container == "" {
		utils.LogError(a.logger, nil, "container name not found", zap.String("AppCmd", a.cmd))
		return errors.New("container name not found")
	}
	a.logger.Info("keploy requires docker compose containers to be run with external network")
	//finding the user docker-compose file in the current directory.
	// TODO currently we just return the first default docker-compose file found in the current directory
	// we should add support for multiple docker-compose files by either parsing cmd for path
	// or by asking the user to provide the path
	path := findComposeFile()
	if path == "" {
		return errors.New("can't find the docker compose file of user. Are you in the right directory? ")
	}
	// kdocker-compose.yaml file will be run instead of the user docker-compose.yaml file acc to below cases
	newPath := "docker-compose-tmp.yaml"

	compose, err := a.docker.ReadComposeFile(path)
	if err != nil {
		utils.LogError(a.logger, err, "failed to read the compose file")
		return err
	}
	composeChanged := false

	// Check if docker compose file uses relative file names for bind mounts
	ok := a.docker.HasRelativePath(compose)
	if ok {
		err = a.docker.ForceAbsolutePath(compose, path)
		if err != nil {
			utils.LogError(a.logger, nil, "failed to convert relative paths to absolute paths in volume mounts in docker compose file")
			return err
		}
		composeChanged = true
	}

	// Checking info about the network and whether its external:true
	info := a.docker.GetNetworkInfo(compose)

	if info == nil {
		info, err = a.docker.SetKeployNetwork(compose)
		if err != nil {
			utils.LogError(a.logger, nil, "failed to set default network in the compose file", zap.String("network", a.keployNetwork))
			return err
		}
		composeChanged = true
	}

	if !info.External {
		err = a.docker.MakeNetworkExternal(compose)
		if err != nil {
			utils.LogError(a.logger, nil, "failed to make the network external in the compose file", zap.String("network", info.Name))
			return fmt.Errorf("error while updating network to external: %v", err)
		}
		composeChanged = true
	}

	a.keployNetwork = info.Name

	ok, err = a.docker.NetworkExists(a.keployNetwork)
	if err != nil {
		utils.LogError(a.logger, nil, "failed to find default network", zap.String("network", a.keployNetwork))
		return err
	}

	//if keploy-network doesn't exist locally then create it
	if !ok {
		err = a.docker.CreateNetwork(a.keployNetwork)
		if err != nil {
			utils.LogError(a.logger, nil, "failed to create default network", zap.String("network", a.keployNetwork))
			return err
		}
	}

	if composeChanged {
		err = a.docker.WriteComposeFile(compose, newPath)
		if err != nil {
			utils.LogError(a.logger, nil, "failed to write the compose file", zap.String("path", newPath))
		}
		a.logger.Info("Created new docker-compose for keploy internal use", zap.String("path", newPath))
		//Now replace the running command to run the kdocker-compose.yaml file instead of user docker compose file.
		a.cmd = modifyDockerComposeCommand(a.cmd, newPath)
	}

	if a.containerNetwork == "" {
		a.containerNetwork = a.keployNetwork
	}
	err = a.injectNetwork(a.containerNetwork)
	if err != nil {
		utils.LogError(a.logger, err, fmt.Sprintf("failed to inject network:%v to the keploy container", a.containerNetwork))
		return err
	}
	return nil
}

func (a *App) Kind(_ context.Context) utils.CmdType {
	return a.kind
}

// injectNetwork attaches the given network to the keploy container
// and also sends the keploy container ip of the new network interface to the kernel space
func (a *App) injectNetwork(network string) error {
	// inject the network to the keploy container
	a.logger.Info(fmt.Sprintf("trying to inject network:%v to the keploy container", network))
	err := a.docker.AttachNetwork(a.keployContainer, []string{network})
	if err != nil {
		utils.LogError(a.logger, nil, "failed to inject network to the keploy container")
		return err
	}

	a.keployNetwork = network

	//sending new proxy ip to kernel, since dynamically injected new network has different ip for keploy.
	inspect, err := a.docker.ContainerInspect(context.Background(), a.keployContainer)
	if err != nil {
		utils.LogError(a.logger, nil, fmt.Sprintf("failed to get inspect keploy container:%v", inspect))
		return err
	}

	keployNetworks := inspect.NetworkSettings.Networks
	//Here we considering that the application would use only one custom network.
	//TODO: handle for application having multiple custom networks
	//TODO: check the logic for correctness
	for n, settings := range keployNetworks {
		if n == network {
			a.keployIPv4 = settings.IPAddress
			a.logger.Info("Successfully injected network to the keploy container", zap.Any("Keploy container", a.keployContainer), zap.Any("appNetwork", network))
			return nil
		}
		//if networkName != "bridge" {
		//	network = networkName
		//	newProxyIpString = networkSettings.IPAddress
		//	a.logger.Debug(fmt.Sprintf("Network Name: %s, New Proxy IP: %s\n", networkName, networkSettings.IPAddress))
		//}
	}
	return fmt.Errorf("failed to find the network:%v in the keploy container", network)
}

func (a *App) extractMeta(ctx context.Context, e events.Message) (bool, error) {
	if e.Action != "start" {
		return false, nil
	}
	// Fetch container details by inspecting using container ID to check if container is created
	info, err := a.docker.ContainerInspect(ctx, e.ID)
	if err != nil {
		a.logger.Debug("failed to inspect container by container Id", zap.Error(err))
		return false, err
	}

	// Check if the container's name matches the desired name
	if info.Name != "/"+a.container {
		a.logger.Debug("ignoring container creation for unrelated container", zap.String("containerName", info.Name))
		return false, nil
	}

	// Set Docker Container ID
	a.docker.SetContainerID(e.ID)
	a.logger.Debug("checking for container pid", zap.Any("containerDetails.State.Pid", info.State.Pid))
	if info.State.Pid == 0 {
		return false, errors.New("failed to get the pid of the container")
	}
	a.logger.Debug("", zap.Any("containerDetails.State.Pid", info.State.Pid), zap.String("containerName", a.container))
	inode, err := getInode(info.State.Pid)
	if err != nil {
		return false, err
	}

	a.inodeChan <- inode
	a.logger.Debug("container started and successfully extracted inode", zap.Any("inode", inode))
	if info.NetworkSettings == nil || info.NetworkSettings.Networks == nil {
		a.logger.Debug("container network settings not available", zap.Any("containerDetails.NetworkSettings", info.NetworkSettings))
		return false, nil
	}

	n, ok := info.NetworkSettings.Networks[a.containerNetwork]
	if !ok || n == nil {
		a.logger.Debug("container network not found", zap.Any("containerDetails.NetworkSettings.Networks", info.NetworkSettings.Networks))
		return false, fmt.Errorf("container network not found: %s", fmt.Sprintf("%+v", info.NetworkSettings.Networks))
	}
	a.containerIPv4 = n.IPAddress
	return inode != 0 && n.IPAddress != "", nil
}

func (a *App) getDockerMeta(ctx context.Context) <-chan error {
	// listen for the docker daemon events
	defer a.logger.Debug("exiting from goroutine of docker daemon event listener")

	errCh := make(chan error, 1)
	timer := time.NewTimer(time.Duration(a.containerDelay) * time.Second)
	logTicker := time.NewTicker(1 * time.Second)
	defer logTicker.Stop()

	eventFilter := filters.NewArgs(
		filters.KeyValuePair{Key: "type", Value: "container"},
		filters.KeyValuePair{Key: "type", Value: "network"},
		filters.KeyValuePair{Key: "action", Value: "create"},
		filters.KeyValuePair{Key: "action", Value: "connect"},
		filters.KeyValuePair{Key: "action", Value: "start"},
	)

	messages, errCh2 := a.docker.Events(ctx, types.EventsOptions{
		Filters: eventFilter,
	})

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		errCh <- errors.New("failed to get the error group from the context")
		return errCh
	}
	g.Go(func() error {
		defer utils.Recover(a.logger)
		defer close(errCh)
		for {
			select {
			case <-timer.C:
				errCh <- errors.New("timeout waiting for the container to start")
				return nil
			case <-ctx.Done():
				a.logger.Debug("context cancelled, stopping the listener for container creation event.")
				errCh <- ctx.Err()
				return nil
			case e := <-messages:
				done, err := a.extractMeta(ctx, e)
				if err != nil {
					errCh <- err
					return nil
				}

				if done {
					return nil
				}
			// for debugging purposes
			case <-logTicker.C:
				a.logger.Debug("still waiting for the container to start.", zap.String("containerName", a.container))
				return nil
			case err := <-errCh2:
				errCh <- err
				return nil
			}
		}
	})
	return errCh
}

func (a *App) runDocker(ctx context.Context) models.AppError {
	// if a.cmd is empty, it means the user wants to run the application manually,
	// so we don't need to run the application in a goroutine
	if a.cmd == "" {
		return models.AppError{}
	}

	g, ctx := errgroup.WithContext(ctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)

	defer func() {
		err := g.Wait()
		if err != nil {
			utils.LogError(a.logger, err, "failed to run dockerized app")
		}
	}()

	errCh := make(chan error, 1)
	// listen for the "create container" event in order to send the inode of the container to the kernel
	errCh2 := a.getDockerMeta(ctx)

	g.Go(func() error {
		defer utils.Recover(a.logger)
		defer close(errCh)
		err := a.run(ctx)
		if err.Err != nil {
			utils.LogError(a.logger, err.Err, "Application stopped with the error")
			errCh <- err.Err
		}
		return nil
	})

	select {
	case err := <-errCh:
		if err != nil && errors.Is(err, context.Canceled) {
			return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: ctx.Err()}
		}
		return models.AppError{AppErrorType: models.ErrInternal, Err: err}
	case err := <-errCh2:
		if err != nil && errors.Is(err, context.Canceled) {
			return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: ctx.Err()}
		}
		return models.AppError{AppErrorType: models.ErrInternal, Err: err}
	case <-ctx.Done():
		return models.AppError{AppErrorType: models.ErrCtxCanceled, Err: ctx.Err()}
	}
}

func (a *App) Run(ctx context.Context, inodeChan chan uint64) models.AppError {
	a.inodeChan = inodeChan

	if utils.IsDockerKind(a.kind) {
		return a.runDocker(ctx)
	}
	return a.run(ctx)
}

func (a *App) run(ctx context.Context) models.AppError {
	// Run the app as the user who invoked sudo
	userCmd := a.cmd
	username := os.Getenv("SUDO_USER")

	if utils.FindDockerCmd(a.cmd) == utils.DockerRun {
		userCmd = utils.EnsureRmBeforeName(userCmd)
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", userCmd)
	if username != "" {
		// print all environment variables
		a.logger.Debug("env inherited from the cmd", zap.Any("env", os.Environ()))
		// Run the command as the user who invoked sudo to preserve the user environment variables and PATH
		cmd = exec.CommandContext(ctx, "sudo", "-E", "-u", os.Getenv("SUDO_USER"), "env", "PATH="+os.Getenv("PATH"), "sh", "-c", userCmd)
	}

	// Set the cancel function for the command
	cmd.Cancel = func() error {

		return utils.InterruptProcessTree(a.logger, cmd.Process.Pid, syscall.SIGINT)
	}
	// wait after sending the interrupt signal, before sending the kill signal
	cmd.WaitDelay = 10 * time.Second

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Set the output of the command
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	a.logger.Debug("", zap.Any("executing cli", cmd.String()))

	err := cmd.Start()
	if err != nil {
		return models.AppError{AppErrorType: models.ErrCommandError, Err: err}
	}

	err = cmd.Wait()
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

//if a.docker.GetContainerID() == "" {
//	a.logger.Debug("still waiting for the container to start.", zap.String("containerName", a.container))
//	continue
//}
////Inspecting the application container again since the ip and pid takes some time to be linked to the container.
//info, err := a.docker.ContainerInspect(ctx, a.container)
//if err != nil {
//	return err
//}
//
//a.logger.Debug("checking for container pid", zap.Any("containerDetails.State.Pid", info.State.Pid))
//if info.State.Pid == 0 {
//	a.logger.Debug("container not yet started", zap.Any("containerDetails.State.Pid", info.State.Pid))
//	continue
//}
//a.logger.Debug("", zap.Any("containerDetails.State.Pid", info.State.Pid), zap.String("containerName", a.container))
//a.inode,err = getInode(info.State.Pid)
//if err != nil {
//	return err
//}
//if info.NetworkSettings == nil || info.NetworkSettings.Networks == nil {
//	a.logger.Debug("container network settings not available", zap.Any("containerDetails.NetworkSettings", info.NetworkSettings))
//	continue
//}
//
//n, ok := info.NetworkSettings.Networks[a.containerNetwork]
//if !ok || n == nil {
//	return errors.New("container network not found")
//}
//a.keployIPv4 = n.IPAddress
//a.logger.Info("container started successfully", zap.Any("", info.NetworkSettings.Networks))
//return

//case e := <-messages:
//	if e.Type != events.ContainerEventType || e.Action != "start" {
//		continue
//	}
//
//	// Fetch container details by inspecting using container ID to check if container is created
//	c, err := a.docker.ContainerInspect(ctx, e.ID)
//	if err != nil {
//		a.logger.Debug("failed to inspect container by container Id", zap.Error(err))
//		return err
//	}
//
//	// Check if the container's name matches the desired name
//	if c.Name != "/"+a.container {
//		a.logger.Debug("ignoring container creation for unrelated container", zap.String("containerName", c.Name))
//		continue
//	}
//	// Set Docker Container ID
//	a.docker.SetContainerID(e.ID)
//
//	a.logger.Debug("container created for desired app", zap.Any("ID", e.ID))
