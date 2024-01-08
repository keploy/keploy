package utils

import (
	"bufio"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/log"
	sentry "github.com/getsentry/sentry-go"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

// askForConfirmation asks the user for confirmation. A user must type in "yes" or "no" and
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

var KeployVersion string

func attachLogFileToSentry(logFilePath string) {
	file, err := os.Open(logFilePath)
	if err != nil {
		errors.New(fmt.Sprintf("Error opening log file: %s", err.Error()))
		return
	}
	defer file.Close()

	content, _ := ioutil.ReadAll(file)

	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetExtra("logfile", string(content))
	})
	sentry.Flush(time.Second * 5)
}

func HandlePanic() {
	if r := recover(); r != nil {
		attachLogFileToSentry("./keploy-logs.txt")
		sentry.CaptureException(errors.New(fmt.Sprint(r)))
		// Get the stack trace
		stackTrace := debug.Stack()

		log.Error(Emoji+"Recovered from:", r, "\nstack trace:\n", string(stackTrace))
		sentry.Flush(time.Second * 2)
	}
}

// It checks if the cmd is related to docker or not, it also returns if its a docker compose file
func IsDockerRelatedCmd(cmd string) (bool, string) {
	// Check for Docker command patterns
	dockerCommandPatterns := []string{
		"docker-compose ",
		"sudo docker-compose ",
		"docker compose ",
		"sudo docker compose ",
		"docker ",
		"sudo docker ",
	}

	for _, pattern := range dockerCommandPatterns {
		if strings.HasPrefix(strings.ToLower(cmd), pattern) {
			if strings.Contains(pattern, "compose") {
				return true, "docker-compose"
			}
			return true, "docker"
		}
	}

	// Check for Docker Compose file extension
	dockerComposeFileExtensions := []string{".yaml", ".yml"}
	for _, extension := range dockerComposeFileExtensions {
		if strings.HasSuffix(strings.ToLower(cmd), extension) {
			return true, "docker-compose"
		}
	}

	return false, ""
}

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
	ApiTimeout         uint64
	PassThroughPorts   []uint
	ConfigPath         string
	MongoPassword      string
	CoverageReportPath string
	EnableTele         bool
	WithCoverage       bool
}

func getAlias(keployAlias *string, logger *zap.Logger) {
	// Get the name of the operating system.
	osName := runtime.GOOS
	if osName == "Windows" {
		logger.Error("Windows is not supported. Use WSL2 instead.")
		return
	}
	if osName == "darwin" {
		//Get the current docker context.
		cmd := exec.Command("docker", "context", "ls", "--format", "{{.Name}}")
		out, err := cmd.Output()
		if err != nil {
			logger.Error("Failed to get the current docker context", zap.Error(err))
			return
		}
		dockerContext := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
		dockerContext = strings.Split(dockerContext, "\n")[0]
		if choice == "colima" {
			if dockerContext == "default" {
				logger.Error("Error: Docker is using the default context, set to colima using 'docker context use colima'")
				return
			}
			*keployAlias = "sudo docker run --pull always  --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host -it -v " + os.Getenv("PWD") + ":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy "
		} else if choice == "docker" {
			if dockerContext == "colima" {
				logger.Error("Error: Docker is using the colima context, set to default using 'docker context use default'")
				return
			}
			*keployAlias = "sudo docker run --pull always --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host -it -v " + os.Getenv("PWD") + ":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy "
		} else {
			logger.Error("Please enter one of the two options provided.")
			return
		}
	} else if osName == "linux" {
		*keployAlias = "sudo docker run --pull always --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host -it -v " + os.Getenv("PWD") + ":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy "
	}
}

