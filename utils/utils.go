package utils

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

var WarningSign = "\U000026A0"

func BindFlagsToViper(logger *zap.Logger, cmd *cobra.Command, viperKeyPrefix string) {
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		// Construct the Viper key and the env variable name
		if viperKeyPrefix == "" {
			viperKeyPrefix = cmd.Name()
		}
		viper.SetEnvPrefix("KEPLOY")
		viperKey := viperKeyPrefix + "." + flag.Name
		envVarName := strings.ToUpper(viperKeyPrefix + "_" + flag.Name)
		envVarName = strings.ReplaceAll(envVarName, ".", "_") // Why do we need this?

		// Bind the flag to Viper with the constructed key
		err := viper.BindPFlag(viperKey, flag)
		if err != nil {
			LogError(logger, err, "failed to bind flag to config")
		}

		// Tell Viper to also read this flag's value from the corresponding env variable
		err = viper.BindEnv(viperKey, envVarName)
		if err != nil {
			LogError(logger, err, "failed to bind environment variables to config")
		}
	})
}

//func ModifyToSentryLogger(ctx context.Context, logger *zap.Logger, client *sentry.Client, configDb *configdb.ConfigDb) *zap.Logger {
//	cfg := zapsentry.Configuration{
//		Level:             zapcore.ErrorLevel, //when to send message to sentry
//		EnableBreadcrumbs: true,               // enable sending breadcrumbs to Sentry
//		BreadcrumbLevel:   zapcore.InfoLevel,  // at what level should we sent breadcrumbs to sentry
//		Tags: map[string]string{
//			"component": "system",
//		},
//	}
//
//	core, err := zapsentry.NewCore(cfg, zapsentry.NewSentryClientFromClient(client))
//	//in case of err it will return noop core. So we don't need to attach it to log.
//	if err != nil {
//		logger.Debug("failed to init zap", zap.Error(err))
//		return logger
//	}
//
//	logger = zapsentry.AttachCoreToLogger(core, logger)
//	kernelVersion := ""
//	if runtime.GOOS == "linux" {
//		cmd := exec.CommandContext(ctx, "uname", "-r")
//		kernelBytes, err := cmd.Output()
//		if err != nil {
//			logger.Debug("failed to get kernel version", zap.Error(err))
//		} else {
//			kernelVersion = string(kernelBytes)
//		}
//	}
//
//	arch := runtime.GOARCH
//	installationID, err := configDb.GetInstallationId(ctx)
//	if err != nil {
//		logger.Debug("failed to get installationID", zap.Error(err))
//	}
//	sentry.ConfigureScope(func(scope *sentry.Scope) {
//		scope.SetTag("Keploy Version", Version)
//		scope.SetTag("Linux Kernel Version", kernelVersion)
//		scope.SetTag("Architecture", arch)
//		scope.SetTag("Installation ID", installationID)
//	})
//	return logger
//}

// LogError logs the error with the provided fields if the error is not context.Canceled.
func LogError(logger *zap.Logger, err error, msg string, fields ...zap.Field) {
	if logger == nil {
		fmt.Println("Failed to log error. Logger is nil.")
		return
	}
	if !errors.Is(err, context.Canceled) {
		logger.Error(msg, append(fields, zap.Error(err))...)
	}
}
func DeleteLogs(logger *zap.Logger) {
	//Check if keploy-log.txt exists
	_, err := os.Stat("keploy-logs.txt")
	if os.IsNotExist(err) {
		return
	}
	//If it does, remove it.
	err = os.Remove("keploy-logs.txt")
	if err != nil {
		LogError(logger, err, "Error removing log file")
		return
	}
}

type GitHubRelease struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
}

var ErrGitHubAPIUnresponsive = errors.New("GitHub API is unresponsive")

var Emoji = "\U0001F430" + " Keploy:"
var ConfigGuide = `
# Example on using tests
#tests:
#  filters:
#   - path: "/user/app"
#     urlMethods: ["GET"]
#     headers: {
#       "^asdf*": "^test"
#     }
#     host: "dc.services.visualstudio.com"
#Example on using stubs
#stubs:
#  filters:
#   - path: "/user/app"
#     port: 8080
#   - port: 8081
#   - host: "dc.services.visualstudio.com"
#   - port: 8081
#     host: "dc.services.visualstudio.com"
#     path: "/user/app"
	#
#Example on using globalNoise
#globalNoise:
#   global:
#     body: {
#        # to ignore some values for a field,
#        # pass regex patterns to the corresponding array value
#        "url": ["https?://\S+", "http://\S+"],
#     }
#     header: {
#        # to ignore the entire field, pass an empty array
#        "Date": [],
#      }
#    # to ignore fields or the corresponding values for a specific test-set,
#    # pass the test-set-name as a key to the "test-sets" object and
#    # populate the corresponding "body" and "header" objects
#    test-sets:
#      test-set-1:
#        body: {
#          # ignore all the values for the "url" field
#          "url": []
#        }
#        header: {
#          # we can also pass the exact value to ignore for a field
#          "User-Agent": ["PostmanRuntime/7.34.0"]
#        }
`

