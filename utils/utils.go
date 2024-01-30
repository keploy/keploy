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

var Version string

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
		if len(dockerContext) == 0 {
			logger.Error("Could not get the current docker context")
			return
		}
		dockerContext = strings.Split(dockerContext, "\n")[0]
		if dockerContext == "colima" {
			logger.Info("Starting keploy in docker with colima context, as that is the current context.")
			*keployAlias = "docker run --pull always --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host -it -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy "
		} else {
			logger.Info("Starting keploy in docker with default context, as that is the current context.")
			*keployAlias = "docker run --pull always --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host -it -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy "
		}
	} else if osName == "linux" {
		*keployAlias = "sudo docker run --pull always --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host -it -v " + os.Getenv("PWD") + ":" + os.Getenv("PWD") + " -w " + os.Getenv("PWD") + " -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm ghcr.io/keploy/keploy "
	}
}

func appendFlags(flagName string, flagValue string) string {
	if len(flagValue) > 0 {
		// Check for = in the flagName.
		if strings.Contains(flagName, "=") {
			return " --" + flagName + flagValue
		}
		return " --" + flagName + " " + flagValue
	}
	return ""
}

func UpdateKeployToDocker(cmdName string, isDockerCompose bool, flags interface{}, logger *zap.Logger) {
	var recordFlags RecordFlags
	var testFlags TestFlags
	//Check the type of flags.
	switch flag := flags.(type) {
	case RecordFlags:
		recordFlags = flag
	case TestFlags:
		testFlags = flag
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
			keployAlias = keployAlias + " --passThroughPorts=" + fmt.Sprintf("%v ", joinedPorts)
		}
		if recordFlags.ConfigPath != "." {
			keployAlias = keployAlias + " --configPath " + recordFlags.ConfigPath
		}
		if len(recordFlags.Path) > 0 {
			keployAlias = keployAlias + " --path " + recordFlags.Path
		}
		addtionalFlags := appendFlags("containerName", recordFlags.ContainerName) + appendFlags("buildDelay ", recordFlags.BuildDelay.String()) + appendFlags("delay", fmt.Sprintf("%d", recordFlags.Delay)) + appendFlags("proxyport", fmt.Sprintf("%d", recordFlags.Proxyport)) + appendFlags("networkName", recordFlags.NetworkName) + appendFlags("enableTele=", fmt.Sprintf("%v", recordFlags.EnableTele))
		keployAlias = keployAlias + addtionalFlags
		cmd = exec.Command("sh", "-c", keployAlias)

	} else {
		keployAlias = keployAlias + "\"" + testFlags.Command + "\" "
		if len(testFlags.PassThroughPorts) > 0 {
			portSlice := make([]string, len(recordFlags.PassThroughPorts))
			for i, port := range recordFlags.PassThroughPorts {
				portSlice[i] = fmt.Sprintf("%d", port)
			}
			joinedPorts := strings.Join(portSlice, ",")
			keployAlias = keployAlias + " --passThroughPorts=" + fmt.Sprintf("%v ", joinedPorts)
		}
		if testFlags.ConfigPath != "." {
			keployAlias = keployAlias + " --configPath " + testFlags.ConfigPath
		}
		if len(testFlags.Testsets) > 0 {
			testSetSlice := make([]string, len(testFlags.Testsets))
			for i, testSet := range testFlags.Testsets {
				testSetSlice[i] = fmt.Sprintf("%v", testSet)
			}
			joinedTestSets := strings.Join(testSetSlice, ",")
			keployAlias = keployAlias + " --testsets=" + fmt.Sprintf("%v", joinedTestSets)
		}
		if len(testFlags.Path) > 0 {
			keployAlias = keployAlias + " --path " + testFlags.Path
		}
		addtionalFlags := appendFlags("containerName", testFlags.ContainerName) + appendFlags("buildDelay", testFlags.BuildDelay.String()) + appendFlags("delay", fmt.Sprintf("%d", testFlags.Delay)) + appendFlags("networkName", testFlags.NetworkName) + appendFlags("enableTele=", fmt.Sprintf("%v", testFlags.EnableTele)) + appendFlags("apiTimeout", fmt.Sprintf("%d", testFlags.ApiTimeout)) + appendFlags("mongoPassword", testFlags.MongoPassword) + appendFlags("coverageReportPath", testFlags.CoverageReportPath) + appendFlags("withCoverage", fmt.Sprintf("%v", testFlags.WithCoverage)) + appendFlags("proxyport", fmt.Sprintf("%d", testFlags.Proxyport))
		keployAlias = keployAlias + addtionalFlags
		cmd = exec.Command("sh", "-c", keployAlias)
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
