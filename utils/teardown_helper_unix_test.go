//go:build unix

package utils

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A HELPER PROCESS NEVER OUTLIVES THE TEST BINARY THAT STARTED IT.
//
// The escalation tests run this test binary again as an agent (a helper) that
// waits to be killed, and stand-ins that run as long as a stage of teardown
// work. A test binary killed mid-test -- a go test timeout, a job slot's
// memory cap -- runs no cleanup, and they outlived it: reparented to init,
// sleeping for an hour or spinning on a CPU. So a helper runs in a process
// group of its own, which the test's end kills; the kernel kills it when the
// thread that started it ends (Pdeathsig, on Linux); and it exits once its
// stdin, a pipe the test binary holds open, reads end of file
// (helperExitsWithParent). A stand-in's loop ends once its parent has.

// lockedBuffer is a helper's output: written by the copy from its pipes,
// read by a test that fails.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// startHelper runs this test binary's test name with env set, as a helper:
// its output goes to the returned buffer, and exited receives its end.
func startHelper(t *testing.T, name, env string) (helper *exec.Cmd, exited <-chan error, out *lockedBuffer) {
	t.Helper()
	// Pdeathsig follows the thread that started the helper, not the process:
	// the test keeps to this thread until it ends.
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	helper = exec.Command(os.Args[0], "-test.run=^"+name+"$")
	helper.Env = append(os.Environ(), env)
	out = &lockedBuffer{}
	helper.Stdout, helper.Stderr = out, out
	helper.SysProcAttr = helperProcAttr()
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	reaped := make(chan struct{})
	go func() { ended <- helper.Wait(); close(reaped) }()
	t.Cleanup(func() {
		select {
		case <-reaped:
		default: // not reaped: its process group is still its own
			_ = syscall.Kill(-helper.Process.Pid, syscall.SIGKILL)
			<-reaped
		}
		_ = stdin.Close()
	})
	return helper, ended, out
}

// helperExitsWithParent has a helper exit once its stdin, a pipe the test
// binary that started it holds open (startHelper), reads end of file: that
// binary is gone. Every helper calls it first.
func helperExitsWithParent() {
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(5)
	}()
}

// awaitHelperFile returns once the helper has written name in dir, failing
// the test if the helper ends first.
func awaitHelperFile(t *testing.T, dir, name string, exited <-chan error, out *lockedBuffer) {
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

// busyChild is a stand-in for a CA tool a stage of teardown work runs: always
// running, or waiting to run, until its parent -- the test binary -- is gone.
func busyChild() *exec.Cmd {
	return exec.Command("sh", "-c", `while kill -0 "$PPID" 2>/dev/null; do :; done`)
}

const helperOrphanEnv = "KEPLOY_TEST_HELPER_ORPHAN"

// TestHelperOrphanParent is not a test of its own: with
// KEPLOY_TEST_HELPER_ORPHAN set to a directory it is a test binary that
// starts, in that directory, every helper and stand-in of this package's
// teardown tests, writes their pids to its file "pids" and then "up", and
// waits to be killed. It is a helper itself (startHelper): it ends with the
// test that started it.
func TestHelperOrphanParent(t *testing.T) {
	dir := os.Getenv(helperOrphanEnv)
	if dir == "" {
		return
	}
	helperExitsWithParent()
	var pids strings.Builder
	for i, h := range orphanHelpers() {
		hdir := filepath.Join(dir, strconv.Itoa(i))
		if err := os.Mkdir(hdir, 0o700); err != nil {
			t.Fatal(err)
		}
		helper, exited, out := startHelper(t, h.name, h.env+"="+hdir)
		awaitHelperFile(t, hdir, "ready", exited, out)
		fmt.Fprintf(&pids, "%d %s\n", helper.Process.Pid, h.name)
	}
	child := busyChild()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&pids, "%d busyChild\n", child.Process.Pid)
	if err := os.WriteFile(filepath.Join(dir, "pids"), []byte(pids.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "up"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {} // killed here, mid-test: no cleanup runs
}

// No helper or stand-in of the teardown tests outlives a test binary killed
// mid-test: one that did slept for an hour, or spun on a CPU, reparented to
// init, holding the test box's job slots.
func TestNoTeardownHelperOutlivesItsTest(t *testing.T) {
	dir := t.TempDir() // the parent's: removed here, as the parent is killed
	parent, exited, out := startHelper(t, "TestHelperOrphanParent", helperOrphanEnv+"="+dir)
	awaitHelperFile(t, dir, "up", exited, out)
	b, err := os.ReadFile(filepath.Join(dir, "pids"))
	if err != nil {
		t.Fatal(err)
	}
	pids := map[int]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var pid int
		var name string
		if _, err := fmt.Sscanf(line, "%d %s", &pid, &name); err == nil {
			pids[pid] = name
		}
	}
	if len(pids) != len(orphanHelpers())+1 {
		t.Fatalf("the parent wrote %d pids, want %d: %s", len(pids), len(orphanHelpers())+1, b)
	}
	if err := parent.Process.Kill(); err != nil { // SIGKILL, mid-test
		t.Fatal(err)
	}
	<-exited
	awaitGone(t, pids)
}

// awaitGone returns once every process in pids has ended, failing the test --
// and ending, itself, each that has not -- if one outlives a bound that only
// turns a leak into a failure.
func awaitGone(t *testing.T, pids map[int]string) {
	t.Helper()
	hung := time.After(time.Minute)
	for pid, name := range pids {
		for processAlive(pid) {
			select {
			case <-hung:
				for p := range pids {
					if processAlive(p) { // this test's own leaks, not a pid reused
						_ = syscall.Kill(p, syscall.SIGKILL)
					}
				}
				t.Fatalf("%s (pid %d) outlived the test binary that started it", name, pid)
			case <-time.After(5 * time.Millisecond):
			}
		}
		delete(pids, pid)
	}
}

// processAlive reports whether process pid still runs: it exists and, where
// /proc tells, is not a zombie -- which a PID 1 that reaps nothing would keep
// for ever.
func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		return true // no /proc here: kill alone tells
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := bytes.LastIndexByte(b, ')')
	f := bytes.Fields(b[i+1:])
	return i >= 0 && len(f) > 0 && f[0][0] != 'Z' && f[0][0] != 'X'
}

// unixOrphanHelpers is the helpers of teardown_unix_test.go, by the test that
// is each and the variable that gives it its directory.
func unixOrphanHelpers() []struct{ name, env string } {
	return []struct{ name, env string }{
		{"TestEscalationHelper", "KEPLOY_TEST_ESCALATION"},
		{"TestEscalationWhileBusyHelper", "KEPLOY_TEST_ESCALATION_BUSY"},
		{"TestEscalationBeforeTheStopHelper", "KEPLOY_TEST_ESCALATION_EARLY"},
	}
}
