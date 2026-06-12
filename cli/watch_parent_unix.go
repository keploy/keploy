//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// watchParentInterval is how often the watchdog polls the parent client's
// liveness. One second is small enough that a leftover agent never blocks the
// next keploy run for long, and large enough to be negligible overhead.
const watchParentInterval = time.Second

// watchParentProcess terminates this agent process when its parent keploy
// client (clientPID) goes away.
//
// clientPID is a real process id: the launcher passes the client's os.Getpid()
// via --client-pid (see pkg/platform/http/agent.go and docker/util.go), and it
// is consumed as a pid elsewhere too (/proc/<pid>/environ auto-detection). So
// liveness via kill(pid, 0) below checks that exact process, not a group.
//
// The agent is spawned in its own process group (Setpgid) WITHOUT a
// PR_SET_PDEATHSIG and without watching its parent, so an ABNORMAL death of the
// parent CLI — `kill -9`, a panic/crash, the OOM-killer, or an abruptly-closed
// terminal — orphans the agent. The orphan keeps keploy's eBPF hooks, DNS
// takeover, and the proxy/ingress listeners alive, so the user's NEXT
// `keploy record`/`keploy test` cannot bind its ports and hangs. (A clean Ctrl-C
// is fine — that path already tears everything down.)
//
// This watchdog closes that gap: it polls the parent PID and, when it
// disappears, sends THIS process SIGTERM so the existing graceful-shutdown path
// runs (unloads eBPF, releases ports, restores DNS). Watching the original
// client PID — rather than relying on PDEATHSIG — is robust across the `sudo`
// the agent is usually launched under (whose death PDEATHSIG would otherwise
// track instead of the real CLI). No-op when clientPID <= 0.
func watchParentProcess(ctx context.Context, logger *zap.Logger, clientPID int) {
	if logger != nil {
		logger.Debug("arming parent-death watchdog", zap.Int("clientPID", clientPID))
	}
	if clientPID <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(watchParentInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !parentAlive(clientPID) {
					if logger != nil {
						logger.Info("parent client process exited; self-terminating to release eBPF hooks, DNS and ports",
							zap.Int("clientPID", clientPID))
					}
					// Reuse the normal signal-driven graceful shutdown.
					_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
					return
				}
			}
		}
	}()
}

// parentAlive reports whether process pid still exists. Signal 0 performs error
// checking without sending a signal: ESRCH => the process is gone; any other
// result (incl. EPERM — exists but not ours, not expected since the agent runs
// as root) is treated as "alive" so we never self-terminate on uncertainty.
func parentAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
