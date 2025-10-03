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

	"github.com/docker/docker/api/types"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type DockerConfigStruct struct {
	DockerImage  string
	Envs         map[string]string
	VolumeMounts []string
}

var DockerConfig = DockerConfigStruct{
	DockerImage: "ghcr.io/keploy/keploy",
}

func GenerateDockerEnvs(config DockerConfigStruct) string {
	var envs []string
	for key, value := range config.Envs {
		if runtime.GOOS == "windows" {
			envs = append(envs, fmt.Sprintf("-e %s=%s", key, value))
		} else {
			envs = append(envs, fmt.Sprintf("-e %s='%s'", key, value))
		}
	}
	return strings.Join(envs, " ")
}

// StartInDocker will check if the docker command is provided as an input
// then start the Keploy as a docker container and run the command
// should also return a boolean if the execution is moved to docker
func StartInDocker(ctx context.Context, logger *zap.Logger, conf *config.Config, opts models.SetupOptions) error {

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
	// TODO: Not workign correctly
	// cmdType := utils.FindDockerCmd(conf.Command)
	// if conf.InDocker || !(utils.IsDockerCmd(cmdType)) {
	// 	fmt.Println("Keploy is running outside docker...")
	// 	return nil
	// }
	fmt.Println("Keploy is running inside docker...")
	// pass the all the commands and args to the docker version of Keploy
	err := RunInDocker(ctx, logger, opts)
	if err != nil {
		utils.LogError(logger, err, "failed to run the command in docker")
		return err
	}
	// gracefully exit the current process
	logger.Info("exiting the current process as the command is moved to docker")

	// if utils.LogFile != nil {
	// 	_ = utils.LogFile.Close()
	// 	if err := utils.DeleteFileIfNotExists(logger, "keploy-logs.txt"); err != nil {
	// 		return nil
	// 	}
	// 	if err := utils.DeleteFileIfNotExists(logger, "docker-compose-tmp.yaml"); err != nil {
	// 		return nil
	// 	}
	// }

	// os.Exit(0)
	return nil
}

func GetDockerCommandAndSetup(ctx context.Context, logger *zap.Logger, conf *config.Config, opts models.SetupOptions) (keployAlias string, err error) {
	// Preserves your environment variable setup
	if DockerConfig.Envs == nil {
		DockerConfig.Envs = map[string]string{
			"INSTALLATION_ID": conf.InstallationID,
		}
	} else {
		DockerConfig.Envs["INSTALLATION_ID"] = conf.InstallationID
	}

	// Preserves your Docker client initialization and setup
	client, err := New(logger)
	if err != nil {
		return "", fmt.Errorf("failed to initialise docker: %w", err)
	}

	addKeployNetwork(ctx, logger, client)
	err = client.CreateVolume(ctx, "debugfs", true, map[string]string{
		"type":   "debugfs",
		"device": "debugfs",
	})
	if err != nil {
		// Log the error but don't fail, consistent with original logic.
		utils.LogError(logger, err, "failed to create debugfs volume")
	}

	// Preserves the alias generation
	keployalias, err := getAlias(ctx, logger, opts)
	if err != nil {
		return "", err
	}

	// Split the alias into program and arguments for direct execution
	// parts := strings.Fields(keployAlias)
	// if len(parts) == 0 {
	// 	return "", fmt.Errorf("generated docker command alias is empty")
	// }

	return keployalias, nil
}

func RunInDocker(ctx context.Context, logger *zap.Logger, opts models.SetupOptions) error {

	//Get the correct keploy alias.
	keployAlias, err := getAlias(ctx, logger, opts)
	if err != nil {
		return err
	}
	client, err := New(logger)
	if err != nil {
		utils.LogError(logger, err, "failed to initalise docker")
		return err
	}

	// addKeployNetwork(ctx, logger, client)
	err = client.CreateVolume(ctx, "debugfs", true, map[string]string{
		"type":   "debugfs",
		"device": "debugfs",
	})
	if err != nil {
		utils.LogError(logger, err, "failed to debugfs volume")
		return err
	}

	cmd := PrepareDockerCommand(ctx, keployAlias)

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
			logger.Info("Docker agent run cancelled gracefully.")
			return nil
		}
		utils.LogError(logger, err, "failed to start keploy in docker")
		return err
	}
	return nil
}

