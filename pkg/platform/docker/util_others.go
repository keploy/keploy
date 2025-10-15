//go:build !windows

package docker

import (
	"context"
	"os/exec"
	"syscall"
)

func PrepareDockerCommand(ctx context.Context, keployAlias string) *exec.Cmd {
	// Use sh -c for Unix-like systems
	cmd := exec.CommandContext(
		ctx,
		"sh",
		"-c",
		keployAlias,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	return cmd
}
