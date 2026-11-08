//go:build !windows

package record

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The folder a record's packet capture is streamed to is the test set's, made
// only inside the project: through a keploy folder the repository carries as
// a link out of the project, it is refused with why, and no folder is made
// where the link leads. One inside the project is made as before.
func TestPcapDirIsMadeOnlyInsideTheProject(t *testing.T) {
	project, outside := t.TempDir(), t.TempDir()
	keployDir := filepath.Join(project, "keploy")
	if err := os.Symlink(outside, keployDir); err != nil {
		t.Fatal(err)
	}
	if dir, err := pcapDir(keployDir, "test-set-0"); err == nil {
		t.Errorf("pcapDir through a keploy folder that links out = %s; want it refused", dir)
	}
	if _, err := os.Lstat(filepath.Join(outside, "test-set-0")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a folder was made where the keploy link leads: %v", err)
	}

	inside := filepath.Join(t.TempDir(), "keploy")
	dir, err := pcapDir(inside, "test-set-0")
	if err != nil || dir != filepath.Join(inside, "test-set-0") {
		t.Fatalf("pcapDir = %s, %v", dir, err)
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		t.Errorf("the test set's folder was not made: %v", err)
	}
}
