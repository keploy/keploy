package proxy

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.uber.org/zap"
)

func TestDiskMocksLeaveNoFileBehindEvenIfNeverClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows keeps an open file's name")
	}
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "keploy-diskmocks-*.gob"))
	d, err := NewDiskMocks(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "keploy-diskmocks-*.gob"))
	if len(after) != len(before) {
		t.Fatalf("a spill file name is left in %s", os.TempDir())
	}
}
