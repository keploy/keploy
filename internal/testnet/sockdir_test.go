package testnet

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A test's socket directory is not under GOTMPDIR, where its t.TempDir is,
// and is gone once the test ends.
func TestASocketDirIsOutsideTheTestsTempDirAndGoesWithTheTest(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	var dir string
	t.Run("a test", func(t *testing.T) {
		t.Setenv("GOTMPDIR", long)
		dir = SocketDir(t)
		if strings.HasPrefix(dir, long) {
			t.Errorf("the socket directory %s is under GOTMPDIR %s", dir, long)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("the socket directory %s while the test runs: %v, %v", dir, info, err)
		}
	})
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the socket directory %s once its test ended: %v; want it removed", dir, err)
	}
}
