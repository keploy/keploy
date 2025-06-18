package utils

import (
	"bufio"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/getsentry/sentry-go"
	netLib "github.com/shirou/gopsutil/v3/net"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

var WarningSign = "\U000026A0"

var TemplatizedValues = map[string]interface{}{}

var ErrCode = 0

func ReplaceHost(currentURL string, ipAddress string) (string, error) {
	// Parse the current URL
	parsedURL, err := url.Parse(currentURL)

	if err != nil {
		// Return the original URL if parsing fails
		return currentURL, err
	}

	if ipAddress == "" {
		return currentURL, fmt.Errorf("failed to replace url in case of docker env")
	}

	// Replace hostname with the IP address
	parsedURL.Host = strings.Replace(parsedURL.Host, parsedURL.Hostname(), ipAddress, 1)
	// Return the modified URL
	return parsedURL.String(), nil
}

func ReplaceGrpcHost(authority string, ipAddress string) (string, error) {
	// Check if ipAddress is empty
	if ipAddress == "" {
		return authority, fmt.Errorf("failed to replace authority in case of docker env: empty IP address")
	}

	// Split authority into host and port
	parts := strings.Split(authority, ":")
	if len(parts) != 2 {
		return authority, fmt.Errorf("invalid authority format, expected host:port but got %s", authority)
	}

	// Replace the host part with ipAddress, keeping the port
	return ipAddress + ":" + parts[1], nil
}

func ReplaceGrpcPort(authority string, port string) (string, error) {
	// Check if port is empty
	if port == "" {
		return authority, fmt.Errorf("failed to replace port in case of docker env: empty port")
	}

	// Split authority into host and port
	parts := strings.Split(authority, ":")
	if len(parts) == 0 {
		return authority, fmt.Errorf("invalid authority format, got empty string")
	}

	// If there's no port in the authority, append the new port
	if len(parts) == 1 {
		return parts[0] + ":" + port, nil
	}

	// Replace the port part, keeping the host
	return parts[0] + ":" + port, nil
}

// ReplaceBaseURL replaces the base URL (scheme + host) of the given URL with the provided baseURL.
// It returns the updated URL as a string or an error if the operation fails.
func ReplaceBaseURL(currentURL string, baseURL string) (string, error) {
	// Parse the current URL
	parsedURL, err := url.Parse(currentURL)
	if err != nil {
		return currentURL, err
	}

	// Check if baseURL is valid
	if baseURL == "" {
		return currentURL, fmt.Errorf("failed to replace baseURL: baseURL is empty")
	}

	// Parse the new baseURL
	newBaseURL, err := url.Parse(baseURL)
	if err != nil {
		return currentURL, fmt.Errorf("invalid baseURL: %w", err)
	}

	// Replace the scheme and host
	parsedURL.Scheme = newBaseURL.Scheme
	parsedURL.Host = newBaseURL.Host

	// Return the updated URL as a string
	return parsedURL.String(), nil
}

func ReplacePort(currentURL string, port string) (string, error) {
	if port == "" {
		return currentURL, fmt.Errorf("failed to replace port in case of docker env")
	}

	parsedURL, err := url.Parse(currentURL)

	if err != nil {
		return currentURL, err
	}

	if parsedURL.Port() == "" {
		parsedURL.Host = parsedURL.Host + ":" + port
	} else {
		parsedURL.Host = strings.Replace(parsedURL.Host, parsedURL.Port(), port, 1)
	}

	return parsedURL.String(), nil
}

func kebabToCamel(s string) string {
	parts := strings.Split(s, "-")
	for i := 1; i < len(parts); i++ {
		parts[i] = cases.Title(language.English).String(parts[i])
	}
	return strings.Join(parts, "")
}

func BindFlagsToViper(logger *zap.Logger, cmd *cobra.Command, viperKeyPrefix string) error {
	var bindErr error
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		camelCaseName := kebabToCamel(flag.Name)
		err := viper.BindPFlag(camelCaseName, flag)
		if err != nil {
			LogError(logger, err, "failed to bind flag Name to flag")
			bindErr = err
		}
		// Construct the Viper key and the env variable name
		if viperKeyPrefix == "" {
			viperKeyPrefix = cmd.Name()
		}
		viperKey := viperKeyPrefix + "." + camelCaseName
		envVarName := strings.ToUpper(viperKeyPrefix + "_" + camelCaseName)
		envVarName = strings.ReplaceAll(envVarName, ".", "_") // Why do we need this?

		// Bind the flag to Viper with the constructed key
		err = viper.BindPFlag(viperKey, flag)
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

// RemoveDoubleQuotes removes all double quotes from the values in the provided template map.
// This function handles cases where the templating engine fails to parse values containing both single and double quotes.
// For example:
// Input: '"Not/A)Brand";v="8", "Chromium";v="126", "Brave";v="126"'
// Output: Not/A)Brand;v=8, Chromium;v=126, Brave;v=126
func RemoveDoubleQuotes(tempMap map[string]interface{}) {
	// Remove double quotes
	for key, val := range tempMap {
		if str, ok := val.(string); ok {
			tempMap[key] = strings.ReplaceAll(str, `"`, "")
		}
	}
}

func DeleteFileIfNotExists(logger *zap.Logger, name string) (err error) {
	//Check if file exists
	_, err = os.Stat(name)
	if os.IsNotExist(err) {
		return nil
	}
	//If it does, remove it.
	err = os.Remove(name)
	if err != nil {
		LogError(logger, err, "Error removing file")
		return err
	}

	return nil
}

type GitHubRelease struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
}

var ErrGitHubAPIUnresponsive = errors.New("GitHub API is unresponsive")

var Emoji = "\U0001F430" + " Keploy:"
var ConfigGuide = `
# Visit [https://keploy.io/docs/running-keploy/configuration-file/] to learn about using keploy through configration file.
`

// AskForConfirmation asks the user for confirmation. A user must type in "yes" or "no" and
// then press enter. It has fuzzy matching, so "y", "Y", "yes", "YES", and "Yes" all count as
// confirmations. If the input is not recognized, it will ask again. The function does not return
// until it gets a valid response from the user.
func AskForConfirmation(s string) (bool, error) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("%s [y/n]: ", s)
	response, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	response = strings.ToLower(strings.TrimSpace(response))
	switch response {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		fmt.Println("Invalid input. Exiting...")
		return false, errors.New("invalid confirmation input")
	}
}
func CheckFileExists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

