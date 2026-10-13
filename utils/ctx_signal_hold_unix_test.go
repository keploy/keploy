//go:build unix

package utils

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// signalHoldHelperDir, set, makes TestSignalHoldHelper a process whose first
// stop signal waits on a hold (SetSignalHold) that the test releases by
// creating "release" in the directory, or a second signal cuts short.
const signalHoldHelperDir = "KEPLOY_TEST_SIGNAL_HOLD_HELPER"

// signalHoldLinger, set, keeps the helper running after its root context is
// cancelled until a later signal reaches it.
const signalHoldLinger = "KEPLOY_TEST_SIGNAL_HOLD_LINGER"

// signalHoldNone, set, runs the helper with no hold, as the CLI runs.
const signalHoldNone = "KEPLOY_TEST_SIGNAL_HOLD_NONE"

// signalHoldOffer, set, has the helper's stop tell the user, once its root
// context is cancelled, that another signal ends it now (OfferStopNow), as a
// recording's drain does.
const signalHoldOffer = "KEPLOY_TEST_SIGNAL_HOLD_OFFER"

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
	hold := func(stopNow <-chan struct{}) {
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
	}
	if os.Getenv(signalHoldNone) == "" {
		SetSignalHold(hold)
	}
	ctx := NewCtx()
	mark("ready")
	<-ctx.Done()
	if os.Getenv(signalHoldOffer) != "" {
		OfferStopNow()
	}
	mark("cancelled")
	if os.Getenv(signalHoldLinger) != "" {
		// Still stopping when a later signal comes (the agent's client
		// stopping it once it has closed its streams).
		<-InterruptedAfter(0)
		mark("again")
		os.Exit(0)
	}
	if _, err := os.Stat(filepath.Join(dir, "stopped-now")); err == nil && RepeatInterrupts() == 0 {
		os.Exit(5) // a second signal that cut the hold short is not counted
	}
	os.Exit(0)
}

// signalHoldRun starts the helper and returns it, with a func that waits for a
// mark (failing after a generous bound: it only ends a broken run).
func signalHoldRun(t *testing.T, env ...string) (*exec.Cmd, string, func(string), chan error) {
	cmd, dir, waitFor, exited, _ := signalHoldRunOut(t, env...)
	return cmd, dir, waitFor, exited
}

// signalHoldRunOut is signalHoldRun, and also returns what the helper writes
// to its stdout (where NewCtx says what a signal does), whole once it exited.
func signalHoldRunOut(t *testing.T, env ...string) (*exec.Cmd, string, func(string), chan error, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalHoldHelper$")
	cmd.Env = append(append(os.Environ(), signalHoldHelperDir+"="+dir), env...)
	out := &bytes.Buffer{}
	cmd.Stdout = out
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
	return cmd, dir, waitFor, exited, out
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
// interrupted again (RepeatInterrupts), and the root context is cancelled.
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

// A signal after the hold finished (the agent's recording client closed its
// streams, and then stopped the agent) cuts nothing short: it is not said to
// lose what was being saved. One that cuts the hold short still is.
func TestNewCtx_ASignalAfterTheHoldFinishedLosesNothing(t *testing.T) {
	cmd, dir, waitFor, exited, out := signalHoldRunOut(t, signalHoldLinger+"=1")
	waitFor("ready")
	_ = cmd.Process.Signal(syscall.SIGINT)
	waitFor("holding")
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor("cancelled")
	_ = cmd.Process.Signal(syscall.SIGTERM)
	waitFor("again")
	if err := <-exited; err != nil {
		t.Fatalf("the run ended with %v", err)
	}
	said := out.String()
	if !strings.Contains(said, "Signal received again") || strings.Contains(said, "not saved") {
		t.Fatalf("a signal after the hold finished was said to lose what was being saved:\n%s", said)
	}

	cmd, _, waitFor, exited, out = signalHoldRunOut(t)
	waitFor("ready")
	_ = cmd.Process.Signal(syscall.SIGINT)
	waitFor("holding")
	_ = cmd.Process.Signal(syscall.SIGTERM)
	waitFor("cancelled")
	if err := <-exited; err != nil {
		t.Fatalf("the run ended with %v", err)
	}
	if !strings.Contains(out.String(), "not saved") {
		t.Fatalf("a signal that cut the hold short was not said to lose what was being saved:\n%s", out.String())
	}
}

// With no hold (the CLI), a second signal after the stop told the user one
// would end it at once (its drain does: OfferStopNow) stops it now, and says
// so; what that gives up, the stop says itself. One before that is said, and
// claims nothing: it may be the first signal again, as sudo hands it on.
func TestNewCtx_ASecondSignalWithNoHoldStopsNowOnlyOnceOffered(t *testing.T) {
	for _, offered := range []bool{true, false} {
		env := []string{signalHoldNone + "=1", signalHoldLinger + "=1"}
		if offered {
			env = append(env, signalHoldOffer+"=1")
		}
		cmd, _, waitFor, exited, out := signalHoldRunOut(t, env...)
		waitFor("ready")
		_ = cmd.Process.Signal(syscall.SIGINT)
		waitFor("cancelled")
		_ = cmd.Process.Signal(syscall.SIGINT)
		waitFor("again")
		if err := <-exited; err != nil {
			t.Fatalf("the run ended with %v", err)
		}
		said := out.String()
		switch {
		case offered && !strings.Contains(said, "Signal received again: interrupt, stopping now"):
			t.Fatalf("a second signal after the stop offered to end was not said to stop it now:\n%s", said)
		case !offered && (strings.Contains(said, "stopping now") || strings.Contains(said, "not saved") ||
			!strings.Contains(said, "Signal received again: interrupt")):
			t.Fatalf("a second signal before the stop said what one does was said to cut it short:\n%s", said)
		}
	}
}
