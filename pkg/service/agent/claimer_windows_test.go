//go:build windows

package agent

import (
	"io"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

// envClaimerHold makes this test binary, run again as a child, hold until
// its stdin closes.
const envClaimerHold = "KEPLOY_TEST_CLAIMER_HOLD_UNTIL_STDIN_CLOSES"

// Not a test of its own: run as the child of the tests below, it holds until
// its stdin closes.
func TestWindowsClaimerChildHoldsUntilStdinCloses(t *testing.T) {
	if os.Getenv(envClaimerHold) != "1" {
		t.Skip("only run as a child of this package's tests")
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// startHolder starts this test binary again as a child that holds until its
// stdin closes, and ends it in t.Cleanup. It starts no process of its own.
func startHolder(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWindowsClaimerChildHoldsUntilStdinCloses$")
	cmd.Env = append(os.Environ(), envClaimerHold+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the child: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// This process has a creation time, is alive at it and at no other, and a
// pid no process has is not alive.
func TestWindowsProcStartReadsALiveProcess(t *testing.T) {
	start, ok := procStart(os.Getpid())
	if !ok || start == 0 {
		t.Fatalf("procStart(this process) = %d, %v", start, ok)
	}
	if !procAlive(os.Getpid(), start) {
		t.Fatal("this process is not alive at its own creation time")
	}
	if procAlive(os.Getpid(), start+1) {
		t.Fatal("this process is alive at another creation time")
	}
	// A pid far above any the system hands out.
	if _, ok := procStart(0x7ffffffc); ok {
		t.Fatal("a pid no process has is alive")
	}
	if _, ok := procStart(0); ok {
		t.Fatal("pid 0 is alive")
	}
}

// Windows process IDs are 32 bits wide: a pid wider than that names no
// process, not the one its low 32 bits name (this process, here).
func TestWindowsProcStartTakesAPidWiderThan32BitsForNone(t *testing.T) {
	wide := int64(os.Getpid()) + 1<<32
	if int64(int(wide)) != wide {
		t.Skip("int is 32 bits wide here: no pid is wider")
	}
	if _, ok := procStart(os.Getpid()); !ok {
		t.Fatal("this process is not alive")
	}
	if start, ok := procStart(int(wide)); ok {
		t.Fatalf("procStart(%d) = %d, true: read as this process %d", wide, start, os.Getpid())
	}
}

// A process that has exited is not alive, even while a handle another
// process holds keeps its object, and so its pid and creation time, readable.
func TestWindowsProcStartTakesAnExitedProcessForGone(t *testing.T) {
	child := startHolder(t)
	pid := child.Process.Pid
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("open the child: %v", err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	start, ok := procStart(pid)
	if !ok {
		t.Fatal("the running child is not alive")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill the child: %v", err)
	}
	if ev, err := windows.WaitForSingleObject(h, 10_000); err != nil || ev != windows.WAIT_OBJECT_0 {
		t.Fatalf("the child did not exit: %d, %v", ev, err)
	}
	if _, ok := procStart(pid); ok {
		t.Fatal("the exited child is alive")
	}
	if procAlive(pid, start) {
		t.Fatal("the exited child is alive at its creation time")
	}
}

// A child of this process descends from it, and this process does not
// descend from its child.
func TestWindowsProcDescendsForAChild(t *testing.T) {
	child := startHolder(t)
	if !procDescends(child.Process.Pid, os.Getpid()) {
		t.Fatalf("procDescends(child %d, this process %d) = false", child.Process.Pid, os.Getpid())
	}
	if procDescends(os.Getpid(), child.Process.Pid) {
		t.Fatalf("procDescends(this process %d, its child %d) = true", os.Getpid(), child.Process.Pid)
	}
}
