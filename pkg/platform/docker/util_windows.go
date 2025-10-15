//go:build windows

package docker

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

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
