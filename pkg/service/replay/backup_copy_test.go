package replay

import (
	"path/filepath"
	"testing"
)

// TestCopyDirContentsRefusesADirectoryItCannotRead keeps the copy honest about
// the error the caller acts on: a set it cannot read fails the backup rather
// than returning an empty backup that looks like success.
func TestCopyDirContentsRefusesADirectoryItCannotRead(t *testing.T) {
	r := &Replayer{}
	if err := r.copyDirContents(filepath.Join(t.TempDir(), "gone"), t.TempDir()); err == nil {
		t.Fatal("a copy of a directory that does not exist returned no error")
	}
}
