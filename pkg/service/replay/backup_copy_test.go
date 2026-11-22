package replay

import (
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/config"
)

// TestCopyDirContentsRefusesADirectoryItCannotRead keeps the copy honest about
// the error the caller acts on: a set it cannot read fails the backup rather
// than returning an empty backup that looks like success.
func TestCopyDirContentsRefusesADirectoryItCannotRead(t *testing.T) {
	root := t.TempDir()
	r := &Replayer{config: &config.Config{Path: root}}
	if err := r.copyDirContents(filepath.Join(root, "gone"), filepath.Join(root, "copy")); err == nil {
		t.Fatal("a copy of a directory that does not exist returned no error")
	}
}
