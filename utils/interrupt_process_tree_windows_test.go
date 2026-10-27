//go:build windows

package utils

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// A stop on Windows that had to force-kill the app says so: ping -t answers a
// CTRL_BREAK with its statistics and goes on, so the stop's grace runs out
// and taskkill /F ends it. The stop used to set its kill and then return
// none, and a set's end that killed its app was judged.
func TestTheWindowsStopSaysItKilled(t *testing.T) {
	cmd := exec.Command("ping", "-t", "127.0.0.1")
	// As keploy starts a native app on Windows: the CTRL_BREAK goes to its
	// group, not to this test's.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-reaped })

	killed, err := InterruptProcessTreeKilled(zap.NewNop(), cmd.Process.Pid, syscall.SIGINT)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(killed, "so keploy killed it") {
		t.Fatalf("the stop said %q; want the kill it made said", killed)
	}
	// The bound only turns a process the kill missed into a failure.
	select {
	case <-reaped:
	case <-time.After(time.Minute):
		t.Fatal("ping outlived the stop that said it killed it")
	}
}
