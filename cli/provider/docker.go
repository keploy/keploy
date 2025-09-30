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

	"github.com/docker/docker/api/types"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/platform/docker"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"golang.org/x/term"
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

	if utils.LogFile != nil {
		err := utils.LogFile.Close()
		if err != nil {
			utils.LogError(logger, err, "Failed to close Keploy Logs")
		}
		if err := utils.DeleteFileIfNotExists(logger, "keploy-logs.txt"); err != nil {
			return nil
		}
		if err := utils.DeleteFileIfNotExists(logger, "docker-compose-tmp.yaml"); err != nil {
			return nil
		}
	}

	os.Exit(0)
	return nil
}

// quoteAppCmdArgsWindows takes args (excluding argv[0]) and ensures that the
// application command after "-c" is merged and quoted (e.g. -c "docker compose up").
func quoteAppCmdArgsWindows(args []string, logger *zap.Logger) []string {
	idx := -1
	for i, a := range args {
		if a == "-c" {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(args) {
		logger.Debug("no -c or nothing after -c", zap.String("args", strings.Join(args, " ")))
		return args // no -c or nothing after -c
	}

	// Collect app command tokens after -c until the next flag (starting with '-')
	j := idx + 1
	var app []string
	for ; j < len(args); j++ {
		if strings.HasPrefix(args[j], "-") {
			break
		}
		app = append(app, args[j])
	}
	if len(app) == 0 {
		return args // nothing to quote
	}
	logger.Debug("app in quoteAppCmdArgsWindows", zap.String("app", strings.Join(app, " ")))

	quoted := strconv.Quote(strings.Join(app, " "))

	// Rebuild: everything before -c, then -c "joined app", then the remaining flags
	out := make([]string, 0, len(args)-(len(app)-1))
	out = append(out, args[:idx]...)
	out = append(out, "-c", quoted)
	out = append(out, args[j:]...)
	return out
}

// Merge everything after -c into one token until the next flag (starting with '-').
func mergeAppCmdAfterDashC(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" && i+1 < len(args) {
			j := i + 1
			var app []string
			for ; j < len(args); j++ {
				if strings.HasPrefix(args[j], "-") {
					break
				}
				app = append(app, args[j])
			}
			if len(app) > 0 {
				out = append(out, "-c", strings.Join(app, " "))
				i = j - 1
				continue
			}
		}
		out = append(out, args[i])
	}
	return out
}

func splitFieldsPreserveSpaces(s string) []string {
	// Simple fields split is fine for keployAlias (it has no quoted spaces).
	return strings.Fields(s)
}

func RunInDocker(ctx context.Context, logger *zap.Logger) error {
	client, err := docker.New(logger)
	if err != nil {
		utils.LogError(logger, err, "failed to initialize docker")
		return err
	}

	// create all the volumes from DockerConfig.VolumeMounts
	for _, volume := range DockerConfig.VolumeMounts {
		volumeName := volume
		if strings.Contains(volume, ":") {
			volumeName = strings.Split(volume, ":")[0]
		}
		logger.Debug("creating volume", zap.String("volume", volumeName))
		err := client.CreateVolume(ctx, volumeName, true, nil)
		if err != nil {
			utils.LogError(logger, err, "failed to create volume "+volumeName)
			return err
		}
	}

	//Get the correct keploy alias.
	keployAlias, err := getAlias(ctx, logger)
	if err != nil {
		return err
	}
	logger.Debug("keployAlias", zap.String("keployAlias", keployAlias))

	var quotedArgs []string

	for _, arg := range os.Args[1:] {
		quotedArgs = append(quotedArgs, strconv.Quote(arg))
	}

	addKeployNetwork(ctx, logger, client)
	err = client.CreateVolume(ctx, "debugfs", true, map[string]string{
		"type":   "debugfs",
		"device": "debugfs",
	})
	if err != nil {
		utils.LogError(logger, err, "failed to debugfs volume")
		return err
	}

	var cmd *exec.Cmd

	// Detect the operating system
	if runtime.GOOS == "windows" {
		aliasParts := strings.Fields(keployAlias)
		if len(aliasParts) == 0 || aliasParts[0] != "docker" {
			return errors.New("invalid keployAlias: must start with 'docker'")
		}

		// Base args from alias (drop leading "docker")
		dockerArgs := append([]string{}, aliasParts[1:]...)

		// Rebuild args so that -c value is one single token wrapped in quotes
		args := []string{}
		for i := 1; i < len(os.Args); i++ {
			if os.Args[i] == "-c" && i+1 < len(os.Args) {
				appCmd := strings.Join(os.Args[i+1:], " ")
				// instead of escaping, literally wrap it with quotes
				args = append(args, "-c", `"`+appCmd+`"`)
				break
			}
			args = append(args, os.Args[i])
		}

		finalArgs := append(dockerArgs, args...)

		// Call docker directly, not via cmd.exe
		cmd = exec.CommandContext(ctx, "docker", finalArgs...)
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

	Volumes := ""
	for _, volume := range DockerConfig.VolumeMounts {
		Volumes = Volumes + " -v " + volume
	}

	switch osName {
	case "linux":
		alias := "sudo docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + Volumes + " -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
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
			alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + Volumes + " -v " + pwd + ":" + dpwd + " -w " + dpwd + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") + "\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img
			return alias, nil
		}
		// if default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + Volumes + " -v " + pwd + ":" + dpwd + " -w " + dpwd + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") + "\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img
		return alias, nil
	case "darwin":
		// Get the context and docker daemon endpoint.
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

		// Construct the alias command based on context-specific `debugfs` mount
		var alias string
		if currentContext == "colima" {

			// To allow docker client to connect to the colima daemon because by default it uses the default docker daemon
			err := os.Setenv("DOCKER_HOST", dockerEndpoint)
			if err != nil {
				utils.LogError(logger, err, "failed to set DOCKER_HOST environment variable for colima context")
				return "", errors.New("failed to get alias")
			}
			logger.Info("Starting keploy in docker with colima context, as that is the current context.")
			alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + Volumes + " -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
			return alias, nil
		}
		// if default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		alias = "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + Volumes + " -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
		return alias, nil
	}
	return "", errors.New("failed to get alias")
}

func addKeployNetwork(ctx context.Context, logger *zap.Logger, client docker.Client) {

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