func getAlias(ctx context.Context, logger *zap.Logger, opts models.SetupOptions) (string, error) {
	// Get the name of the operating system.
	osName := runtime.GOOS
	//TODO: configure the hardcoded port mapping
	img := DockerConfig.DockerImage + ":v" + utils.Version
	logger.Info("Starting keploy in docker with image", zap.String("image:", img))
	envs := GenerateDockerEnvs(DockerConfig)
	if envs != "" {
		envs = envs + " "
	}
	appPortsStr := ""
	if len(opts.AppPorts) > 0 {
		appPortsStr = " " + strings.Join(opts.AppPorts, " ")
	}
	appNetworkStr := ""
	if opts.AppNetwork != "" {
		appNetworkStr = " --network " + opts.AppNetwork
	}

	switch osName {
	case "linux":
		alias := "sudo docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs + "-e BINARY_TO_DOCKER=true -p " +
			fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
			" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
			" --privileged" + " -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") +
			" -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") +
			"/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
			" --docker-network " + opts.DockerNetwork + " --agent-ip " + opts.AgentIP + " --mode " + string(opts.Mode)

		if opts.EnableTesting {
			alias += " --enable-testing"
		}
		alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
		// alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)

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
			// alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + "-v " + pwd + ":" + dpwd + " -w " + dpwd + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") + "\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img
			// return alias, nil

			alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs + "-e BINARY_TO_DOCKER=true -p " +
				fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
				" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
				" --privileged" + " -v " + pwd + ":" + dpwd + " -w " + dpwd +
				" -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") +
				"\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
				" --docker-network " + opts.DockerNetwork + " --agent-ip " + opts.AgentIP + " --mode " + string(opts.Mode)

			if opts.EnableTesting {
				alias += " --enable-testing"
			}
			alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
			return alias, nil
		}
		// if default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		// alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + "-v " + pwd + ":" + dpwd + " -w " + dpwd + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") + "\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img
		alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs + "-e BINARY_TO_DOCKER=true -p " +
			fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
			" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
			" --privileged" + " -v " + pwd + ":" + dpwd + " -w " + dpwd +
			" -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") +
			"\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
			" --docker-network " + opts.DockerNetwork + " --agent-ip " + opts.AgentIP + " --mode " + string(opts.Mode)

		if opts.EnableTesting {
			alias += " --enable-testing"
		}
		alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
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
			// alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
			// return alias, nil
			logger.Info("Starting keploy in docker with colima context, as that is the current context.")
			alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs + "-e BINARY_TO_DOCKER=true -p " +
				fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
				" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
				" --privileged" + " -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") +
				" -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") +
				"/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
				" --docker-network " + opts.DockerNetwork + " --agent-ip " + opts.AgentIP + " --mode " + string(opts.Mode)

			if opts.EnableTesting {
				alias += " --enable-testing"
			}
			alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
			return alias, nil
		}
		// if default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		// alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
		// return alias, nil
		alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs + "-e BINARY_TO_DOCKER=true -p " +
			fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
			" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
			" --privileged" + " -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") +
			" -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") +
			"/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
			" --docker-network " + opts.DockerNetwork + " --agent-ip " + opts.AgentIP + " --mode " + string(opts.Mode)

		if opts.EnableTesting {
			alias += " --enable-testing"
		}
		alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
		return alias, nil
	}
	return "", errors.New("failed to get alias")
}
func addKeployNetwork(ctx context.Context, logger *zap.Logger, client Client) {

	// Check if the 'keploy-network' network exists
	networks, err := client.NetworkList(ctx, types.NetworkListOptions{})
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
	_, err = client.NetworkCreate(ctx, "keploy-network", types.NetworkCreate{
		CheckDuplicate: true,
	})
	if err != nil {
		logger.Debug("failed to create keploy network")
		return
	}

	logger.Debug("keploy network created")
}

func convertPathToUnixStyle(path string) string {
	// Replace backslashes with forward slashes
	unixPath := strings.ReplaceAll(path, "\\", "/")
	// Remove 'C:'
	if len(unixPath) > 1 && unixPath[1] == ':' {
		unixPath = unixPath[2:]
	}
	return unixPath
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
