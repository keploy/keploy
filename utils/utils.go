package utils

import (
	"bufio"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/log"
	sentry "github.com/getsentry/sentry-go"
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
		log.Error(Emoji+"Recovered from:", r)
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

func UpdateKeployToDocker(cmdName string, isDockerCompose bool, recordFlags RecordFlags, testFlags TestFlags) {
	// Get the name of the operating system.
	osName := runtime.GOOS
	if osName == "Windows" {
		log.Error("Windows is not supported yet. Use WSL2 instead.")
		return
	}
	var keployAlias string
	if osName == "darwin" {
		fmt.Println("Do you want to use keploy with Docker or Colima? (docker/colima):")
		reader := bufio.NewReader(os.Stdin)
		choice, _ := reader.ReadString('\n')
		choice = strings.ToLower(strings.TrimSpace(choice))
		//Get the current docker context.
		cmd := exec.Command("docker", "context", "ls", "--format", "{{.Name}}")
		out, err := cmd.Output()
		if err != nil {
			log.Error("Failed to get the current docker context", err.Error())
			return
		}
		dockerContext := strings.Split(strings.TrimSpace(string(out)), "\n")[0]
		dockerContext = strings.Split(dockerContext, "\n")[0]
		if choice == "colima" {
			if dockerContext == "default" {
				log.Error("Error: Docker is using the default context, set to colima using 'docker context use colima'")
				return
			}
			keployAlias = "sudo docker run --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host -it -v " + os.Getenv("PWD") + ":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm keploylocal " + cmdName + " -c "
		} else {
			if dockerContext == "colima" {
				log.Error("Error: Docker is using the colima context, set to default using 'docker context use default'")
				return
			}
			keployAlias = "sudo docker run --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host -it -v " + os.Getenv("PWD") + ":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v debugfs:/sys/kernel/debug:rw -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm keploylocal " + cmdName + " -c "
			fmt.Println("This is the alias", keployAlias)
		}
	}
	if osName == "linux" {
		keployAlias = "sudo docker run --name keploy-v2 -e BINARY_TO_DOCKER=true -p 16789:16789 --privileged --pid=host -it -v " + os.Getenv("PWD") + ":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock -v " + os.Getenv("HOME") + "/.keploy-config:/root/.keploy-config -v " + os.Getenv("HOME") + "/.keploy:/root/.keploy --rm keploylocal " + cmdName + " -c "
	}
	var cmd *exec.Cmd
	if cmdName == "record" {
		keployAlias = keployAlias + "\"" + recordFlags.Command + "\" "
		if len(recordFlags.PassThroughPorts) > 0 {
			keployAlias = keployAlias + " --passThroughPorts " + fmt.Sprintf("%v", recordFlags.PassThroughPorts)
		}
		if recordFlags.ConfigPath != "." {
			keployAlias = keployAlias + " --configPath " + recordFlags.ConfigPath
		}
		if len(testFlags.Path) > 0 {
			keployAlias = keployAlias + " --path " + recordFlags.Path
		}
		if isDockerCompose {
			addtionalFlags := "--containerName " + recordFlags.ContainerName + " --buildDelay " + recordFlags.BuildDelay.String() + " --delay " + fmt.Sprintf("%d", recordFlags.Delay) + " --proxyport " + fmt.Sprintf("%d", recordFlags.Proxyport) + " --networkName " + recordFlags.NetworkName + " --enableTele=" + fmt.Sprintf("%v", recordFlags.EnableTele)
			keployAlias = keployAlias + addtionalFlags
			cmd = exec.Command("sh", "-c", keployAlias)
			fmt.Println("This is the alias", keployAlias)
		} else {
			addtionalFlags := "--delay " + fmt.Sprintf("%d", recordFlags.Delay) + " --proxyport " + fmt.Sprintf("%d", recordFlags.Proxyport) + " --networkName " + recordFlags.NetworkName + " --enableTele=" + fmt.Sprintf("%v", recordFlags.EnableTele)
			keployAlias = keployAlias + addtionalFlags
			cmd = exec.Command("sh", "-c", keployAlias)
		}
	} else {
		keployAlias = keployAlias + "\"" + testFlags.Command + "\" "
		if len(testFlags.PassThroughPorts) > 0 {
			keployAlias = keployAlias + " --passThroughPorts " + fmt.Sprintf("%v", testFlags.PassThroughPorts)
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
		if isDockerCompose {
			addtionalFlags := cmdName + " -c \"" + testFlags.Command + "\" --containerName " + testFlags.ContainerName + " --buildDelay " + testFlags.BuildDelay.String() + " --delay " + fmt.Sprintf("%d", testFlags.Delay) + " --networkName " + testFlags.NetworkName + " --enableTele=" + fmt.Sprintf("%v", testFlags.EnableTele) + " --apiTimeout " + fmt.Sprintf("%d", testFlags.ApiTimeout) + " --mongoPassword " + testFlags.MongoPassword + " --coverageReportPath " + testFlags.CoverageReportPath + " --withCoverage " + fmt.Sprintf("%v", testFlags.WithCoverage)
			keployAlias = keployAlias + addtionalFlags
			cmd = exec.Command("sh", "-c", keployAlias)
		} else {
			additionalFlags := "--delay " + fmt.Sprintf("%d", testFlags.Delay) + " --networkName " + testFlags.NetworkName + " --enableTele=" + fmt.Sprintf("%v", testFlags.EnableTele) + " --apiTimeout " + fmt.Sprintf("%d", testFlags.ApiTimeout) + " --mongoPassword " + testFlags.MongoPassword + " --coverageReportPath " + testFlags.CoverageReportPath + " --withCoverage " + fmt.Sprintf("%v", testFlags.WithCoverage)
			keployAlias = keployAlias + additionalFlags
			cmd = exec.Command("sh", "-c", keployAlias)
		}
	}

	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		log.Error("Failed to start keploy in docker", err.Error())
		return
	}

}
