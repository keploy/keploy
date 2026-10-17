//go:build linux || darwin

package utils

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// startGroupLeader starts command (sh -c) in a process group of its own, as
// keploy starts a native app, and returns it with a channel closed once it
// has exited and been reaped. It is killed when the test ends.
func startGroupLeader(t *testing.T, command string) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); <-reaped })
	return cmd, reaped
}

// awaitReaped fails t unless the process behind reaped exits; the bound only
// turns a hang into a failure.
func awaitReaped(t *testing.T, what string, reaped <-chan struct{}) {
	t.Helper()
	select {
	case <-reaped:
	case <-time.After(time.Minute):
		t.Fatalf("%s outlived the stop", what)
	}
}

// A tree the stop cannot read still has its leader's group interrupted,
// waited for and, if need be, killed, and the stop says so: it used to return
// with no signal sent, and the app was killed when the command's WaitDelay
// ran out, unsaid.
func TestATreeTheStopCannotReadIsStillStopped(t *testing.T) {
	orig := processTree
	processTree = func(int) ([]int, func(int) (int, error), error) { return nil, nil, errors.New("no /proc here") }
	t.Cleanup(func() { processTree = orig })
	for _, tc := range []struct {
		name, command string
		killed        bool
	}{
		{"exits on SIGINT", "touch ready; exec sleep 600", false},
		{"ignores SIGINT", `trap "" INT; touch ready; exec sleep 600`, true},
	} {
		dir := t.TempDir()
		cmd, reaped := startGroupLeader(t, "cd "+dir+" && "+tc.command)
		// The shell has set its trap once it writes ready. A SIGINT before the
		// exec ends the first shell as it would end sleep, and the second
		// ignores it as sleep does. (Not /proc: macOS has none.)
		for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the app did not start", tc.name)
			}
		}
		killed, err := InterruptProcessTreeKilled(zap.NewNop(), cmd.Process.Pid, syscall.SIGINT)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if (killed != "") != tc.killed {
			t.Errorf("%s: the stop said %q, want a kill %v", tc.name, killed, tc.killed)
		}
		awaitReaped(t, tc.name, reaped)
	}
}

// A group the stop could not signal was not asked to stop, and the stop says
// it could not let it stop on its own: what ends it next (the command's
// WaitDelay) is no exit of its own.
func TestAGroupTheStopCannotSignalIsSaid(t *testing.T) {
	orig := signalGroup
	signalGroup = func(logger *zap.Logger, pid int, sig syscall.Signal) error {
		if sig == syscall.SIGINT {
			return syscall.EPERM
		}
		return orig(logger, pid, sig)
	}
	t.Cleanup(func() { signalGroup = orig })
	cmd, _ := startGroupLeader(t, "exec sleep 600")
	killed, err := InterruptProcessTreeKilled(zap.NewNop(), cmd.Process.Pid, syscall.SIGINT)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(killed, "could not be signalled") {
		t.Fatalf("the stop said %q, want the group it could not signal named", killed)
	}
}
