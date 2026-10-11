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

// signalHoldHelperDir, set, makes TestSignalHoldHelper a process whose first
// stop signal waits on a hold (SetSignalHold) that the test releases by
// creating "release" in the directory, or a second signal cuts short.
const signalHoldHelperDir = "KEPLOY_TEST_SIGNAL_HOLD_HELPER"

func TestSignalHoldHelper(t *testing.T) {
	dir := os.Getenv(signalHoldHelperDir)
	if dir == "" {
		return
	}
	mark := func(name string) {
		tmp := filepath.Join(dir, name+".tmp")
		if os.WriteFile(tmp, nil, 0o600) != nil || os.Rename(tmp, filepath.Join(dir, name)) != nil {
			os.Exit(3)
		}
	}
	SetSignalHold(func(stopNow <-chan struct{}) {
		mark("holding")
		for {
			if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
				mark("released")
				return
			}
			select {
			case <-stopNow:
				mark("stopped-now")
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
	ctx := NewCtx()
	mark("ready")
	<-ctx.Done()
	mark("cancelled")
	if _, err := os.Stat(filepath.Join(dir, "stopped-now")); err == nil {
		select {
		case <-InterruptedAgain():
		default:
			os.Exit(5) // a second signal that cut the hold short is not marked
		}
	}
	os.Exit(0)
}

// signalHoldRun starts the helper and returns it, with a func that waits for a
// mark (failing after a generous bound: it only ends a broken run).
func signalHoldRun(t *testing.T) (*exec.Cmd, string, func(string), chan error) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalHoldHelper$")
	cmd.Env = append(os.Environ(), signalHoldHelperDir+"="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitFor := func(name string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the run never got to %q", name)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	return cmd, dir, waitFor, exited
}

func marked(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// The first stop signal waits on the hold before it cancels: what the process
// still does for someone else (an agent serving a recording's drain) finishes
// first, and the root context is cancelled only once the hold returns.
func TestNewCtx_TheFirstSignalWaitsOnTheHold(t *testing.T) {
	cmd, dir, waitFor, exited := signalHoldRun(t)
	waitFor("ready")
	_ = cmd.Process.Signal(syscall.SIGINT)
	waitFor("holding")
	if marked(dir, "cancelled") {
		t.Fatal("the root context was cancelled while the hold had not returned")
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor("cancelled")
	if err := <-exited; err != nil {
		t.Fatalf("the run ended with %v", err)
	}
	if !marked(dir, "released") || marked(dir, "stopped-now") {
		t.Fatal("the hold did not end by its release")
	}
}

// A second signal while the hold waits cuts it short (stopNow), marks the run
// InterruptedAgain, and the root context is cancelled.
func TestNewCtx_ASecondSignalCutsTheHoldShort(t *testing.T) {
	cmd, dir, waitFor, exited := signalHoldRun(t)
	waitFor("ready")
	_ = cmd.Process.Signal(syscall.SIGINT)
	waitFor("holding")
	_ = cmd.Process.Signal(syscall.SIGTERM)
	waitFor("stopped-now")
	waitFor("cancelled")
	if err := <-exited; err != nil {
		t.Fatalf("the run ended with %v", err)
	}
	if marked(dir, "released") {
		t.Fatal("the hold was released, not cut short")
	}
}
