//go:build linux || darwin
// +build linux darwin

package utils

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestTerminateCommandSignalsAndNeverKills guards the agent's stop:
// TerminateCommand sends SIGTERM and returns, and it
// never escalates to SIGKILL however long the process takes to exit. The
// keploy agent removes its TLS uprobes as it shuts down, and a SIGKILL that
// lands part way through a removal leaks the uprobe ("failed to unregister,
// leaking uprobe"), so the CLI must wait for the agent's own exit instead.
func TestTerminateCommandSignalsAndNeverKills(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	got := filepath.Join(dir, "got-term")
	release := filepath.Join(dir, "release")
	// On SIGTERM: note it, then keep "shutting down" until the test releases
	// it -- a slow shutdown, longer than any grace a timer would allow.
	script := `trap 'touch "$GOT"; while [ ! -e "$REL" ]; do sleep 0.05; done; exit 0' TERM
touch "$READY"
while :; do sleep 0.05; done`
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "READY="+ready, "GOT="+got, "REL="+release)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // mirror NewAgentCommand
	if err := StartCommand(cmd); err != nil {
		t.Fatalf("start: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	// A failing assertion must not leave the shell looping (Kill is safe
	// after the reap; a group kill by pid would not be).
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitForFile := func(path string) {
		t.Helper()
		for {
			if _, err := os.Stat(path); err == nil {
				return
			}
			select {
			case err := <-waited:
				t.Fatalf("the process exited (%v) before %s appeared", err, filepath.Base(path))
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	waitForFile(ready) // its SIGTERM handler is installed

	if err := TerminateCommand(cmd, zap.NewNop()); err != nil {
		t.Fatalf("TerminateCommand: %v", err)
	}
	waitForFile(got)
	// Still shutting down, and still alive: nothing escalated.
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("the process is gone while it was still shutting down: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := <-waited; err != nil {
		t.Fatalf("the process did not exit on its own (status %v): it was killed", err)
	}
}

// A process that has already exited is not an error for TerminateCommand.
func TestTerminateCommandOnAnExitedProcess(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := TerminateCommand(cmd, zap.NewNop()); err != nil {
		t.Fatalf("TerminateCommand on an exited process: %v", err)
	}
}

// Once RestoreTerminal has given the terminal back, the stdin copy loop ends
// at the next keystroke (as a closed PTY ended it), instead of living on to
// swallow keystrokes meant for whatever reads stdin next -- a retried
// agent's sudo password prompt.
func TestStdinGateEndsTheCopyOnceTheTerminalIsRestored(t *testing.T) {
	h := &PTYHandle{}
	h.RestoreTerminal()
	if n, err := (stdinGate{h}).Write([]byte("x")); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Write after RestoreTerminal = (%d, %v), want (0, io.ErrClosedPipe)", n, err)
	}
}

// KillAgentCommand ends an agent started directly with SIGKILL. One started
// through sudo is root, which the user may not signal; sudo is sent the
// escalation signal instead, which it hands on to the agent
// (keployutils.EscalationSignal). Here "sudo" is a script of that name that
// says which signal reached it.
func TestKillAgentCommandKillsThroughSudo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wantUSR2 bool
	}{
		{"keploy", false},
		{"sudo", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin, got, ready := filepath.Join(dir, tc.name), filepath.Join(dir, "got"), filepath.Join(dir, "ready")
			script := "#!/bin/sh\ntrap 'echo usr2 > \"" + got + "\"; exit 0' USR2\ntouch \"" + ready + "\"\nwhile :; do sleep 0.01; done\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bin)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			hung := time.After(time.Minute) // only turns a hang into a failure
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				select {
				case <-hung:
					t.Fatal("the stand-in never started")
				case <-time.After(5 * time.Millisecond):
				}
			}
			if err := KillAgentCommand(cmd, zap.NewNop()); err != nil {
				t.Fatalf("KillAgentCommand: %v", err)
			}
			select {
			case <-exited:
			case <-time.After(time.Minute):
				t.Fatal("the stand-in outlived KillAgentCommand")
			}
			st := cmd.ProcessState.Sys().(syscall.WaitStatus)
			_, gotUSR2 := os.Stat(got)
			if tc.wantUSR2 {
				if gotUSR2 != nil {
					t.Fatalf("sudo was not sent the escalation signal (it ended with %v)", cmd.ProcessState)
				}
				return
			}
			if !st.Signaled() || st.Signal() != syscall.SIGKILL {
				t.Fatalf("an agent started directly ended with %v, want SIGKILL", cmd.ProcessState)
			}
			// And never signals a process already reaped.
			if err := KillAgentCommand(cmd, zap.NewNop()); err != nil {
				t.Fatalf("KillAgentCommand on a reaped process: %v", err)
			}
		})
	}
}
