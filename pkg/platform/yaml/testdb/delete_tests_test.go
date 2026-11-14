package testdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func writeTestCaseFiles(t *testing.T, root, set string, names ...string) string {
	t.Helper()
	dir := filepath.Join(root, set, "tests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n+".yaml"), []byte("kind: Http\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func onDisk(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name+".yaml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// A test case already gone is deleted: the call goes on to the next, says no
// ERROR, and returns no error. A recorder that asks again, one by one, for
// test cases a batch deleted before it failed part way must not read them as
// failures, nor print a failure for each.
func TestDeleteTestsGoesOnPastATestCaseAlreadyGone(t *testing.T) {
	root := t.TempDir()
	dir := writeTestCaseFiles(t, root, "set-1", "test-1", "test-3")
	core, logs := observer.New(zapcore.DebugLevel)
	ts := New(zap.New(core), root)

	if err := ts.DeleteTests(context.Background(), "set-1", []string{"test-1", "test-2", "test-3"}); err != nil {
		t.Fatalf("DeleteTests with test-2 already gone: %v, want nil", err)
	}
	if onDisk(t, dir, "test-1") || onDisk(t, dir, "test-3") {
		t.Fatal("a test case after the one already gone was not deleted")
	}
	if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
		t.Fatalf("%d ERROR lines for a test case already gone: %v", n, logs.FilterLevelExact(zapcore.ErrorLevel).All())
	}
	if logs.FilterMessage("the testcase to delete is already gone").Len() != 1 {
		t.Fatalf("want one Debug line naming the test case already gone, got %v", logs.All())
	}
}

// A test case that cannot be deleted is still an error, said at ERROR, and the
// call stops there.
func TestDeleteTestsStopsAtATestCaseItCannotDelete(t *testing.T) {
	root := t.TempDir()
	dir := writeTestCaseFiles(t, root, "set-1", "test-1", "test-3")
	// A directory in a test case file's place: os.Remove of a non-empty
	// directory fails with an error that is not fs.ErrNotExist.
	if err := os.MkdirAll(filepath.Join(dir, "test-2.yaml", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zapcore.DebugLevel)
	ts := New(zap.New(core), root)

	err := ts.DeleteTests(context.Background(), "set-1", []string{"test-1", "test-2", "test-3"})
	if err == nil {
		t.Fatal("DeleteTests of a test case it cannot delete returned nil")
	}
	if onDisk(t, dir, "test-1") {
		t.Fatal("the test case before the one it cannot delete is still there")
	}
	if !onDisk(t, dir, "test-3") {
		t.Fatal("the call went on past a test case it could not delete")
	}
	if logs.FilterLevelExact(zapcore.ErrorLevel).Len() == 0 {
		t.Fatal("no ERROR for a test case that could not be deleted")
	}
}
