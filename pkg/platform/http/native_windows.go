//go:build windows && amd64

package http

import (
	"os"
	"os/exec"

	"go.keploy.io/server/v3/pkg/agent/hooks/winshim"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// prepareNativeInterception is the client-side half of the unprivileged Windows
// backend.
//
// The agent owns the control pipe and the destination map; this side owns the
// application launch. Windows has no DYLD_INSERT_LIBRARIES, so getting the shim
// into the application means creating the process suspended, mapping the DLL
// into it and resuming it — which can only be done at the moment os/exec creates
// the process. That is what the starter registered here does.
//
// Only the FIRST process needs it: the shim hooks CreateProcessW/A and carries
// itself into every descendant, which matters because the process keploy starts
// is `cmd /C <user command>` and the application is that shell's child.
//
// Best-effort by design. Any failure here leaves the run on exactly the path it
// would have taken without this backend rather than aborting it.
func (a *AgentClient) prepareNativeInterception(opts models.SetupOptions) {
	if opts.IsDocker {
		// Docker runs the agent inside the Linux VM, where the eBPF hooks apply;
		// there is no host process to instrument.
		return
	}
	if !winshim.Enabled() {
		a.logger.Warn("Keploy interception is disabled by " + winshim.EnvDisable + "; the application will run without being recorded or mocked")
		return
	}

	sessionDir, err := winshim.EnsureSessionDir(opts.ClientNSPID)
	if err != nil {
		a.logger.Warn("could not prepare the Windows interception session; the application will run without interception", zap.Error(err))
		return
	}
	pipeName := winshim.ControlPipeName(opts.ClientNSPID)

	// Stage the shim now rather than relying on the agent having done it: the
	// agent starts concurrently, and a DLL that is not on disk when the
	// application is created cannot be injected into it.
	dll, err := winshim.StageShim(a.logger, sessionDir, pipeName)
	if err != nil {
		a.logger.Warn("could not stage the Windows interception shim; the application will run without interception", zap.Error(err))
		return
	}

	logger := a.logger
	debug := a.conf.Debug
	utils.SetNativeAppStarter(func(cmd *exec.Cmd) error {
		// The shim reads the control pipe name from the sidecar next to the DLL,
		// so these only carry debug tracing.
		if debug {
			if cmd.Env == nil {
				cmd.Env = os.Environ()
			}
			cmd.Env = winshim.PrepareApplicationEnv(cmd.Env, sessionDir, true)
		}
		return winshim.StartInstrumented(logger, cmd, dll)
	})

	a.logger.Debug("armed Windows interception for the application launch",
		zap.String("shim", dll), zap.String("control_pipe", pipeName))
}
