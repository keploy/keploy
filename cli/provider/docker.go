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

func StartInDocker(ctx context.Context, logger *zap.Logger, conf *config.Config) error {
    if DockerConfig.Envs == nil {
        DockerConfig.Envs = map[string]string{
            "INSTALLATION_ID": conf.InstallationID,
        }
    } else {
        DockerConfig.Envs["INSTALLATION_ID"] = conf.InstallationID
    }

    cmdType := utils.FindDockerCmd(conf.Command)
    if conf.InDocker || !(utils.IsDockerCmd(cmdType)) {
        return nil
    }

    err := RunInDocker(ctx, logger)
    if err != nil {
        utils.LogError(logger, err, "failed to run the command in docker")
        return err
    }

    logger.Info("exiting the current process as the command is moved to docker")
    os.Exit(0)
    return nil
}

func RunInDocker(ctx context.Context, logger *zap.Logger) error {
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
        utils.LogError(logger, err, "failed to initialize docker")
        return err
    }
    addKeployNetwork(ctx, logger, client)
    err = client.CreateVolume(ctx, "debugfs", true)
    if err != nil {
        utils.LogError(logger, err, "failed to debugfs volume")
        return err
    }

    var cmd *exec.Cmd

    if runtime.GOOS == "windows" {
        var args []string
        args = append(args, "/C")
        args = append(args, strings.Split(keployAlias, " ")...)
        args = append(args, os.Args[1:]...)
        cmd = exec.CommandContext(
            ctx,
            "cmd.exe",
            args...,
        )
    } else {
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
    osName := runtime.GOOS
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
        logger.Info("Starting keploy in docker with default context, as that is the current context.")
        alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + "-v " + pwd + ":" + dpwd + " -w " + dpwd + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("USERPROFILE") + "\\.keploy-config:/root/.keploy-config -v " + os.Getenv("USERPROFILE") + "\\.keploy:/root/.keploy --rm " + img
        return alias, nil
    case "darwin":
        cmd := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{if .Metadata}}Name={{.Name}} {{end}}{{if .Endpoints.docker}}Endpoint={{.Endpoints.docker.Host}}{{end}}")
        out, err := cmd.Output()
        if err != nil {
            utils.LogError(logger, err, "failed to inspect the docker context")
            return "", errors.New("failed to get alias")
        }

        output := strings.TrimSpace(string(out))
        var currentContext, dockerEndpoint string

        for _, part := range strings.Fields(output) {
            if strings.HasPrefix(part, "Name=") {
                currentContext = strings.TrimPrefix(part, "Name=")
            } else if strings.HasPrefix(part, "Endpoint=") {
                dockerEndpoint = strings.TrimPrefix(part, "Endpoint=")
            }
        }

        if currentContext == "" {
            utils.LogError(logger, nil, "failed to find the current docker context")
            return "", errors.New("failed to get alias")
        }

        var alias string
        if currentContext == "colima" {
            err := os.Setenv("DOCKER_HOST", dockerEndpoint)
            if err != nil {
                utils.LogError(logger, err, "failed to set DOCKER_HOST environment variable for colima context")
                return "", errors.New("failed to get alias")
            }
            logger.Info("Starting keploy in docker with colima context, as that is the current context.")
            alias := "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
            return alias, nil
        }
        logger.Info("Starting keploy in docker with default context, as that is the current context.")
        alias = "docker container run --name keploy-v2 " + envs + "-e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
        return alias, nil
    }
    return "", errors.New("failed to get alias")
}

func addKeployNetwork(ctx context.Context, logger *zap.Logger, client docker.Client) {
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
    unixPath := strings.Replace(path, "\\", "/", -1)
    if len(unixPath) > 1 && unixPath[1] == ':' {
        unixPath = unixPath[2:]
    }
    return unixPath
}
