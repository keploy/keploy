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
	netLib "github.com/shirou/gopsutil/v3/net"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"golang.org/x/term"
)

var WarningSign = "\U000026A0"

func BindFlagsToViper(logger *zap.Logger, cmd *cobra.Command, viperKeyPrefix string) error {
	var bindErr error
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		// Construct the Viper key and the env variable name
		if viperKeyPrefix == "" {
			viperKeyPrefix = cmd.Name()
		}
		viperKey := viperKeyPrefix + "." + flag.Name
		envVarName := strings.ToUpper(viperKeyPrefix + "_" + flag.Name)
		envVarName = strings.ReplaceAll(envVarName, ".", "_") // Why do we need this?

		// Bind the flag to Viper with the constructed key
		err := viper.BindPFlag(viperKey, flag)
		if err != nil {
			LogError(logger, err, "failed to bind flag to config")
			bindErr = err
		}

		// Tell Viper to also read this flag's value from the corresponding env variable
		err = viper.BindEnv(viperKey, envVarName)
		logger.Debug("Binding flag to viper", zap.String("viperKey", viperKey), zap.String("envVarName", envVarName))
		if err != nil {
			LogError(logger, err, "failed to bind environment variables to config")
			bindErr = err
		}
	})
	return bindErr
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
		return fmt.Errorf("error opening log file: %s", err.Error())
	}
	defer func() {
		if err := file.Close(); err != nil {
			LogError(logger, err, "Error closing log file")
		}
	}()

	content, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("error reading log file: %s", err.Error())
	}

	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetExtra("logfile", string(content))
	})
	sentry.Flush(time.Second * 5)
	return nil
}

// HandleRecovery handles the common logic for recovering from a panic.
func HandleRecovery(logger *zap.Logger, r interface{}, errMsg string) {
	err := attachLogFileToSentry(logger, "./keploy-logs.txt")
	if err != nil {
		LogError(logger, err, "failed to attach log file to sentry")
	}
	sentry.CaptureException(errors.New(fmt.Sprint(r)))
	// Get the stack trace
	stackTrace := debug.Stack()
	LogError(logger, nil, errMsg, zap.String("stack trace", string(stackTrace)))
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
		HandleRecovery(logger, r, "Recovered from panic")
		err := Stop(logger, fmt.Sprintf("Recovered from: %s", r))
		if err != nil {
			LogError(logger, err, "failed to stop the global context")
		}
		sentry.Flush(2 * time.Second)
	}
}

// GenerateGithubActions generates a GitHub Actions workflow file for Keploy
func GenerateGithubActions(logger *zap.Logger, appCmd string) {
	// Determine the path based on the alias "keploy"
	logger.Debug("Generating GitHub Actions workflow file")
	// Define the content of the GitHub Actions workflow file
	actionsFileContent := `name: Keploy
on:
  push:
    branches:
      - main
  pull_request:
    types: [opened, reopened, synchronize]
jobs:
  e2e-test:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@v2
      - name: Test-Report
        uses: keploy/testgpt@main
        with:
          working-directory: ./
          keploy-path: ./
          command: ` + appCmd + `
`

	// Define the file path where the GitHub Actions workflow file will be saved
	filePath := "/githubactions/keploy.yml"

	//create the file path
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		logger.Error("Error creating directory for GitHub Actions workflow file", zap.Error(err))
		return
	}
	// Write the content to the file
	if err := os.WriteFile(filePath, []byte(actionsFileContent), 0644); err != nil {
		logger.Error("Error writing GitHub Actions workflow file", zap.Error(err))
		return
	}

	logger.Info("GitHub Actions workflow file generated successfully", zap.String("path", filePath))
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
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
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

func getAlias(ctx context.Context, logger *zap.Logger) (string, error) {
	// Get the name of the operating system.
	osName := runtime.GOOS
	//TODO: configure the hardcoded port mapping
	img := "ghcr.io/keploy/keploy:" + "v" + Version
	logger.Info("Starting keploy in docker with image", zap.String("image:", img))

	var ttyFlag string

	if term.IsTerminal(int(os.Stdin.Fd())) {
		ttyFlag = " -it "
	} else {
		ttyFlag = ""
	}

	switch osName {
	case "linux":
		alias := "sudo docker container run --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + " -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
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
			alias := "docker container run --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
			return alias, nil
		}
		// if default docker context is used
		logger.Info("Starting keploy in docker with default context, as that is the current context.")
		alias := "docker container run --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host" + ttyFlag + "-v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm " + img
		return alias, nil
	case "Windows":
		LogError(logger, nil, "Windows is not supported. Use WSL2 instead.")
		return "", errors.New("failed to get alias")
	}
	return "", errors.New("failed to get alias")
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

	cmd := exec.CommandContext(
		ctx,
		"sh",
		"-c",
		keployAlias+" "+strings.Join(quotedArgs, " "),
	)

	cmd.Cancel = func() error {
		return InterruptProcessTree(logger, cmd.Process.Pid, syscall.SIGINT)
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

//func FetchHomeDirectory(isNewConfigPath bool) string {
//	var configFolder = "/.keploy-config"
//
//	if isNewConfigPath {
//		configFolder = "/.keploy"
//	}
//	if runtime.GOOS == "windows" {
//		home := os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
//		if home == "" {
//			home = os.Getenv("USERPROFILE")
//		}
//		return home + configFolder
//	}
//
//	return os.Getenv("HOME") + configFolder
//}

func GetAbsPath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return absPath, nil
}

