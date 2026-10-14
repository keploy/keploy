//go:build linux

package starts

import (
	"os"
	"testing"
	"time"
)

// sysProc reads a process's parent and start from its /proc/<pid>/stat
// (utils.ReadProcStat): this process's, as the kernel reports them.
func TestSysProcReadsThisProcess(t *testing.T) {
	self := uint32(os.Getpid())
	if p, ok := (sysProc{}).Parent(self); !ok || p != uint32(os.Getppid()) {
		t.Fatalf("Parent(self) = %d, %v; want %d", p, ok, os.Getppid())
	}
	born, ok := (sysProc{}).Birth(self)
	if !ok {
		t.Fatal("Birth(self) could not be read")
	}
	// This test binary started a moment ago. Its start time is in clock
	// ticks, and boot() is the uptime's own reading, so allow a second each way.
	if age := time.Since(born); age < -time.Second || age > 5*time.Minute {
		t.Fatalf("Birth(self) = %s, %s ago; want this test binary's start", born, age)
	}
	if _, ok := (sysProc{}).Parent(1 << 30); ok {
		t.Fatal("Parent read a pid no process has")
	}
}
