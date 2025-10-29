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

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"

	"go.keploy.io/server/v3/utils"
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

func GetKeployDockerAlias(ctx context.Context, logger *zap.Logger, conf *config.Config, opts models.SetupOptions) (keployAlias string, err error) {
	// Preserves your environment variable setup
	if DockerConfig.Envs == nil {
		DockerConfig.Envs = map[string]string{
			"INSTALLATION_ID": conf.InstallationID,
		}
	} else {
		DockerConfig.Envs["INSTALLATION_ID"] = conf.InstallationID
	}

	// Preserves your Docker client initialization and setup
	client, err := New(logger, conf)
	if err != nil {
		return "", fmt.Errorf("failed to initialise docker: %w", err)
	}

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

	return keployalias, nil
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
	if len(opts.AppNetworks) > 0 {
		for _, network := range opts.AppNetworks {
			appNetworkStr += " --network " + network
		}
	}

	Volumes := ""
	for i, volume := range DockerConfig.VolumeMounts {
		if i != 0 {
			Volumes = Volumes + "-v " + volume
		} else {
			Volumes = "-v " + volume
		}
		if i == len(DockerConfig.VolumeMounts)-1 {
			Volumes = Volumes + " "
		}
	}

	switch osName {
	case "linux":
		alias := "sudo docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs + "-e BINARY_TO_DOCKER=true -p " +
			fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
			" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
			" --privileged " + Volumes + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") +
			" -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") +
			"/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) + " --mode " + string(opts.Mode)

		if opts.EnableTesting {
			alias += " --enable-testing"
		}
		alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
		alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)

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
				" --privileged " + Volumes + "-v " + pwd + ":" + dpwd + " -w " + dpwd +
				" -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") +
				"\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
				" --mode " + string(opts.Mode)

			if opts.EnableTesting {
				alias += " --enable-testing"
			}
			alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
			alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)
			return alias, nil
		}
		// if default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		// alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 36789:36789 -p 8096:8096 --privileged --pid=host" + "-v " + pwd + ":" + dpwd + " -w " + dpwd + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") + "\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img
		alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs + "-e BINARY_TO_DOCKER=true -p " +
			fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
			" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
			" --privileged " + Volumes + "-v " + pwd + ":" + dpwd + " -w " + dpwd +
			" -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") +
			"\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
			" --mode " + string(opts.Mode)

		if opts.EnableTesting {
			alias += " --enable-testing"
		}
		alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
		alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)
		return alias, nil
	case "darwin":
		// Get the current Docker context and endpoint
		cmd := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{if .Metadata}}Name={{.Name}} {{end}}{{if .Endpoints.docker}}Endpoint={{.Endpoints.docker.Host}}{{end}}")
		out, err := cmd.Output()
		if err != nil {
			utils.LogError(logger, err, "failed to inspect the docker context")
			return "", errors.New("failed to get alias")
		}

		output := strings.TrimSpace(string(out))
		var currentContext, dockerEndpoint string

		// Parse the output for current context and endpoint
		for _, part := range strings.Fields(output) {
			if strings.HasPrefix(part, "Name=") {
				currentContext = strings.TrimPrefix(part, "Name=")
			} else if strings.HasPrefix(part, "Endpoint=") {
				dockerEndpoint = strings.TrimPrefix(part, "Endpoint=")
			}
		}

		// Check if we found a current context
		if currentContext == "" {
			utils.LogError(logger, nil, "failed to find the current docker context")
			return "", errors.New("failed to get alias")
		}

		var alias string
		
		// Handle non-default Docker contexts
		if currentContext != "default" && dockerEndpoint != "" {
			// Extract the socket path from the Docker endpoint
			var dockerSocketPath string
			if strings.HasPrefix(dockerEndpoint, "unix://") {
				dockerSocketPath = strings.TrimPrefix(dockerEndpoint, "unix://")
			} else {
				// Fallback to default socket if we can't parse the endpoint
				dockerSocketPath = "/var/run/docker.sock"
			}

			// Set DOCKER_HOST environment variable for all non-default contexts
			err := os.Setenv("DOCKER_HOST", dockerEndpoint)
			if err != nil {
				utils.LogError(logger, err, "failed to set DOCKER_HOST environment variable for "+currentContext+" context")
				return "", errors.New("failed to get alias")
			}
			
			logger.Info("Starting keploy in docker with "+currentContext+" context, as that is the current context.")
			
			// Use the correct socket path from the current Docker context
			alias = "docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs + "-e BINARY_TO_DOCKER=true -p " +
				fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
				" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
				" --privileged " + Volumes + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") +
				" -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v " + dockerSocketPath + ":/var/run/docker.sock -v " + os.Getenv("HOME") +
				"/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
				" --mode " + string(opts.Mode)

			if opts.EnableTesting {
				alias += " --enable-testing"
			}
			alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
			alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)
			return alias, nil
		}
		
		// Default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		alias = "docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs + "-e BINARY_TO_DOCKER=true -p " +
			fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
			" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
			" --privileged " + Volumes + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") +
			" -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") +
			"/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
			" --mode " + string(opts.Mode)

		if opts.EnableTesting {
			alias += " --enable-testing"
		}
		alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
		alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)
		return alias, nil
	}
	return "", errors.New("failed to get alias")
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
