//go:build !windows

package docker

import (
	"context"
	"os/exec"
	"syscall"

	"go.keploy.io/server/v3/utils"
)

func PrepareDockerCommand(ctx context.Context, keployAlias string) (*exec.Cmd, error) {
	// Run via `sh -c` when a shell is available, else fall back to a direct
	// exec so keploy still works on distroless images with no /bin/sh.
	cmd, err := utils.CommandContext(ctx, keployAlias)
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	return cmd, nil
}
