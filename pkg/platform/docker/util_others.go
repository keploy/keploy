//go:build !windows

package docker

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"go.keploy.io/server/v3/utils"
)

func PrepareDockerCommand(ctx context.Context, keployAlias string) (*exec.Cmd, error) {
	// An alias that hands docker the token through sudo (linuxDockerClient,
	// when keploy is not root) needs a sudo that keeps it. One that does not
	// would start the agent without a token.
	if strings.HasPrefix(keployAlias, sudoKeepingToken+" ") {
		if err := CheckSudoKeepsAgentToken(ctx, false /* root: the root alias has no sudo */); err != nil {
			return nil, err
		}
	}
	// Run via `sh -c` when a shell is available, else fall back to a direct
	// exec so keploy still works on distroless images with no /bin/sh.
	cmd, err := utils.CommandContext(ctx, keployAlias)
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
	// The alias names the control-plane token without a value; see getAlias.
	if extra := AgentTokenEnv(); len(extra) > 0 {
		cmd.Env = append(os.Environ(), extra...)
	}

	return cmd, nil
}
