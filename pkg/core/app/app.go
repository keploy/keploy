package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/app/docker"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func NewApp(logger *zap.Logger, id int, cmd string) core.App {
	app := &App{
		logger: logger,
		id:     id,
		cmd:    cmd,
		kind:   utils.FindDockerCmd(cmd),
	}
	return app
}

type App struct {
	logger           *zap.Logger
	docker           docker.Client
	id               int
	cmd              string
	kind             utils.CmdType
	containerDelay   time.Duration
	container        string
	containerNetwork string
	containerIPv4    string
	keployNetwork    string
	keloyContainer   string
	keployIPv4       string
	inode            uint64
}

func (a *App) Setup(ctx context.Context, opts core.AppOptions) error {
	d, err := docker.New(a.logger)
	if err != nil {
		return err
	}
	a.docker = d
	switch a.kind {
	case utils.Docker:
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

func (a *App) SetupDocker() error {
	var err error
	cont, net, err := parseDockerCmd(a.cmd)
	if err != nil {
		a.logger.Error("failed to parse container name from given docker command", zap.Error(err), zap.Any("cmd", a.cmd))
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

	//injecting appNetwork to keploy.
	err = a.injectNetwork(a.containerNetwork)
	if err != nil {
		a.logger.Error(fmt.Sprintf("failed to inject network:%v to the keploy container", a.containerNetwork))
		return err
	}
	return nil
}

func (a *App) SetupCompose() error {
	if a.container == "" {
		a.logger.Error("please provide container name in case of docker-compose file", zap.Any("AppCmd", a.cmd))
		return errors.New("container name not found")
	}
	a.logger.Info("keploy requires docker compose containers to be run with external network")
	//finding the user docker-compose file in the current directory.
	// TODO currently we just return the first default docker-compose file found in the current directory
	// we should add support for multiple docker-compose files by either parsing cmd for path
	// or by asking the user to provide the path
	path := findComposeFile()
	if path == "" {
		return errors.New("can't find the docker compose file of user. Are you in the right directory?")
	}
	// kdocker-compose.yaml file will be run instead of the user docker-compose.yaml file acc to below cases
	newPath := "docker-compose-tmp.yaml"

	compose, err := a.docker.ReadComposeFile(path)
	composeChanged := false

	// Check if docker compose file uses relative file names for bind mounts
	ok := a.docker.HasRelativePath(compose)
	if ok {
		err = a.docker.ForceAbsolutePath(compose, path)
		if err != nil {
			a.logger.Error("failed to convert relative paths to absolute paths in volume mounts in docker compose file")
			return err
		}
		composeChanged = true
	}

	// Checking info about the network and whether its external:true
	info := a.docker.GetNetworkInfo(compose)

	if info == nil {
		err = a.docker.SetKeployNetwork(compose)
		if err != nil {
			a.logger.Error("failed to set default network in the compose file", zap.String("network", a.keployNetwork))
			return err
		}
		composeChanged = true
	}

	if !info.External {
		err = a.docker.MakeNetworkExternal(compose)
		if err != nil {
			a.logger.Error("failed to make the network external in the compose file", zap.String("network", info.Name))
			return fmt.Errorf("error while updating network to external: %v", err)
		}
		a.keployNetwork = info.Name
		composeChanged = true

	}

	ok, err = a.docker.NetworkExists(a.keployNetwork)
	if err != nil {
		a.logger.Error("failed to find default network", zap.String("network", a.keployNetwork))
		return err
	}

	//if keploy-network doesn't exist locally then create it
	if !ok {
		err = a.docker.CreateNetwork(a.keployNetwork)
		if err != nil {
			a.logger.Error("failed to create default network", zap.String("network", a.keployNetwork))
			return err
		}
	}

	if composeChanged {
		err = a.docker.WriteComposeFile(compose, newPath)
		if err != nil {
			a.logger.Error("failed to write the compose file", zap.String("path", newPath))
		}
		a.logger.Info("Created new docker-compose for keploy internal use", zap.String("path", newPath))
		//Now replace the running command to run the kdocker-compose.yaml file instead of user docker compose file.
		a.cmd = modifyDockerComposeCommand(a.cmd, newPath)
	}

	err = a.injectNetwork(a.containerNetwork)
	if err != nil {
		a.logger.Error(fmt.Sprintf("failed to inject network:%v to the keploy container", a.containerNetwork))
		return err
	}
	return nil
}

func (a *App) Kind(ctx context.Context) utils.CmdType {
	return a.kind
}

// injectNetwork attaches the given network to the keploy container
// and also sends the keploy container ip of the new network interface to the kernel space
func (a *App) injectNetwork(network string) error {
	// inject the network to the keploy container
	a.logger.Info(fmt.Sprintf("trying to inject network:%v to the keploy container", network))
	err := a.docker.AttachNetwork(a.keloyContainer, []string{network})
	if err != nil {
		a.logger.Error("could not inject application network to the keploy container")
		return err
	}

	a.keployNetwork = network

	//sending new proxy ip to kernel, since dynamically injected new network has different ip for keploy.
	kInspect, err := a.docker.ContainerInspect(context.Background(), a.keloyContainer)
	if err != nil {
		a.logger.Error(fmt.Sprintf("failed to get inspect keploy container:%v", kInspect))
		return err
	}

	keployNetworks := kInspect.NetworkSettings.Networks
	//Here we considering that the application would use only one custom network.
	//TODO: handle for application having multiple custom networks
	//TODO: check the logic for correctness
	for n, settings := range keployNetworks {
		if n == network {
			a.keployIPv4 = settings.IPAddress
			a.logger.Info("Successfully injected network to the keploy container", zap.Any("Keploy container", a.keloyContainer), zap.Any("appNetwork", network))
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

func (a *App) handleDockerEvents(ctx context.Context, e events.Message) error {
	switch e.Action {
	case "create":
		// Fetch container details by inspecting using container ID to check if container is created
		c, err := a.docker.ContainerInspect(ctx, e.ID)
		if err != nil {
			a.logger.Debug("failed to inspect container by container Id", zap.Error(err))
			return err
		}

		// Check if the container's name matches the desired name
		if c.Name != "/"+a.container {
			a.logger.Debug("ignoring container creation for unrelated container", zap.String("containerName", c.Name))
			return nil
		}
		// Set Docker Container ID
		a.docker.SetContainerID(e.ID)
	case "connect":
		// check if the container exists
		if a.docker.GetContainerID() == "" {
			a.logger.Debug("still waiting for the container to start.", zap.String("containerName", a.container))
			return nil
		}

		//Inspecting the application container again since the ip and pid takes some time to be linked to the container.
		info, err := a.docker.ContainerInspect(ctx, a.container)
		if err != nil {
			return err
		}

		if info.NetworkSettings == nil || info.NetworkSettings.Networks == nil {
			a.logger.Debug("container network settings not available", zap.Any("containerDetails.NetworkSettings", info.NetworkSettings))
			return nil
		}

		n, ok := info.NetworkSettings.Networks[a.containerNetwork]
		if !ok || n == nil {
			a.logger.Debug("container network not found", zap.Any("containerDetails.NetworkSettings.Networks", info.NetworkSettings.Networks))
			return nil
		}
		a.containerIPv4 = n.IPAddress
		a.logger.Info("successfully deleted container network and  ip settings", zap.Any("ip ", a.keployIPv4))
	case "start":
		if a.docker.GetContainerID() == "" {
			a.logger.Debug("still waiting for the container to start.", zap.String("containerName", a.container))
			return nil
		}
		if a.containerIPv4 == "" {
			return errors.New("docker container started but ip not yet available")
		}

		//Inspecting the application container again since the ip and pid takes some time to be linked to the container.
		info, err := a.docker.ContainerInspect(ctx, a.container)
		if err != nil {
			return err
		}

		a.logger.Debug("checking for container pid", zap.Any("containerDetails.State.Pid", info.State.Pid))
		if info.State.Pid == 0 {
			return errors.New("failed to get the pid of the container")
		}
		a.logger.Debug("", zap.Any("containerDetails.State.Pid", info.State.Pid), zap.String("containerName", a.container))
		a.inode, err = getInode(info.State.Pid)
		if err != nil {
			return err
		}
		a.logger.Debug("container started and successfully extracted inode", zap.Any("inode", a.inode))
	}
	return nil
}

func (a *App) getDockerMeta(ctx context.Context) <-chan error {
	// listen for the docker daemon events
	defer a.logger.Debug("exiting from goroutine of docker daemon event listener")

	errCh := make(chan error)
	timer := time.NewTimer(a.containerDelay)
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

	go func(errCh chan error) {
		for {
			select {
			case <-timer.C:
				errCh <- errors.New("timeout waiting for the container to start")
			case <-ctx.Done():
				a.logger.Debug("context cancelled, stopping the listener for container creation event.")
			case e := <-messages:
				err := a.handleDockerEvents(ctx, e)
				if err != nil {
					errCh <- err
				}
			// for debugging purposes
			case <-logTicker.C:
				a.logger.Debug("still waiting for the container to start.", zap.String("containerName", a.container))
			case err := <-errCh2:
				errCh <- err
			}

		}
	}(errCh)
	return errCh
}

func (a *App) runDocker(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error)
	// listen for the "create container" event in order to send the inode of the container to the kernel
	errCh2 := a.getDockerMeta(ctx)
	// if a.cmd is empty, it means the user wants to run the application manually,
	// so we don't need to run the application in a goroutine
	if a.cmd == "" {
		return nil
	}
	go func(ctx context.Context) {
		defer cancel()
		err := a.Run(ctx)
		if err != nil {
			a.logger.Error("Application stopped with the error", zap.Error(err))
			errCh <- err
		}
	}(ctx)
	select {
	case err := <-errCh:
		return err
	case err := <-errCh2:
		return err
	case <-ctx.Done():
		return nil
	}
}
func findComposeFile() string {
	filenames := []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}

	for _, filename := range filenames {
		if _, err := os.Stat(filename); !os.IsNotExist(err) {
			return filename
		}
	}

	return ""
}

func (a *App) Run(ctx context.Context) error {
	if a.kind == utils.DockerCompose || a.kind == utils.Docker {
		return a.runDocker(ctx)
	}
	return a.run(ctx)
}

func (a *App) run(ctx context.Context) error {
	// Create a new command with your appCmd
	// TODO: do we need sh here? or just use appCmd directly?
	cmd := exec.CommandContext(ctx, "sh", "-c", a.cmd)

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Set the output of the command
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run the app as the user who invoked sudo
	uname := os.Getenv("SUDO_USER")
	if uname != "" {
		// Switch to the user who invoked sudo
		u, err := user.Lookup(uname)
		if err != nil {
			a.logger.Error("failed to lookup user", zap.Error(err))
			return err
		}

		uid, err := strconv.ParseUint(u.Uid, 10, 32)
		gid, err := strconv.ParseUint(u.Gid, 10, 32)

		if err != nil {
			a.logger.Error("failed to parse user or group id", zap.Error(err))
			return err
		}
		// Switch the user
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}

	a.logger.Debug("", zap.Any("executing cli", cmd.String()))

	err := cmd.Start()
	if err != nil {
		a.logger.Error("failed to start the app", zap.Error(err))
		// TODO return named error
		return err
	}

	return cmd.Wait()
}

func modifyDockerComposeCommand(appCmd, newComposeFile string) string {
	// Ensure newComposeFile starts with ./
	if !strings.HasPrefix(newComposeFile, "./") {
		newComposeFile = "./" + newComposeFile
	}

	// Define a regular expression pattern to match "-f <file>"
	pattern := `(-f\s+("[^"]+"|'[^']+'|\S+))`
	re := regexp.MustCompile(pattern)

	// Check if the "-f <file>" pattern exists in the appCmd
	if re.MatchString(appCmd) {
		// Replace it with the new Compose file
		return re.ReplaceAllString(appCmd, fmt.Sprintf("-f %s", newComposeFile))
	}

	// If the pattern doesn't exist, inject the new Compose file right after "docker-compose" or "docker compose"
	upIdx := strings.Index(appCmd, " up")
	if upIdx != -1 {
		return fmt.Sprintf("%s -f %s%s", appCmd[:upIdx], newComposeFile, appCmd[upIdx:])
	}

	return fmt.Sprintf("%s -f %s", appCmd, newComposeFile)
}

func parseDockerCmd(cmd string) (string, string, error) {
	// Regular expression patterns
	containerNamePattern := `--name\s+([^\s]+)`
	networkNamePattern := `(--network|--net)\s+([^\s]+)`

	// Extract container name
	containerNameRegex := regexp.MustCompile(containerNamePattern)
	containerNameMatches := containerNameRegex.FindStringSubmatch(cmd)
	if len(containerNameMatches) < 2 {
		return "", "", fmt.Errorf("failed to parse container name")
	}
	containerName := containerNameMatches[1]

	// Extract network name
	networkNameRegex := regexp.MustCompile(networkNamePattern)
	networkNameMatches := networkNameRegex.FindStringSubmatch(cmd)
	if len(networkNameMatches) < 3 {
		return containerName, "", fmt.Errorf("failed to parse network name")
	}
	networkName := networkNameMatches[2]

	return containerName, networkName, nil
}

func getInode(pid int) (uint64, error) {
	path := filepath.Join("/proc", strconv.Itoa(pid), "ns", "pid")

	f, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	// Dev := (f.Sys().(*syscall.Stat_t)).Dev
	i := (f.Sys().(*syscall.Stat_t)).Ino
	if i == 0 {
		return 0, fmt.Errorf("failed to get the inode of the process")
	}
	return i, nil
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
