package yaml

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// DeleteRecordingFileF says no ERROR for a file that is not there, and
// returns fs.ErrNotExist so its caller can count it as deleted; a file it
// finds and cannot delete is said at ERROR, naming the file, and is not
// fs.ErrNotExist. A recorder that deletes a batch of revoked test cases again,
// one by one, after the batch stopped part way must not print a failure for
// each test case the batch had deleted, nor hide one it could not.
func TestDeleteRecordingFileFSaysAnErrorOnlyForAFileStillThere(t *testing.T) {
	root := t.TempDir()
	set := filepath.Join(root, "set-1", "tests")
	// A directory in the file's place: removing a non-empty directory fails
	// with an error that is not fs.ErrNotExist.
	if err := os.MkdirAll(filepath.Join(set, "held.yaml", "x"), 0o755); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zapcore.DebugLevel)
	err := DeleteRecordingFileF(zap.New(core), root, set, "gone", FormatYAML)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("delete of a file that is not there: %v, want fs.ErrNotExist", err)
	}
	if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
		t.Fatalf("%d ERROR lines for a file that is not there: %v", n, logs.All())
	}

	core, logs = observer.New(zapcore.DebugLevel)
	err = DeleteRecordingFileF(zap.New(core), root, set, "held", FormatYAML)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("delete of a file it cannot delete: %v, want an error that is not fs.ErrNotExist", err)
	}
	said := logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessage("failed to delete the file").All()
	if len(said) != 1 {
		t.Fatalf("want one ERROR naming the file it could not delete, got %v", logs.All())
	}
	if f, _ := said[0].ContextMap()["file"].(string); f != filepath.Join(set, "held.yaml") {
		t.Fatalf("the ERROR names %q, want %q", f, filepath.Join(set, "held.yaml"))
	}
}
