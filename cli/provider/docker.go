package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/docker/docker/api/types/network"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/docker"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/term"
)

type DockerConfigStruct struct {
	DockerImage string
	Envs        map[string]string
}

var DockerConfig = DockerConfigStruct{
	DockerImage: "ghcr.io/keploy/keploy",
}

func GenerateDockerEnvs(config DockerConfigStruct) string {
	var envs []string
	for key, value := range config.Envs {
		envs = append(envs, fmt.Sprintf("-e %s='%s'", key, value))
	}
	return strings.Join(envs, " ")
}

// StartInDocker will check if the docker command is provided as an input
// then start the Keploy as a docker container and run the command
// should also return a boolean if the execution is moved to docker
func StartInDocker(ctx context.Context, logger *zap.Logger, conf *config.Config) error {

	if DockerConfig.Envs == nil {
		DockerConfig.Envs = map[string]string{
			"INSTALLATION_ID": conf.InstallationID,
		}
	} else {
		DockerConfig.Envs["INSTALLATION_ID"] = conf.InstallationID
	}

	//Check if app command starts with docker or docker-compose.
	// If it does, then we would run the docker version of keploy and
	// pass the command and control to it.
	cmdType := utils.FindDockerCmd(conf.Command)
	if conf.InDocker || !(utils.IsDockerCmd(cmdType)) {
		return nil
	}
	// pass the all the commands and args to the docker version of Keploy
	err := RunInDocker(ctx, logger)
	if err != nil {
		utils.LogError(logger, err, "failed to run the command in docker")
		return err
	}
	// gracefully exit the current process
	logger.Info("exiting the current process as the command is moved to docker")
	os.Exit(0)
	return nil
}

func RunInDocker(ctx context.Context, logger *zap.Logger) error {
	//Get the correct keploy alias.
	keployAlias, err := getAlias(ctx, logger)
	if err != nil {
		return err
	}

	var quotedArgs []string

	for _, arg := range os.Args[1:] {
		quotedArgs = append(quotedArgs, strconv.Quote(arg))
	}
	client, err := docker.New(logger)
	if err != nil {
		utils.LogError(logger, err, "failed to initalise docker")
		return err
	}
	addKeployNetwork(ctx, logger, client)
	err = client.CreateVolume(ctx, "debugfs", true)
	if err != nil {
		utils.LogError(logger, err, "failed to debugfs volume")
		return err
	}

	var cmd *exec.Cmd

	// Detect the operating system
	if runtime.GOOS == "windows" {
		var args []string
		args = append(args, "/C")
		args = append(args, strings.Split(keployAlias, " ")...)
		args = append(args, os.Args[1:]...)
		// Use cmd.exe /C for Windows
		cmd = exec.CommandContext(
			ctx,
			"cmd.exe",
			args...,
		)
	} else {
		// Use sh -c for Unix-like systems
		cmd = exec.CommandContext(
			ctx,
			"sh",
			"-c",
			keployAlias+" "+strings.Join(quotedArgs, " "),
		)
	}

	cmd.Cancel = func() error {
		err := utils.SendSignal(logger, -cmd.Process.Pid, syscall.SIGINT)
		if err != nil {
			utils.LogError(logger, err, "failed to start stop docker")
			return err
		}
		return nil
	}

	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr

	logger.Debug("running the following command in docker", zap.String("command", cmd.String()))
	err = cmd.Run()
	if err != nil {
		if ctx.Err() == context.Canceled {
			return ctx.Err()
		}
		utils.LogError(logger, err, "failed to start keploy in docker")
		return err
	}
	return nil
}

func getAlias(ctx context.Context, logger *zap.Logger) (string, error) {
	// Get the name of the operating system.
	osName := runtime.GOOS
	//TODO: configure the hardcoded port mapping
	img := DockerConfig.DockerImage + ":v" + utils.Version
	logger.Info("Starting keploy in docker with image", zap.String("image:", img))
	envs := GenerateDockerEnvs(DockerConfig)
	if envs != "" {
		envs = envs + " "
	}
	var ttyFlag string

	if term.IsTerminal(int(os.Stdin.Fd())) {
		ttyFlag = " -it "
	} else {
		ttyFlag = " "
	}

	switch osName {
	case "linux":
		alias := "sudo docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + " -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
		return alias, nil
	case "windows":
		// Get the current working directory
		pwd, err := os.Getwd()
		if err != nil {
			utils.LogError(logger, err, "failed to get the current working directory")
		}
		dpwd := convertPathToUnixStyle(pwd)
		cmd := exec.CommandContext(ctx, "docker", "context", "ls", "--format", "{{.Name}}\t{{.Current}}")
		out, err := cmd.Output()
		if err != nil {
			utils.LogError(logger, err, "failed to get the current docker context")
			return "", errors.New("failed to get alias")
		}
		dockerContext := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
		if len(dockerContext) == 0 {
			utils.LogError(logger, nil, "failed to get the current docker context")
			return "", errors.New("failed to get alias")
		}
		dockerContext = strings.Split(dockerContext, "\n")[0]
		if dockerContext == "colima" {
			logger.Info("Starting keploy in docker with colima context, as that is the current context.")
			alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + "-v " + pwd + ":" + dpwd + " -w " + dpwd + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") + "\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img
			return alias, nil
		}
		// if default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + "-v " + pwd + ":" + dpwd + " -w " + dpwd + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") + "\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img
		return alias, nil
	case "darwin":
		cmd := exec.CommandContext(ctx, "docker", "context", "ls", "--format", "{{.Name}}\t{{.Current}}")
		out, err := cmd.Output()
		if err != nil {
			utils.LogError(logger, err, "failed to get the current docker context")
			return "", errors.New("failed to get alias")
		}
		dockerContext := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
		if len(dockerContext) == 0 {
			utils.LogError(logger, nil, "failed to get the current docker context")
			return "", errors.New("failed to get alias")
		}
		dockerContext = strings.Split(dockerContext, "\n")[0]
		if dockerContext == "colima" {
			logger.Info("Starting keploy in docker with colima context, as that is the current context.")
			alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
			return alias, nil
		}
		// if default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
		return alias, nil

	}
	return "", errors.New("failed to get alias")
}

func addKeployNetwork(ctx context.Context, logger *zap.Logger, client docker.Client) {

	// Check if the 'keploy-network' network exists
	networks, err := client.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		logger.Debug("failed to list docker networks")
		return
	}

	for _, network := range networks {
		if network.Name == "keploy-network" {
			logger.Debug("keploy network already exists")
			return
		}
	}

	// Create the 'keploy' network if it doesn't exist
	_, err = client.NetworkCreate(ctx, "keploy-network", network.CreateOptions{})
	if err != nil {
		logger.Debug("failed to create keploy network")
		return
	}

	logger.Debug("keploy network created")
}

func convertPathToUnixStyle(path string) string {
	// Replace backslashes with forward slashes
	unixPath := strings.Replace(path, "\\", "/", -1)
	// Remove 'C:'
	if len(unixPath) > 1 && unixPath[1] == ':' {
		unixPath = unixPath[2:]
	}
	return unixPath
}