var Version string
var VersionIdenitfier string

func GetVersionAsComment() string {
	return fmt.Sprintf("# Generated by Keploy (%s)\n", Version)
}

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
        uses: actions/checkout@v4
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
	if cmd == "" {
		return Empty
	}
	// Convert command to lowercase for case-insensitive comparison
	cmdLower := strings.TrimSpace(strings.ToLower(cmd))

	// Define patterns for Docker and Docker Compose
	dockerRunPatterns := []string{"docker run", "sudo docker run", "docker container run", "sudo docker container run"}
	dockerStartPatterns := []string{"docker start", "sudo docker start", "docker container start", "sudo docker container start"}
	dockerComposePatterns := []string{"docker-compose", "sudo docker-compose", "docker compose", "sudo docker compose"}

	// Check for Docker Compose command patterns and file extensions
	for _, pattern := range dockerComposePatterns {
		if strings.HasPrefix(cmdLower, pattern) {
			return DockerCompose
		}
	}
	// Check for Docker start command patterns
	for _, pattern := range dockerStartPatterns {
		if strings.HasPrefix(cmdLower, pattern) {
			return DockerStart
		}
	}
	// Check for Docker run command patterns
	for _, pattern := range dockerRunPatterns {
		if strings.HasPrefix(cmdLower, pattern) {
			return DockerRun
		}
	}
	return Native
}

type CmdType string

// CmdType constants
const (
	DockerRun     CmdType = "docker-run"
	DockerStart   CmdType = "docker-start"
	DockerCompose CmdType = "docker-compose"
	Native        CmdType = "native"
	Empty         CmdType = ""
)

func ToInt(value interface{}) int {
	switch v := value.(type) {
	case int:
		return v
	case string:
		i, err := strconv.Atoi(v)
		if err != nil {
			fmt.Printf("failed to convert string to int: %v", err)
			return 0
		}
		return i
	case float64:
		return int(v)

	}
	return 0
}

// ToString remove all types of value to strings for comparison.
func ToString(val interface{}) string {
	switch v := val.(type) {
	case int:
		return strconv.Itoa(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case int64:
		return strconv.FormatInt(v, 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case string:
		return v
	}
	return ""
}

func ToFloat(value interface{}) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			fmt.Printf("failed to convert string to float: %v", err)
			return 0
		}
		return f
	case int:
		return float64(v)
	}
	return 0
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

func ToAbsPath(logger *zap.Logger, originalPath string) string {
	path := originalPath
	//if user provides relative path
	if len(path) > 0 && path[0] != '/' {
		absPath, err := filepath.Abs(path)
		if err != nil {
			LogError(logger, err, "failed to get the absolute path from relative path")
		}
		path = absPath
	} else if len(path) == 0 { // if user doesn't provide any path
		cdirPath, err := os.Getwd()
		if err != nil {
			LogError(logger, err, "failed to get the path of current directory")
		}
		path = cdirPath
	}
	path += "/keploy"
	return path
}