// AskForConfirmation asks the user for confirmation. A user must type in "yes" or "no" and
// then press enter. It has fuzzy matching, so "y", "Y", "yes", "YES", and "Yes" all count as
// confirmations. If the input is not recognized, it will ask again. The function does not return
// until it gets a valid response from the user.
func AskForConfirmation(s string) (bool, error) {
	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Printf("%s [y/n]: ", s)

		response, err := reader.ReadString('\n')
		if err != nil {
			return false, err
		}

		response = strings.ToLower(strings.TrimSpace(response))

		if response == "y" || response == "yes" {
			return true, nil
		} else if response == "n" || response == "no" {
			return false, nil
		}
	}
}

func CheckFileExists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

var Version string

func attachLogFileToSentry(logger *zap.Logger, logFilePath string) error {
	file, err := os.Open(logFilePath)
	if err != nil {
		return fmt.Errorf("Error opening log file: %s", err.Error())
	}
	defer func() {
		if err := file.Close(); err != nil {
			LogError(logger, err, "Error closing log file")
		}
	}()

	content, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("Error reading log file: %s", err.Error())
	}

	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetExtra("logfile", string(content))
	})
	sentry.Flush(time.Second * 5)
	return nil
}

// Recover recovers from a panic and logs the stack trace to Sentry.
// It also stops the global context.
func Recover(logger *zap.Logger) {
	if logger == nil {
		fmt.Println(Emoji + "Failed to recover from panic. Logger is nil.")
		return
	}
	sentry.Flush(2 * time.Second)
	if r := recover(); r != nil {
		err := attachLogFileToSentry(logger, "./keploy-logs.txt")
		if err != nil {
			LogError(logger, err, "failed to attach log file to sentry")
		}
		sentry.CaptureException(errors.New(fmt.Sprint(r)))
		// Get the stack trace
		stackTrace := debug.Stack()
		LogError(logger, nil, "Recovered from panic", zap.String("stack trace", string(stackTrace)))
		//stopping the global context
		err = Stop(logger, fmt.Sprintf("Recovered from: %s", r))
		if err != nil {
			LogError(logger, err, "failed to stop the global context")
			//return
		}
		sentry.Flush(time.Second * 2)
	}
}

// GetLatestGitHubRelease fetches the latest version and release body from GitHub releases with a timeout.
func GetLatestGitHubRelease(ctx context.Context, logger *zap.Logger) (GitHubRelease, error) {
	// GitHub repository details
	repoOwner := "keploy"
	repoName := "keploy"

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)

	client := http.Client{
		Timeout: 4 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return GitHubRelease{}, err
	}

	resp, err := client.Do(req)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return GitHubRelease{}, ErrGitHubAPIUnresponsive
		}
		return GitHubRelease{}, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			LogError(logger, err, "failed to close response body")
		}
	}()

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return GitHubRelease{}, err
	}
	return release, nil
}

// FindDockerCmd checks if the cli is related to docker or not, it also returns if it is a docker compose file
func FindDockerCmd(cmd string) CmdType {
	// Convert command to lowercase for case-insensitive comparison
	cmdLower := strings.TrimSpace(strings.ToLower(cmd))

	// Define patterns for Docker and Docker Compose
	dockerPatterns := []string{"docker", "sudo docker"}
	dockerComposePatterns := []string{"docker-compose", "sudo docker-compose", "docker compose", "sudo docker compose"}

	// Check for Docker Compose command patterns and file extensions
	for _, pattern := range dockerComposePatterns {
		if strings.HasPrefix(cmdLower, pattern) {
			return DockerCompose
		}
	}
	// Check for Docker command patterns
	for _, pattern := range dockerPatterns {
		if strings.HasPrefix(cmdLower, pattern) {
			return Docker
		}
	}
	return Native
}

type CmdType string

// CmdType constants
const (
	Docker        CmdType = "docker"
	DockerCompose CmdType = "docker-compose"
	Native        CmdType = "native"
)

type RecordFlags struct {
	Path             string
	Command          string
	ContainerName    string
	Proxyport        uint32
	NetworkName      string
	Delay            uint64
	BuildDelay       time.Duration
	PassThroughPorts []uint
	ConfigPath       string
	EnableTele       bool
}

type TestFlags struct {
	Path               string
	Proxyport          uint32
	Command            string
	Testsets           []string
	ContainerName      string
	NetworkName        string
	Delay              uint64
	BuildDelay         time.Duration
	APITimeout         uint64
	PassThroughPorts   []uint
	ConfigPath         string
	MongoPassword      string
	CoverageReportPath string
	EnableTele         bool
	WithCoverage       bool
}

