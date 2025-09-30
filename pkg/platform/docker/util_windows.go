//go:build windows

package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

func ExtractInodeByPid(pid int) (string, error) {
	// Execute command in the container to get the PID namespace
	output, err := exec.Command("docker", "exec", "keploy-init", "stat", "/proc/1/ns/pid").Output()
	if err != nil {
		return "", err
	}
	outputStr := string(output)

	// Use a regular expression to extract the inode from the output
	re := regexp.MustCompile(`pid:\[(\d+)\]`)
	match := re.FindStringSubmatch(outputStr)

	if len(match) < 2 {
		return "", fmt.Errorf("failed to extract PID namespace inode")
	}

	pidNamespace := match[1]
	return pidNamespace, nil
}

func PrepareDockerCommand(ctx context.Context, keployAlias string) *exec.Cmd {
	var args []string
	args = append(args, "/C")
	args = append(args, strings.Split(keployAlias, " ")...)
	args = append(args, os.Args[1:]...)
	// Use cmd.exe /C for Windows
	cmd := exec.CommandContext(
		ctx,
		"cmd.exe",
		args...,
	)

	return cmd
}