// makeDirectory creates a directory if not exists with all user access
func makeDirectory(path string) error {
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

type ErrType string

// ErrType constants to get the type of error, during init or runtime
const (
	Init    ErrType = "init"
	Runtime ErrType = "runtime"
)

type CmdError struct {
	Type ErrType
	Err  error
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
		err := SendSignal(logger, -pid, sig)
		if err != nil {
			logger.Error("error sending signal to the process group id", zap.Int("pgid", pid), zap.Error(err))
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

func isGoBinary(logger *zap.Logger, filePath string) bool {
	f, err := elf.Open(filePath)
	if err != nil {
		logger.Debug(fmt.Sprintf("failed to open file %s", filePath), zap.Error(err))
		return false
	}
	if err := f.Close(); err != nil {
		LogError(logger, err, "failed to close file", zap.String("file", filePath))
	}

	// Check for section names typical to Go binaries
	sections := []string{".go.buildinfo", ".gopclntab"}
	for _, section := range sections {
		if sect := f.Section(section); sect != nil {
			fmt.Println(section)
			return true
		}
	}
	return false
}

// DetectLanguage detects the language of the test command and returns the executable
func DetectLanguage(logger *zap.Logger, cmd string) (config.Language, string) {
	if cmd == "" {
		return models.Unknown, ""
	}
	fields := strings.Fields(cmd)
	executable := fields[0]
	if strings.HasPrefix(cmd, "python") {
		return models.Python, executable
	}

	if executable == "node" || executable == "npm" || executable == "yarn" {
		return models.Javascript, executable
	}

	if executable == "java" {
		return models.Java, executable
	}

	if executable == "go" || (len(fields) == 1 && isGoBinary(logger, executable)) {
		return models.Go, executable
	}
	return models.Unknown, executable
}

// FileExists checks if a file exists and is not a directory at the given path.
func FileExists(path string) (bool, error) {
	fileInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return fileInfo.Mode().IsRegular(), nil
}

// ExpandPath expands a given path, replacing the tilde with the user's home directory
func ExpandPath(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		homeDir, err := getHomeDir()
		if err != nil {
			return "", err
		}
		return strings.Replace(path, "~", homeDir, 1), nil
	}
	return path, nil
}

// getHomeDir retrieves the appropriate home directory based on the execution context
func getHomeDir() (string, error) {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if usr, err := user.Lookup(sudoUser); err == nil {
			return usr.HomeDir, nil
		}
	}
	// Fallback to the current user's home directory
	if usr, err := user.Current(); err == nil {
		return usr.HomeDir, nil
	}
	// Fallback if neither method works
	return "", errors.New("failed to retrieve current user info")
}

func IsDockerCmd(kind CmdType) bool {
	return (kind == DockerRun || kind == DockerStart || kind == DockerCompose)
}

func AddToGitIgnore(logger *zap.Logger, path string, ignoreString string) error {
	gitignorePath := path + "/.gitignore"

	file, err := os.OpenFile(gitignorePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("error opening or creating .gitignore file: %v", err)
	}

	defer func() {
		if err := file.Close(); err != nil {
			logger.Error("error closing .gitignore file: %v", zap.Error(err))
		}
	}()

	scanner := bufio.NewScanner(file)
	found := false
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == ignoreString {
			found = true
			break
		}
	}

	if !found {
		if _, err := file.WriteString("\n" + ignoreString + "\n"); err != nil {
			return fmt.Errorf("error writing to .gitignore file: %v", err)
		}
		return nil
	}

	return nil
}

func Hash(data []byte) string {
	hasher := sha256.New()
	hasher.Write(data)
	return hex.EncodeToString(hasher.Sum(nil))
}

func GetLastDirectory() (string, error) {
	// Get the current working directory
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	// Extract the base (last directory)
	lastDir := filepath.Base(dir)
	return lastDir, nil
}

func IsFileEmpty(filePath string) (bool, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return false, err
	}
	return fileInfo.Size() == 0, nil
}
func IsXMLResponse(resp *models.HTTPResp) bool {
	if resp == nil || resp.Header == nil {
		return false
	}

	contentType, exists := resp.Header["Content-Type"]
	if !exists || contentType == "" {
		return false
	}
	return strings.Contains(contentType, "application/xml") || strings.Contains(contentType, "text/xml")
}

// // XMLToMap converts an XML string into a map[string]interface{}
// func XMLToMap(data string) (map[string]any, error) {
// 	mv, err := mxj.NewMapXml([]byte(data))
// 	if err != nil {
// 		return nil, err
// 	}
// 	return mv, nil
// }

// // MapToXML converts a map[string]interface{} into an XML string
// func MapToXML(data map[string]any) (string, error) {
// 	mv := mxj.Map(data)
// 	xmlBytes, err := mv.Xml()
// 	if err != nil {
// 		return "", err
// 	}
// 	return string(xmlBytes), nil
// }
