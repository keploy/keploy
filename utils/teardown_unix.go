//go:build unix

package utils

import (
	"os"
	"os/signal"
	"syscall"
)

// EscalationSignal is what the CLI sends a stopping agent it has to kill but
// may not signal itself: an agent elevated by sudo runs as root, which the
// user who ran keploy cannot send SIGKILL, while sudo keeps that user's uid
// and hands this signal on to the agent (checked with sudo 1.9.16p2).
const EscalationSignal = syscall.SIGUSR2

// DieOnEscalation has EscalationSignal kill this process (SIGKILL to itself)
// wherever it is. It is for `keploy agent`, whose CLI sends it only when its
// stop has to be forced: a second Ctrl+C, or a teardown that has stopped
// making progress or has detached everything and not exited
// (pkg/platform/http, stopWatch).
func DieOnEscalation() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, EscalationSignal)
	go func() {
		<-c
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	}()
}
