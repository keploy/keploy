//go:build !windows

package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"github.com/docker/docker/api/types/network"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/term"
)

type ConfigStruct struct {
	DockerImage string
	Envs        map[string]string
}

var DockerConfig = ConfigStruct{
	DockerImage: "ghcr.io/keploy/keploy",
}

func ParseDockerCmd(cmd string, kind utils.CmdType, idc Client) (string, string, error) {

	// Regular expression patterns
	var containerNamePattern string
	switch kind {
	case utils.DockerStart:
		containerNamePattern = `start\s+(?:-[^\s]+\s+)*([^\s]*)`
	default:
		containerNamePattern = `--name\s+([^\s]+)`
	}

	networkNamePattern := `(--network|--net)\s+([^\s]+)`

	// Extract container name
	containerNameRegex := regexp.MustCompile(containerNamePattern)
	containerNameMatches := containerNameRegex.FindStringSubmatch(cmd)
	if len(containerNameMatches) < 2 {
		return "", "", fmt.Errorf("failed to parse container name")
	}
	containerName := containerNameMatches[1]

	if kind == utils.DockerStart {
		networks, err := idc.ExtractNetworksForContainer(containerName)
		if err != nil {
			return containerName, "", err
		}
		for i := range networks {
			return containerName, i, nil
		}
		return containerName, "", fmt.Errorf("failed to parse network name")
	}

	// Extract network name
	networkNameRegex := regexp.MustCompile(networkNamePattern)
	networkNameMatches := networkNameRegex.FindStringSubmatch(cmd)
	if len(networkNameMatches) < 3 {
		return containerName, "", fmt.Errorf("failed to parse network name")
	}
	networkName := networkNameMatches[2]

	return containerName, networkName, nil
}

func GenerateDockerEnvs(config ConfigStruct) string {
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

	client, err := New(logger)
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

	//nolint:staticcheck // runtime.GOOS lint suppression
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
			keployAlias,
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid: true,
		}
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
	cmd.Stderr = os.Stderr

	logger.Info("running the following command in docker", zap.String("command", cmd.String()))
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
		// ttyFlag = " -it "
		ttyFlag = " "
	} else {
		ttyFlag = " "
	}

	switch osName {
	case "linux":
		alias := "sudo docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + ttyFlag + " -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
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
			alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + ttyFlag + "-v " + pwd + ":" + dpwd + " -w " + dpwd + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") + "\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img
			return alias, nil
		}
		// if default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + ttyFlag + "-v " + pwd + ":" + dpwd + " -w " + dpwd + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") + "\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img
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
			alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + ttyFlag + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
			return alias, nil
		}
		// if default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + ttyFlag + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
		return alias, nil
	}
	return "", errors.New("failed to get alias")
}

func addKeployNetwork(ctx context.Context, logger *zap.Logger, client Client) {

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

// ExtractPidNamespaceInode extracts the inode of the PID namespace of a given PID
func ExtractPidNamespaceInode(pid int) (string, error) {
	// Check the OS
	if runtime.GOOS != "linux" {
		// Execute command in the container to get the PID namespace
		output, err := exec.Command("docker", "exec", "keploy-init", "stat", "/proc/1/ns/pid").Output()
		if err != nil {
			return "", err
		}
		outputStr := string(output)

		// Use a regular expression to extract the inode from the output
		re := regexp.MustCompile(`pid:\[(\d+)\]`)
		match := re.FindStringSubmatch(outputStr)

		if len(match) < 2 {
			return "", fmt.Errorf("failed to extract PID namespace inode")
		}

		pidNamespace := match[1]
		return pidNamespace, nil
	}

	// Check the namespace file in /proc
	nsPath := fmt.Sprintf("/proc/%d/ns/pid", pid)
	fileInfo, err := os.Stat(nsPath)
	if err != nil {
		return "", err
	}

	// Retrieve inode number
	inode := fileInfo.Sys().(*syscall.Stat_t).Ino
	return fmt.Sprintf("%d", inode), nil
}
