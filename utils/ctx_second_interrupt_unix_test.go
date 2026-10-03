//go:build unix

package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// secondInterruptHelperDir, set, makes TestSecondInterruptHelper a keploy run
// whose stop waits on work that only a second interrupt ends (a recording's
// drain while frames keep coming).
const secondInterruptHelperDir = "KEPLOY_TEST_SECOND_INTERRUPT_HELPER"

func TestSecondInterruptHelper(t *testing.T) {
	dir := os.Getenv(secondInterruptHelperDir)
	if dir == "" {
		return
	}
	mark := func(name string) {
		tmp := filepath.Join(dir, name+".tmp")
		if os.WriteFile(tmp, nil, 0o600) != nil || os.Rename(tmp, filepath.Join(dir, name)) != nil {
			os.Exit(3)
		}
	}
	ctx := NewCtx()
	mark("ready")
	<-ctx.Done()
	mark("stopping")
	select {
	case <-InterruptedAgain():
		mark("gave-up")
		os.Exit(0)
	case <-time.After(20 * time.Second):
		os.Exit(4)
	}
}

// A stop that waits for as long as it makes progress (a recording draining
// what the agent captured) must still be ended by a second Ctrl+C. SIGINT
// stayed registered and unread after the first, so the second did nothing and
// only SIGKILL, which skips all cleanup, could end the process.
func TestNewCtx_SecondInterruptEndsTheStop(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSecondInterruptHelper$")
	cmd.Env = append(os.Environ(), secondInterruptHelperDir+"="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	waitFor := func(name string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				t.Fatalf("the run never got to %q", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("ready")
	_ = cmd.Process.Signal(syscall.SIGINT)
	waitFor("stopping")
	_ = cmd.Process.Signal(syscall.SIGINT)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the run did not end on the second interrupt: %v", err)
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("a second interrupt did not end the stop")
	}
	if _, err := os.Stat(filepath.Join(dir, "gave-up")); err != nil {
		t.Fatal("the stop's work was not told of the second interrupt")
	}
}
