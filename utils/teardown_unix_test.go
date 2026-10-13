//go:build unix

package utils

import (
	"bytes"
	"context"
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
	stopping, stop := context.WithCancel(context.Background())
	stop() // its stop has begun
	DieOnEscalation(stopping)
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

// A path the command line gives is held to what a keploy CLI gives before the
// agent, root under sudo, writes there: the right name, in a directory that is
// not a symlink and that only its owner may write to. And every write goes
// through that directory as it was opened: a path swapped for a symlink
// afterwards does not move the writes.
func TestACommandLineProgressPathIsChecked(t *testing.T) {
	ok := filepath.Join(t.TempDir(), "keploy-agent-stop-ok")
	if err := os.Mkdir(ok, 0o700); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(ok, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		ok         bool
	}{
		{"a relative path", filepath.Join("rel", TeardownProgressFileName), false},
		{"another name", filepath.Join(ok, "shadow"), false},
		{"a directory others may write to", filepath.Join(open, TeardownProgressFileName), false},
		{"a directory that is a symlink", filepath.Join(link, TeardownProgressFileName), false},
		{"the CLI's", filepath.Join(ok, TeardownProgressFileName), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := openCommandLineSink(tc.path)
			if (err == nil) != tc.ok {
				t.Fatalf("openCommandLineSink(%q) = %v, want ok=%v", tc.path, err, tc.ok)
			}
		})
	}

	sink, err := openCommandLineSink(filepath.Join(ok, TeardownProgressFileName))
	if err != nil {
		t.Fatal(err)
	}
	// The path is moved and a symlink put in its place: the writes stay
	// in the directory that was opened.
	moved, elsewhere := ok+".moved", t.TempDir()
	if err := os.Rename(ok, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, ok); err != nil {
		t.Fatal(err)
	}
	if err := sink.write([]byte(`{"seq":1}`)); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(moved, TeardownProgressFileName)); err != nil || string(b) != `{"seq":1}` {
		t.Fatalf("the report is not in the directory that was opened: %q %v", b, err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("a write followed the symlink put in the directory's place: %v", entries)
	}
	if err := sink.clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, TeardownProgressFileName)); !os.IsNotExist(err) {
		t.Fatalf("clear left the report: %v", err)
	}
}

// TestEscalationWhileBusyHelper is not a test of its own: with
// KEPLOY_TEST_ESCALATION_BUSY set it is an agent in the middle of teardown
// work, which ends when the test writes "end" in that directory.
func TestEscalationWhileBusyHelper(t *testing.T) {
	dir := os.Getenv("KEPLOY_TEST_ESCALATION_BUSY")
	if dir == "" {
		return
	}
	stopping, stop := context.WithCancel(context.Background())
	stop() // its stop has begun
	DieOnEscalation(stopping)
	end := BeginTeardownWork()
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		os.Exit(3)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "end")); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	end()
	time.Sleep(time.Hour)
	os.Exit(0)
}

// The escalation signal does not kill an agent in the middle of teardown work
// -- a uprobe removal, a CA tool rewriting a trust store -- but once that work
// has ended: the CLI judged from a report a moment old.
func TestTheEscalationWaitsForTeardownWork(t *testing.T) {
	dir := t.TempDir()
	helper := exec.Command(os.Args[0], "-test.run=^TestEscalationWhileBusyHelper$")
	helper.Env = append(os.Environ(), "KEPLOY_TEST_ESCALATION_BUSY="+dir)
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
	select { // only that it has NOT died yet
	case err := <-exited:
		t.Fatalf("the escalation killed the agent in the middle of teardown work (%v)\n%s", err, out.String())
	case <-time.After(200 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(dir, "end"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Minute):
		t.Fatalf("the helper outlived the escalation once its work ended\n%s", out.String())
	}
	if st := helper.ProcessState.Sys().(syscall.WaitStatus); !st.Signaled() || st.Signal() != syscall.SIGKILL {
		t.Fatalf("the helper ended with %v, want it killed (SIGKILL)\n%s", helper.ProcessState, out.String())
	}
}

// TestEscalationBeforeTheStopHelper is not a test of its own: with
// KEPLOY_TEST_ESCALATION_EARLY set it is an agent whose stop has not begun;
// it marks "stopping" once its root context is cancelled, and waits.
func TestEscalationBeforeTheStopHelper(t *testing.T) {
	dir := os.Getenv("KEPLOY_TEST_ESCALATION_EARLY")
	if dir == "" {
		return
	}
	root, cancel := context.WithCancel(context.Background())
	SetCancel(cancel)
	DieOnEscalation(root)
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		os.Exit(3)
	}
	<-root.Done()
	if err := os.WriteFile(filepath.Join(dir, "stopping"), nil, 0o600); err != nil {
		os.Exit(3)
	}
	time.Sleep(time.Hour)
	os.Exit(0)
}

// Before an agent's stop has begun, the escalation signal begins it, as
// SIGTERM would: a stray one -- an operator who meant SIGUSR1 -- does not kill
// an agent with its CA installed. Once the stop has begun, it kills.
func TestTheEscalationBeforeTheStopBeginsIt(t *testing.T) {
	dir := t.TempDir()
	helper := exec.Command(os.Args[0], "-test.run=^TestEscalationBeforeTheStopHelper$")
	helper.Env = append(os.Environ(), "KEPLOY_TEST_ESCALATION_EARLY="+dir)
	var out bytes.Buffer
	helper.Stdout, helper.Stderr = &out, &out
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- helper.Wait() }()
	t.Cleanup(func() { _ = helper.Process.Kill() })
	await := func(name string) {
		t.Helper()
		hung := time.After(time.Minute) // only turns a hang into a failure
		for {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return
			}
			select {
			case err := <-exited:
				t.Fatalf("the helper ended before %q: %v\n%s", name, err, out.String())
			case <-hung:
				t.Fatalf("the helper never got to %q\n%s", name, out.String())
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	await("ready")
	if err := helper.Process.Signal(EscalationSignal); err != nil {
		t.Fatal(err)
	}
	await("stopping") // the signal began the stop, and killed nothing
	if err := helper.Process.Signal(EscalationSignal); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Minute):
		t.Fatalf("the helper outlived the escalation once its stop had begun\n%s", out.String())
	}
	if st := helper.ProcessState.Sys().(syscall.WaitStatus); !st.Signaled() || st.Signal() != syscall.SIGKILL {
		t.Fatalf("the helper ended with %v, want it killed (SIGKILL)\n%s", helper.ProcessState, out.String())
	}
}
