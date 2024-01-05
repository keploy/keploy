package utils

import (
	"bufio"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
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

func UpdateKeployToDocker(cmdName string, appCmd string, isDockerCompose bool, appContainer string, buildDelay string) {
	workingDir, _ := os.Getwd()
	var cmd *exec.Cmd
	if isDockerCompose {
		cmd = exec.Command("sudo", "docker", "run", "-e", "BINARY_TO_DOCKER=true", "--pull", "always", "--name", "keploy-v2", "-p", "16789:16789", "--privileged", "--pid=host", "-it", "-v", fmt.Sprintf("%s:/files", workingDir), "-v", "/sys/fs/cgroup:/sys/fs/cgroup", "-v", "/sys/kernel/debug:/sys/kernel/debug", "-v", "/sys/fs/bpf:/sys/fs/bpf", "-v", "/var/run/docker.sock:/var/run/docker.sock", "--rm", "ghcr.io/keploy/keploy", cmdName, "-c", appCmd, "--containerName", appContainer, "--buildDelay", buildDelay)
	} else {
		cmd = exec.Command("sudo", "docker", "run", "-e", "BINARY_TO_DOCKER=true", "--pull", "always", "--name", "keploy-v2", "-p", "16789:16789", "--privileged", "--pid=host", "-it", "-v", fmt.Sprintf("%s:/files", workingDir), "-v", "/sys/fs/cgroup:/sys/fs/cgroup", "-v", "/sys/kernel/debug:/sys/kernel/debug", "-v", "/sys/fs/bpf:/sys/fs/bpf", "-v", "/var/run/docker.sock:/var/run/docker.sock", "--rm", "ghcr.io/keploy/keploy", cmdName, "-c", appCmd)
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
