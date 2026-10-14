//go:build unix

package utils

import (
	"bytes"
	"fmt"
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
	hooksRan := make(chan struct{})
	RegisterPreCancelHook(func() { mark("hooks"); close(hooksRan) })
	hold := func() {
		mark("holding")
		var repeats uint64
		stopping := false
		for {
			// Each signal after the first, as it is counted.
			for n := RepeatInterrupts(); repeats < n; {
				repeats++
				mark(fmt.Sprintf("repeated-%d", repeats))
			}
			// The process stopped another way while it holds: the agent's
			// stop over its API (ExecCancel, as utils.Stop does).
			if _, err := os.Stat(filepath.Join(dir, "stop-api")); err == nil && !stopping {
				stopping = true
				ExecCancel()
			}
			if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
				mark("released")
				return
			}
			time.Sleep(5 * time.Millisecond)
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
	// NewCtx goes on with the stop (its pre-cancel hooks) once it no longer
	// waits on the hold.
	<-hooksRan
	if os.Getenv(signalHoldLinger) != "" {
		// Still stopping when a later signal comes (the agent's client
		// stopping it once it has closed its streams).
		<-InterruptedAfter(0)
		select {
		case <-StopNow():
			mark("stopnow")
		default:
		}
		mark("again")
		os.Exit(0)
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
	if !marked(dir, "released") {
		t.Fatal("the hold did not end by its release")
	}
}

// A copy of the stop -- the same signal again, as the sudo that runs a
// non-root CLI's agent hands on what `sudo pkill -INT -f keploy` sent it --
// does not cut the hold short: it is counted and said, and the root context
// is cancelled only once the hold is released. Taken for a second Ctrl+C, it
// cut the agent's hold short, and the agent closed the streams its
// recording's client was still draining.
func TestNewCtx_ACopyOfTheStopDoesNotCutTheHoldShort(t *testing.T) {
	cmd, dir, waitFor, exited, out := signalHoldRunOut(t)
	waitFor("ready")
	_ = cmd.Process.Signal(syscall.SIGINT)
	waitFor("holding")
	_ = cmd.Process.Signal(syscall.SIGINT)       // sudo's copy
	deadline := time.Now().Add(30 * time.Second) // only ends a broken run
	for !marked(dir, "repeated-1") && !marked(dir, "cancelled") {
		if time.Now().After(deadline) {
			t.Fatal("the copy of the stop was never counted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if marked(dir, "cancelled") {
		t.Fatal("a copy of the stop cut the hold short: the root context was cancelled while it held")
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor("cancelled")
	if err := <-exited; err != nil {
		t.Fatalf("the run ended with %v", err)
	}
	if !marked(dir, "released") {
		t.Fatal("the hold did not end by its release")
	}
	said := out.String()
	if !strings.Contains(said, "Signal received again: interrupt; Keploy stops once its client has saved") || strings.Contains(said, "not saved") || strings.Contains(said, "stopping now") {
		t.Fatalf("the copy was not said, or was said to cut something short:\n%s", said)
	}
}

// No signal cuts the hold short, however many come, or however late: a copy
// of the stop can come any time after it (a sudo, or an agent, starved or
// frozen), and no time tells one from a second signal. Each is counted
// (RepeatInterrupts), and the hold ends by its release.
func TestNewCtx_NoRepeatedSignalCutsTheHoldShort(t *testing.T) {
	cmd, dir, waitFor, exited := signalHoldRun(t)
	waitFor("ready")
	_ = cmd.Process.Signal(syscall.SIGINT)
	waitFor("holding")
	for i, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGINT} {
		_ = cmd.Process.Signal(sig)
		waitFor(fmt.Sprintf("repeated-%d", i+1))
		if marked(dir, "cancelled") {
			t.Fatalf("signal %d after the stop (%v) cut the hold short", i+1, sig)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor("cancelled")
	if err := <-exited; err != nil {
		t.Fatalf("the run ended with %v", err)
	}
	if !marked(dir, "released") {
		t.Fatal("the hold did not end by its release")
	}
}

// Once the process is stopped some other way while it holds -- the agent's
// stop over its API, its escalation signal (ExecCancel) -- the stop no longer
// waits on the hold: it goes on (its pre-cancel hooks run) with the hold not
// released, and the run ends. A client that must stop the agent regardless
// still can.
func TestNewCtx_TheHoldEndsWhenTheProcessIsStoppedAnotherWay(t *testing.T) {
	cmd, dir, waitFor, exited := signalHoldRun(t)
	waitFor("ready")
	_ = cmd.Process.Signal(syscall.SIGINT)
	waitFor("holding")
	if err := os.WriteFile(filepath.Join(dir, "stop-api"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor("cancelled")
	waitFor("hooks")
	if err := <-exited; err != nil {
		t.Fatalf("the run ended with %v", err)
	}
	if marked(dir, "released") {
		t.Fatal("the stop went on only once the hold was released")
	}
}

// A signal after the hold finished (the agent's recording client closed its
// streams, and then stopped the agent) cuts nothing short: it is not said to
// lose what was being saved.
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
}

// With no hold (the CLI), a second signal after the stop told the user one
// would end it at once (its drain does: OfferStopNow) ends it (StopNow); one
// before that does not: it may be the first signal again, as sudo hands it
// on. Either is said, claiming nothing: what it gives up, the stop that acts
// on it says itself.
func TestNewCtx_ASecondSignalWithNoHoldStopsNowOnlyOnceOffered(t *testing.T) {
	for _, offered := range []bool{true, false} {
		env := []string{signalHoldNone + "=1", signalHoldLinger + "=1"}
		if offered {
			env = append(env, signalHoldOffer+"=1")
		}
		cmd, dir, waitFor, exited, out := signalHoldRunOut(t, env...)
		waitFor("ready")
		_ = cmd.Process.Signal(syscall.SIGINT)
		waitFor("cancelled")
		_ = cmd.Process.Signal(syscall.SIGINT)
		waitFor("again")
		if err := <-exited; err != nil {
			t.Fatalf("the run ended with %v", err)
		}
		if marked(dir, "stopnow") != offered {
			t.Fatalf("offered=%v: the second signal ended the stop: %v", offered, marked(dir, "stopnow"))
		}
		said := out.String()
		if !strings.Contains(said, "Signal received again: interrupt") || strings.Contains(said, "stopping now") || strings.Contains(said, "not saved") {
			t.Fatalf("offered=%v: the second signal was not said, or was said to cut something short:\n%s", offered, said)
		}
	}
}
