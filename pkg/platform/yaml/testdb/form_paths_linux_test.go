package testdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
	yaml "go.keploy.io/server/v3/pkg/platform/yaml"
)

// A copy of a capture's file whose read fails names the copy's source in its
// error (read <source>: ...), and the source ends in the part's file name,
// the app's: the line and the error name it by its folder. The copy's
// destination is named the same way (a copy that fails on its write side,
// write <destination>: copy_file_range: ..., as a folder at the source
// makes it: TestAFormPartsFileNameIsNamedByItsSize). The read here fails
// deterministically: the source links to /proc/self/mem, which
// copy_file_range declines, and whose first page no process maps, so the
// read at its start fails with EIO.
func TestACopysFailedReadNamesItsSourceByItsFolder(t *testing.T) {
	const s = logleak.Canary
	name := "cv-" + s + ".pdf"
	root := filepath.Join(t.TempDir(), "keploy")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	logger, logs := logleak.Observe()
	ts := NewWithFormat(logger, root, yaml.FormatJSON)
	src := filepath.Join(t.TempDir(), "keploy_3_"+name)
	if err := os.Symlink("/proc/self/mem", src); err != nil {
		t.Fatal(err)
	}
	err := ts.InsertTestCase(context.Background(), formCase("test-6", models.FormData{Key: "file", FileNames: []string{name}, Paths: []string{src}, Captured: []string{src}}), "set", false)
	said := logs.FilterMessage("failed to copy asset file").All()
	if len(said) != 1 || err == nil || !strings.HasPrefix(models.FormFilePathsInError(err).Error(), "read ") {
		t.Fatalf("precondition: the copy's read did not fail and log (insert = %v, lines %+v)", err, said)
	}
	if logleak.Holds(err.Error(), s) {
		t.Errorf("insert = %v, want the copy's failure without the file's name", err)
	}
	logleak.Check(t, logs, s, "the test store's failed copy")
}
