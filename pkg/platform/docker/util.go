package docker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
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
	if DockerConfig.Envs == nil {
		DockerConfig.Envs = map[string]string{
			"INSTALLATION_ID": conf.InstallationID,
		}
	} else {
		DockerConfig.Envs["INSTALLATION_ID"] = conf.InstallationID
	}

	client, err := New(logger, conf)
	if err != nil {
		return "", fmt.Errorf("failed to initialise docker: %w", err)
	}

	err = client.CreateVolume(ctx, "debugfs", true, map[string]string{
		"type":   "debugfs",
		"device": "debugfs",
	})
	if err != nil {
		utils.LogError(logger, err, "failed to create debugfs volume")
	}

	keployalias, err := getAlias(ctx, logger, opts, conf.Debug)
	if err != nil {
		return "", err
	}
	return keployalias, nil
}

func getAlias(ctx context.Context, logger *zap.Logger, opts models.SetupOptions, debug bool) (string, error) {
	osName := runtime.GOOS

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

	tlsVolumeMount := fmt.Sprintf("-v %s:%s ", KeployTLSVolumeName, KeployTLSMountPath)

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
	Volumes = Volumes + tlsVolumeMount

	extraArgs := opts.ExtraArgs

	switch osName {
	case "linux":
		alias := "sudo docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs +
			"-e BINARY_TO_DOCKER=true -p " +
			fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
			" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
			" --cap-add=BPF --cap-add=PERFMON --cap-add=NET_ADMIN --cap-add=SYS_RESOURCE --cap-add=SYS_PTRACE " + Volumes +
			" -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf " +
			" --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
			" --mode " + string(opts.Mode) + " --dns-port " + fmt.Sprintf("%d", opts.DnsPort) + " --is-docker"

		if opts.EnableTesting {
			alias += " --enable-testing"
		}
		alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
		alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)

		if opts.GlobalPassthrough {
			alias += " --global-passthrough"
		}
		if opts.BuildDelay > 0 {
			alias += fmt.Sprintf(" --build-delay %d", opts.BuildDelay)
		}
		if models.IsAnsiDisabled {
			alias += " --disable-ansi"
		}
		if len(opts.PassThroughPorts) > 0 {
			portStrings := make([]string, len(opts.PassThroughPorts))
			for i, port := range opts.PassThroughPorts {
				portStrings[i] = strconv.Itoa(int(port))
			}
			alias += fmt.Sprintf(" --pass-through-ports=%s", strings.Join(portStrings, ","))
		}
		if debug {
			alias += " --debug"
		}
		if opts.ConfigPath != "" && opts.ConfigPath != "." {
			alias += " --config-path " + opts.ConfigPath
		}
		if opts.Synchronous {
			alias += " --sync"
		}
		if len(extraArgs) > 0 {
			alias += " " + strings.Join(extraArgs, " ")
		}
		return alias, nil

	case "windows":
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
			alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs +
				"-e BINARY_TO_DOCKER=true -p " +
				fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
				" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
				" --cap-add=BPF --cap-add=PERFMON --cap-add=NET_ADMIN --cap-add=SYS_RESOURCE --cap-add=SYS_PTRACE " + Volumes +
				" -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf " +
				" --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
				" --mode " + string(opts.Mode) + " --dns-port " + fmt.Sprintf("%d", opts.DnsPort) + " --is-docker"

			if opts.EnableTesting {
				alias += " --enable-testing"
			}
			alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
			alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)

			if opts.GlobalPassthrough {
				alias += " --global-passthrough"
			}
			if opts.BuildDelay > 0 {
				alias += fmt.Sprintf(" --build-delay %d", opts.BuildDelay)
			}
			if models.IsAnsiDisabled {
				alias += " --disable-ansi"
			}
			if len(opts.PassThroughPorts) > 0 {
				portStrings := make([]string, len(opts.PassThroughPorts))
				for i, port := range opts.PassThroughPorts {
					portStrings[i] = strconv.Itoa(int(port))
				}
				alias += fmt.Sprintf(" --pass-through-ports=%s", strings.Join(portStrings, ","))
			}
			if debug {
				alias += " --debug"
			}
			if opts.ConfigPath != "" && opts.ConfigPath != "." {
				alias += " --config-path " + opts.ConfigPath
			}
			if opts.Synchronous {
				alias += " --sync"
			}
			if len(extraArgs) > 0 {
				alias += " " + strings.Join(extraArgs, " ")
			}
			return alias, nil
		}

		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs +
			"-e BINARY_TO_DOCKER=true -p " +
			fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
			" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
			" --cap-add=BPF --cap-add=PERFMON --cap-add=NET_ADMIN --cap-add=SYS_RESOURCE --cap-add=SYS_PTRACE " + Volumes +
			" -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf " +
			" --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
			" --mode " + string(opts.Mode) + " --dns-port " + fmt.Sprintf("%d", opts.DnsPort) + " --is-docker"

		if opts.EnableTesting {
			alias += " --enable-testing"
		}
		alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
		alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)

		if opts.GlobalPassthrough {
			alias += " --global-passthrough"
		}
		if opts.BuildDelay > 0 {
			alias += fmt.Sprintf(" --build-delay %d", opts.BuildDelay)
		}
		if models.IsAnsiDisabled {
			alias += " --disable-ansi"
		}
		if len(opts.PassThroughPorts) > 0 {
			portStrings := make([]string, len(opts.PassThroughPorts))
			for i, port := range opts.PassThroughPorts {
				portStrings[i] = strconv.Itoa(int(port))
			}
			alias += fmt.Sprintf(" --pass-through-ports=%s", strings.Join(portStrings, ","))
		}
		if debug {
			alias += " --debug"
		}
		if opts.ConfigPath != "" && opts.ConfigPath != "." {
			alias += " --config-path " + opts.ConfigPath
		}
		if opts.Synchronous {
			alias += " --sync"
		}
		if len(extraArgs) > 0 {
			alias += " " + strings.Join(extraArgs, " ")
		}
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
			alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs +
				"-e BINARY_TO_DOCKER=true -p " +
				fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
				" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
				" --cap-add=BPF --cap-add=PERFMON --cap-add=NET_ADMIN --cap-add=SYS_RESOURCE --cap-add=SYS_PTRACE " + Volumes +
				" -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf " +
				" --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
				" --mode " + string(opts.Mode) + " --dns-port " + fmt.Sprintf("%d", opts.DnsPort) + " --is-docker"

			if opts.EnableTesting {
				alias += " --enable-testing"
			}
			alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
			alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)

			if opts.GlobalPassthrough {
				alias += " --global-passthrough"
			}
			if opts.BuildDelay > 0 {
				alias += fmt.Sprintf(" --build-delay %d", opts.BuildDelay)
			}
			if models.IsAnsiDisabled {
				alias += " --disable-ansi"
			}
			if len(opts.PassThroughPorts) > 0 {
				portStrings := make([]string, len(opts.PassThroughPorts))
				for i, port := range opts.PassThroughPorts {
					portStrings[i] = strconv.Itoa(int(port))
				}
				alias += fmt.Sprintf(" --pass-through-ports=%s", strings.Join(portStrings, ","))
			}
			if debug {
				alias += " --debug"
			}
			if opts.ConfigPath != "" && opts.ConfigPath != "." {
				alias += " --config-path " + opts.ConfigPath
			}
			if opts.Synchronous {
				alias += " --sync"
			}
			if len(extraArgs) > 0 {
				alias += " " + strings.Join(extraArgs, " ")
			}
			return alias, nil
		}

		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		alias := "docker container run --name " + opts.KeployContainer + appNetworkStr + " " + envs +
			"-e BINARY_TO_DOCKER=true -p " +
			fmt.Sprintf("%d", opts.AgentPort) + ":" + fmt.Sprintf("%d", opts.AgentPort) +
			" -p " + fmt.Sprintf("%d", opts.ProxyPort) + ":" + fmt.Sprintf("%d", opts.ProxyPort) + appPortsStr +
			" --cap-add=BPF --cap-add=PERFMON --cap-add=NET_ADMIN --cap-add=SYS_RESOURCE --cap-add=SYS_PTRACE " + Volumes +
			" -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf " +
			" --rm " + img + " --client-pid " + fmt.Sprintf("%d", opts.ClientNSPID) +
			" --mode " + string(opts.Mode) + " --dns-port " + fmt.Sprintf("%d", opts.DnsPort) + " --is-docker"

		if opts.EnableTesting {
			alias += " --enable-testing"
		}
		alias += " --port " + fmt.Sprintf("%d", opts.AgentPort)
		alias += " --proxy-port " + fmt.Sprintf("%d", opts.ProxyPort)

		if opts.GlobalPassthrough {
			alias += " --global-passthrough"
		}
		if opts.BuildDelay > 0 {
			alias += fmt.Sprintf(" --build-delay %d", opts.BuildDelay)
		}
		if models.IsAnsiDisabled {
			alias += " --disable-ansi"
		}
		if len(opts.PassThroughPorts) > 0 {
			portStrings := make([]string, len(opts.PassThroughPorts))
			for i, port := range opts.PassThroughPorts {
				portStrings[i] = strconv.Itoa(int(port))
			}
			alias += fmt.Sprintf(" --pass-through-ports=%s", strings.Join(portStrings, ","))
		}
		if debug {
			alias += " --debug"
		}
		if opts.ConfigPath != "" && opts.ConfigPath != "." {
			alias += " --config-path " + opts.ConfigPath
		}
		if opts.Synchronous {
			alias += " --sync"
		}
		if len(extraArgs) > 0 {
			alias += " " + strings.Join(extraArgs, " ")
		}
		return alias, nil
	}

	return "", errors.New("failed to get alias")
}

func ParseDockerCmd(cmd string, kind utils.CmdType, idc Client) (string, string, error) {
	var containerNamePattern string
	switch kind {
	case utils.DockerStart:
		containerNamePattern = `start\s+(?:-[^\s]+\s+)*([^\s]*)`
	default:
		containerNamePattern = `--name\s+([^\s]+)`
	}
	networkNamePattern := `(--network|--net)\s+([^\s]+)`

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

	networkNameRegex := regexp.MustCompile(networkNamePattern)
	networkNameMatches := networkNameRegex.FindStringSubmatch(cmd)
	if len(networkNameMatches) < 3 {
		return containerName, "", fmt.Errorf("failed to parse network name")
	}
	networkName := networkNameMatches[2]

	return containerName, networkName, nil
}

