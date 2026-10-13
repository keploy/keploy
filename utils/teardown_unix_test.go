//go:build unix

package utils

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestEscalationHelper is not a test of its own: with KEPLOY_TEST_ESCALATION
// set it is an agent that calls DieOnEscalation, says it is ready, and waits.
func TestEscalationHelper(t *testing.T) {
	dir := os.Getenv("KEPLOY_TEST_ESCALATION")
	if dir == "" {
		return
	}
	DieOnEscalation()
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		os.Exit(3)
	}
	time.Sleep(time.Hour)
	os.Exit(0)
}

// EscalationSignal kills an agent where it stands (SIGKILL): it is how the CLI
// forces the stop of an agent sudo runs as root, which it may not signal.
func TestDieOnEscalation(t *testing.T) {
	dir := t.TempDir()
	helper := exec.Command(os.Args[0], "-test.run=^TestEscalationHelper$")
	helper.Env = append(os.Environ(), "KEPLOY_TEST_ESCALATION="+dir)
	var out bytes.Buffer
	helper.Stdout, helper.Stderr = &out, &out
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- helper.Wait() }()
	t.Cleanup(func() { _ = helper.Process.Kill() })
	hung := time.After(time.Minute) // only turns a hang into a failure
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("the helper ended before it was ready: %v\n%s", err, out.String())
		case <-hung:
			t.Fatalf("the helper was never ready\n%s", out.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := helper.Process.Signal(EscalationSignal); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Minute):
		t.Fatalf("the helper outlived the escalation signal\n%s", out.String())
	}
	if st := helper.ProcessState.Sys().(syscall.WaitStatus); !st.Signaled() || st.Signal() != syscall.SIGKILL {
		t.Fatalf("the helper ended with %v, want it killed (SIGKILL)\n%s", helper.ProcessState, out.String())
	}
}