func UpdateKeployToDocker(cmdName string, isDockerCompose bool, flags interface{}, logger *zap.Logger) {
	var recordFlags RecordFlags
	var testFlags TestFlags
	//Check the type of flags.
	switch flags.(type) {
	case RecordFlags:
		recordFlags = flags.(RecordFlags)
	case TestFlags:
		testFlags = flags.(TestFlags)
	default:
		logger.Error("Unknown flags provided")
		return
	}
	var keployAlias string
	getAlias(&keployAlias, logger)
	keployAlias = keployAlias + cmdName + " -c "
	var cmd *exec.Cmd
	if cmdName == "record" {
		keployAlias = keployAlias + "\"" + recordFlags.Command + "\" "
		if len(recordFlags.PassThroughPorts) > 0 {
			portSlice := make([]string, len(recordFlags.PassThroughPorts))
			for i, port := range recordFlags.PassThroughPorts {
				portSlice[i] = fmt.Sprintf("%d", port)
			}
			joinedPorts := strings.Join(portSlice, ",")
			keployAlias = keployAlias + " --passThroughPorts " + fmt.Sprintf("%v ", joinedPorts)
		}
		if recordFlags.ConfigPath != "." {
			keployAlias = keployAlias + " --configPath " + recordFlags.ConfigPath
		}
		if len(recordFlags.Path) > 0 {
			keployAlias = keployAlias + " --path " + recordFlags.Path
		}
		addtionalFlags := " --containerName " + recordFlags.ContainerName + " --buildDelay " + recordFlags.BuildDelay.String() + " --delay " + fmt.Sprintf("%d", recordFlags.Delay) + " --proxyport " + fmt.Sprintf("%d", recordFlags.Proxyport) + " --networkName " + recordFlags.NetworkName + " --enableTele=" + fmt.Sprintf("%v", recordFlags.EnableTele)
		if isDockerCompose {
			keployAlias = keployAlias + addtionalFlags
			cmd = exec.Command("sh", "-c", keployAlias)
		} else {
			keployAlias = keployAlias + addtionalFlags
			cmd = exec.Command("sh", "-c", keployAlias)
		}
	} else {
		keployAlias = keployAlias + "\"" + testFlags.Command + "\" "
		if len(testFlags.PassThroughPorts) > 0 {
			portSlice := make([]string, len(recordFlags.PassThroughPorts))
			for i, port := range recordFlags.PassThroughPorts {
				portSlice[i] = fmt.Sprintf("%d", port)
			}
			joinedPorts := strings.Join(portSlice, ",")
			keployAlias = keployAlias + " --passThroughPorts " + fmt.Sprintf("%v ", joinedPorts)
		}
		if testFlags.ConfigPath != "." {
			keployAlias = keployAlias + " --configPath " + testFlags.ConfigPath
		}
		if len(testFlags.Testsets) > 0 {
			keployAlias = keployAlias + " --testsets " + fmt.Sprintf("%v", testFlags.Testsets)
		}
		if len(testFlags.Path) > 0 {
			keployAlias = keployAlias + " --path " + testFlags.Path
		}
		addtionalFlags := " --containerName " + testFlags.ContainerName + " --buildDelay " + testFlags.BuildDelay.String() + " --delay " + fmt.Sprintf("%d", testFlags.Delay) + " --networkName " + testFlags.NetworkName + " --enableTele=" + fmt.Sprintf("%v", testFlags.EnableTele) + " --apiTimeout " + fmt.Sprintf("%d", testFlags.ApiTimeout) + " --mongoPassword " + testFlags.MongoPassword + " --coverageReportPath " + testFlags.CoverageReportPath + " --withCoverage " + fmt.Sprintf("%v", testFlags.WithCoverage) + " --proxyport " + fmt.Sprintf("%d", testFlags.Proxyport)
		if isDockerCompose {
			keployAlias = keployAlias + addtionalFlags
			cmd = exec.Command("sh", "-c", keployAlias)
		} else {
			keployAlias = keployAlias + addtionalFlags
			cmd = exec.Command("sh", "-c", keployAlias)
		}
	}

	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		logger.Error("Failed to start keploy in docker", zap.Error(err))
		return
	}

}
