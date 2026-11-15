package proxy

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.uber.org/zap"
)

// TestDiskMocksLeaveNoFileBehindEvenIfNeverClosed: the store's spill file has
// no name on disk from the moment it is made, so a process that dies before
// Close leaves nothing behind. The row reads a temp folder of its own: the
// shared one also holds the spill files other test processes are making at the
// same moment (go test runs packages side by side, and pkg/service/agent's
// rows make stores too), and a glob of it counted one of theirs.
func TestDiskMocksLeaveNoFileBehindEvenIfNeverClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows keeps an open file's name")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	d, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got := filepath.Dir(d.f.Name()); got != dir {
		t.Fatalf("the spill file was made in %s, not in the temp folder %s", got, dir)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		t.Errorf("a spill file name is left in the temp folder: %s", e.Name())
	}
}
