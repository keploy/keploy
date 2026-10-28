package learn

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A zone file that is no regular file (a FIFO, which would hold a read
// until something writes it) is an error, never read: the agent reads a
// process's zone on its connection's path.
func TestAZoneFileThatIsNoRegularFileIsNotRead(t *testing.T) {
	proc := t.TempDir()
	fakeProc(t, proc, "700", "500", nil, map[string][]byte{"etc/hostname": []byte("app\n")}, nil)
	if err := syscall.Mkfifo(filepath.Join(proc, "700", "root", "etc", "localtime"), 0o644); err != nil {
		t.Skipf("no FIFO: %v", err)
	}
	if z, err := ReadProcZone(proc, 700); err == nil || z.Loc != nil {
		t.Fatalf("a FIFO /etc/localtime: %v %v; want an error and no zone", z.Loc, err)
	}
	if _, err := os.Stat(filepath.Join(proc, "700", "root", "etc", "localtime")); err != nil {
		t.Fatal(err)
	}
}