func getAlias(ctx context.Context, logger *zap.Logger) (string, error) {
	// Get the name of the operating system.
	osName := runtime.GOOS
	//TODO: configure the hardcoded port mapping & check if (/.keploy-config:/root/.keploy-config) can be removed from all the aliases
	switch osName {
	case "linux":
		alias := "sudo docker run --pull always --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host -it -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy "
		return alias, nil
	case "darwin":
		cmd := exec.CommandContext(ctx, "docker", "context", "ls", "--format", "{{.Name}}\t{{.Current}}")
		out, err := cmd.Output()
		if err != nil {
			LogError(logger, err, "failed to get the current docker context")
			return "", errors.New("failed to get alias")
		}
		dockerContext := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
		if len(dockerContext) == 0 {
			LogError(logger, nil, "failed to get the current docker context")
			return "", errors.New("failed to get alias")
		}
		dockerContext = strings.Split(dockerContext, "\n")[0]
		if dockerContext == "colima" {
			logger.Info("Starting keploy in docker with colima context, as that is the current context.")
			alias := "docker run --pull always --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host -it -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy "
			return alias, nil
		} else {
			logger.Info("Starting keploy in docker with default context, as that is the current context.")
			alias := "docker run --pull always --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host -it -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy "
			return alias, nil
		}
	case "Windows":
		LogError(logger, nil, "Windows is not supported. Use WSL2 instead.")
		return "", errors.New("failed to get alias")
	}
	return "", errors.New("failed to get alias")
}

//func appendFlags(flagName string, flagValue string) string {
//	if len(flagValue) > 0 {
//		// Check for = in the flagName.
//		if strings.Contains(flagName, "=") {
//			return " --" + flagName + flagValue
//		}
//		return " --" + flagName + " " + flagValue
//	}
//	return ""
//}

func RunInDocker(ctx context.Context, logger *zap.Logger, command string) error {
	//Get the correct keploy alias.
	keployAlias, err := getAlias(ctx, logger)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", keployAlias+" "+command)
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	logger.Debug("This is the keploy alias", zap.String("keployAlias:", keployAlias))
	err = cmd.Run()
	if err != nil {
		LogError(logger, err, "failed to start keploy in docker")
		return err
	}
	return nil
}

// Keys returns an array containing the keys of the given map.
func Keys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func SentryInit(logger *zap.Logger, dsn string) {
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		TracesSampleRate: 1.0,
	})
	if err != nil {
		logger.Debug("Could not initialise sentry.", zap.Error(err))
	}
}

func FetchHomeDirectory(isNewConfigPath bool) string {
	var configFolder = "/.keploy-config"

	if isNewConfigPath {
		configFolder = "/.keploy"
	}
	if runtime.GOOS == "windows" {
		home := os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		return home + configFolder
	}

	return os.Getenv("HOME") + configFolder
}

// InterruptProcessTree interrupts an entire process tree using the given signal
func InterruptProcessTree(cmd *exec.Cmd, logger *zap.Logger, ppid int, sig syscall.Signal) error {
	// Find all descendant PIDs of the given PID & then signal them.
	// Any shell doesn't signal its children when it receives a signal.
	// Children may have their own process groups, so we need to signal them separately.
	children, err := findChildPIDs(ppid)
	if err != nil {
		return err
	}

	children = append(children, ppid)

	for _, pid := range children {
		if cmd.ProcessState == nil {
			err := syscall.Kill(pid, sig)
			if err != nil {
				logger.Error("failed to send signal to process", zap.Int("pid", pid), zap.Error(err))
			}
		}
	}
	return nil
}

// findChildPIDs takes a parent PID and returns a slice of all descendant PIDs.
func findChildPIDs(parentPID int) ([]int, error) {
	var childPIDs []int

	// Recursive helper function to find all descendants of a given PID.
	var findDescendants func(int)
	findDescendants = func(pid int) {
		procDirs, err := os.ReadDir("/proc")
		if err != nil {
			return
		}

		for _, procDir := range procDirs {
			if !procDir.IsDir() {
				continue
			}

			childPid, err := strconv.Atoi(procDir.Name())
			if err != nil {
				continue
			}

			statusPath := filepath.Join("/proc", procDir.Name(), "status")
			statusBytes, err := os.ReadFile(statusPath)
			if err != nil {
				continue
			}

			status := string(statusBytes)
			for _, line := range strings.Split(status, "\n") {
				if strings.HasPrefix(line, "PPid:") {
					fields := strings.Fields(line)
					if len(fields) == 2 {
						ppid, err := strconv.Atoi(fields[1])
						if err != nil {
							break
						}
						if ppid == pid {
							childPIDs = append(childPIDs, childPid)
							findDescendants(childPid)
						}
					}
					break
				}
			}
		}
	}

	// Start the recursion with the initial parent PID.
	findDescendants(parentPID)

	return childPIDs, nil
}
