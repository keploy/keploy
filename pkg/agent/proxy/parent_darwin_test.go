//go:build darwin

package proxy

import (
	"os"
	"os/exec"
	"testing"
)

// On macOS a process's parent is read from the kernel's process table: a
// process this test starts has this test for its parent, so the chain a call
// of it is scoped by runs through this test's process and on to its parent.
func TestParentOfReadsMacOSsProcessTable(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	child, self := uint32(cmd.Process.Pid), uint32(os.Getpid())
	if ppid, ok := parentOf(child); !ok || ppid != self {
		t.Fatalf("the parent of a process this test started is %d (%v), want %d", ppid, ok, self)
	}
	if chain := ancestorChain(child); len(chain) < 2 || chain[0] != child || chain[1] != self {
		t.Fatalf("the chain of a process this test started is %v, want %d, %d first", chain, child, self)
	}
}
