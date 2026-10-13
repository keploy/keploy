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

// secondInterruptHelperDir, set, makes TestSecondInterruptHelper a keploy run
// whose stop waits on work that only the user's second interrupt ends (a
// recording's drain while frames keep coming).
const secondInterruptHelperDir = "KEPLOY_TEST_SECOND_INTERRUPT_HELPER"

// The helper's run: once stopped, it waits for a repeat of the interrupt (the
// copy sudo hands on, sent by the test), checks it ended nothing, then tells
// the user another interrupt stops it now (OfferStopNow), and waits for one.
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
	// The bounds here only end a helper the test has given up on.
	deadline := time.Now().Add(20 * time.Second)
	for RepeatInterrupts() < 1 {
		if time.Now().After(deadline) {
			os.Exit(4)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-StopNow():
		os.Exit(5) // the copy of the first interrupt ended the stop
	default:
	}
	OfferStopNow()
	mark("offered")
	select {
	case <-StopNow():
		mark("gave-up")
		os.Exit(0)
	case <-time.After(20 * time.Second):
		os.Exit(6)
	}
}

// A stop that waits for as long as it makes progress (a recording draining
// what the agent captured) is ended by the user's second Ctrl+C: SIGINT stayed
// registered and unread after the first, so the second did nothing and only
// SIGKILL, which skips all cleanup, could end the process. But only by one
// that comes after the stop told the user so (OfferStopNow). A stop sent to
// both sudo and keploy (`sudo pkill -INT -f keploy`, as CI stops a recording)
// reaches keploy twice: the copy sudo hands on can come after the stop has
// begun, and taken for a second Ctrl+C it gave the recording's drain up at its
// start, with what the agent still held lost.
func TestNewCtx_OnlyAnInterruptAfterTheStopSaysSoEndsIt(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSecondInterruptHelper$")
	cmd.Env = append(os.Environ(), secondInterruptHelperDir+"="+dir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	waitFor := func(name string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return
			}
			select {
			case err := <-exited:
				t.Fatalf("the run ended (%v) before it got to %q:\n%s", err, name, out.String())
			default:
			}
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				t.Fatalf("the run never got to %q", name)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("ready")
	_ = cmd.Process.Signal(syscall.SIGINT) // the stop
	waitFor("stopping")
	_ = cmd.Process.Signal(syscall.SIGINT) // its copy, from sudo, after the stop began
	waitFor("offered")                     // the copy ended nothing
	_ = cmd.Process.Signal(syscall.SIGINT) // the user's second Ctrl+C
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("the run did not end on the user's second interrupt: %v\n%s", err, out.String())
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the user's second interrupt did not end the stop")
	}
	if _, err := os.Stat(filepath.Join(dir, "gave-up")); err != nil {
		t.Fatal("the stop's work was not told of the user's second interrupt")
	}
	said := out.String()
	if strings.Count(said, "Signal received again: interrupt") != 2 || strings.Count(said, "stopping now") != 1 {
		t.Fatalf("want both repeats said, and only the user's second said to stop the run now:\n%s", said)
	}
}