// makeDirectory creates a directory if not exists with all user access
func makeDirectory(path string) error {
	oldUmask := syscall.Umask(0)
	defer syscall.Umask(oldUmask)
	err := os.MkdirAll(path, 0777)
	if err != nil {
		return err
	}
	return nil
}

// SetCoveragePath takes a goCovPath and sets the coverage path accordingly.
// It returns an error if the path is a file or if the path does not exist.
func SetCoveragePath(logger *zap.Logger, goCovPath string) (string, error) {
	if goCovPath == "" {
		// Calculate the current path and create a coverage-reports directory
		currentPath, err := GetAbsPath("")
		if err != nil {
			LogError(logger, err, "failed to get the current working directory")
			return "", err
		}
		goCovPath = currentPath + "/coverage-reports"
		if err := makeDirectory(goCovPath); err != nil {
			LogError(logger, err, "failed to create coverage-reports directory", zap.String("CoverageReportPath", goCovPath))
			return "", err
		}
		return goCovPath, nil
	}

	goCovPath, err := GetAbsPath(goCovPath)
	if err != nil {
		LogError(logger, err, "failed to get the absolute path for the coverage report path", zap.String("CoverageReportPath", goCovPath))
		return "", err
	}
	// Check if the path is a directory
	dirInfo, err := os.Stat(goCovPath)
	if err != nil {
		if os.IsNotExist(err) {
			LogError(logger, err, "the provided path does not exist", zap.String("CoverageReportPath", goCovPath))
			return "", err
		}
		LogError(logger, err, "failed to check the coverage report path", zap.String("CoverageReportPath", goCovPath))
		return "", err
	}
	if !dirInfo.IsDir() {
		msg := "the coverage report path is not a directory. Please provide a valid path to a directory for go coverage reports"

		LogError(logger, nil, msg, zap.String("CoverageReportPath", goCovPath))
		return "", errors.New("the path provided is not a directory")
	}

	return goCovPath, nil
}

// InterruptProcessTree interrupts an entire process tree using the given signal
func InterruptProcessTree(logger *zap.Logger, ppid int, sig syscall.Signal) error {
	// Find all descendant PIDs of the given PID & then signal them.
	// Any shell doesn't signal its children when it receives a signal.
	// Children may have their own process groups, so we need to signal them separately.
	children, err := findChildPIDs(ppid)
	if err != nil {
		return err
	}

	children = append(children, ppid)
	uniqueProcess, err := uniqueProcessGroups(children)
	if err != nil {
		logger.Error("failed to find unique process groups", zap.Int("pid", ppid), zap.Error(err))
		uniqueProcess = children
	}

	for _, pid := range uniqueProcess {
		err := syscall.Kill(-pid, sig)
		// ignore the ESRCH error as it means the process is already dead
		if errno, ok := err.(syscall.Errno); ok && err != nil && errno != syscall.ESRCH {
			logger.Error("failed to send signal to process", zap.Int("pid", pid), zap.Error(err))
		}
	}
	return nil
}

func uniqueProcessGroups(pids []int) ([]int, error) {
	uniqueGroups := make(map[int]bool)
	var uniqueGPIDs []int

	for _, pid := range pids {
		pgid, err := getProcessGroupID(pid)
		if err != nil {
			return nil, err
		}
		if !uniqueGroups[pgid] {
			uniqueGroups[pgid] = true
			uniqueGPIDs = append(uniqueGPIDs, pgid)
		}
	}

	return uniqueGPIDs, nil
}

func getProcessGroupID(pid int) (int, error) {
	statusPath := filepath.Join("/proc", strconv.Itoa(pid), "status")
	statusBytes, err := os.ReadFile(statusPath)
	if err != nil {
		return 0, err
	}

	status := string(statusBytes)
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "NSpgid:") {
			return extractIDFromStatusLine(line), nil
		}
	}

	return 0, nil
}

// extractIDFromStatusLine extracts the ID from a status line in the format "Key:\tValue".
func extractIDFromStatusLine(line string) int {
	fields := strings.Fields(line)
	if len(fields) == 2 {
		id, err := strconv.Atoi(fields[1])
		if err == nil {
			return id
		}
	}
	return -1
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

func GetPIDFromPort(_ context.Context, logger *zap.Logger, port int) (uint32, error) {
	logger.Debug("Getting pid using port", zap.Int("port", port))

	connections, err := netLib.Connections("inet")
	if err != nil {
		return 0, err
	}

	for _, conn := range connections {
		if conn.Status == "LISTEN" && conn.Laddr.Port == uint32(port) {
			if conn.Pid > 0 {
				return uint32(conn.Pid), nil
			}
			return 0, fmt.Errorf("pid %d is out of bounds", conn.Pid)
		}
	}

	// If we get here, no process was found using the given port
	return 0, fmt.Errorf("no process found using port %d", port)
}

func EnsureRmBeforeName(cmd string) string {
	parts := strings.Split(cmd, " ")
	rmIndex := -1
	nameIndex := -1

	for i, part := range parts {
		if part == "--rm" {
			rmIndex = i
		} else if part == "--name" {
			nameIndex = i
			break // Assuming --name will always have an argument, we can break here
		}
	}
	if rmIndex == -1 && nameIndex != -1 {
		parts = append(parts[:nameIndex], append([]string{"--rm"}, parts[nameIndex:]...)...)
	}

	return strings.Join(parts, " ")
}
