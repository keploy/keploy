package yaml

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// replaceFixture writes dst ("old") and src ("new") into a fresh dir.
func replaceFixture(t *testing.T) (dir, src, dst string) {
	t.Helper()
	dir = t.TempDir()
	dst = filepath.Join(dir, "mocks.yaml")
	src = filepath.Join(dir, "mocks.123.tmp")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, src, dst
}

func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	sort.Strings(out)
	return out
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return string(b)
}

var errRefused = errors.New("refused")

// A platform that will not rename over an existing file (Windows) still gets
// the new file in place, and no aside copy is left behind.
func TestReplaceFile_RenameOverAnExistingFileRefused(t *testing.T) {
	dir, src, dst := replaceFixture(t)
	rename := func(from, to string) error {
		if _, err := os.Stat(to); err == nil {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errRefused}
		}
		return os.Rename(from, to)
	}
	if err := ReplaceFileWith(zap.NewNop(), rename, src, dst); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := mustRead(t, dst); got != "new" {
		t.Fatalf("dst holds %q, want the new file", got)
	}
	if names := filesIn(t, dir); len(names) != 1 {
		t.Fatalf("files left beside the replaced one: %v", names)
	}
}

// When the new file cannot be moved at all (a scanner holding it open), the
// original must still be in place afterwards. The fallback used to delete it
// before retrying, and lost both files when the retry failed too.
func TestReplaceFile_NewFileCannotBeMovedKeepsTheOriginal(t *testing.T) {
	dir, src, dst := replaceFixture(t)
	rename := func(from, to string) error {
		if from == src {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errRefused}
		}
		return os.Rename(from, to)
	}
	if err := ReplaceFileWith(zap.NewNop(), rename, src, dst); err == nil {
		t.Fatal("replace succeeded though the new file could not be moved")
	}
	if got := mustRead(t, dst); got != "old" {
		t.Fatalf("dst holds %q after a failed replace, want the original", got)
	}
	if got := mustRead(t, src); got != "new" {
		t.Fatalf("src holds %q, want it untouched for its writer to clean up", got)
	}
	for _, n := range filesIn(t, dir) {
		if strings.Contains(n, ".replaced.") {
			t.Fatalf("a failed replace left the aside copy %s", n)
		}
	}
}

// With no file to replace, a failed rename is just that failure.
func TestReplaceFile_NoTargetReturnsTheRenameError(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new.tmp")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	rename := func(from, to string) error {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: errRefused}
	}
	err := ReplaceFileWith(zap.NewNop(), rename, src, filepath.Join(dir, "mocks.yaml"))
	if !errors.Is(err, errRefused) {
		t.Fatalf("want the rename error, got %v", err)
	}
	if names := filesIn(t, dir); len(names) != 1 || names[0] != "new.tmp" {
		t.Fatalf("unexpected files: %v", names)
	}
}

func TestReplaceFile_Replaces(t *testing.T) {
	dir, src, dst := replaceFixture(t)
	if err := ReplaceFile(zap.NewNop(), src, dst); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := mustRead(t, dst); got != "new" {
		t.Fatalf("dst holds %q, want the new file", got)
	}
	if names := filesIn(t, dir); len(names) != 1 {
		t.Fatalf("files left beside the replaced one: %v", names)
	}
}

// renameKeepingTheAsideCopy renames as the platform does, except that it moves
// the target aside into a directory of that name, which os.Remove then cannot
// delete: the aside copy stays, as it does on Windows when another process
// holds the file open without sharing delete. refuse picks the renames the
// platform refuses.
func renameKeepingTheAsideCopy(refuse func(from, to string) bool) func(from, to string) error {
	return func(from, to string) error {
		if refuse(from, to) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: errRefused}
		}
		if strings.Contains(filepath.Base(to), ".replaced.") {
			if err := os.Mkdir(to, 0o755); err != nil {
				return err
			}
			return os.Rename(from, filepath.Join(to, filepath.Base(from)))
		}
		return os.Rename(from, to)
	}
}

// A replace that put the new file in place has replaced it, even when the
// previous version it moved aside cannot be removed: it says so in a warning
// naming that copy, not as a failure. Returning an error there told callers
// nothing was written, and a caller that then cleaned up deleted the new file.
func TestReplaceFile_AnAsideCopyThatCannotBeRemovedIsNotAFailure(t *testing.T) {
	dir, src, dst := replaceFixture(t)
	refuseOverExisting := func(from, to string) bool {
		_, err := os.Stat(to)
		return from == src && err == nil
	}
	core, logs := observer.New(zapcore.WarnLevel)
	if err := ReplaceFileWith(zap.New(core), renameKeepingTheAsideCopy(refuseOverExisting), src, dst); err != nil {
		t.Fatalf("replace returned %v though the new file is in place", err)
	}
	if got := mustRead(t, dst); got != "new" {
		t.Fatalf("dst holds %q, want the new file", got)
	}
	var aside string
	for _, n := range filesIn(t, dir) {
		if strings.Contains(n, ".replaced.") {
			aside = filepath.Join(dir, n)
		}
	}
	if aside == "" {
		t.Fatalf("the aside copy is not there to warn about: %v", filesIn(t, dir))
	}
	warned := false
	for _, e := range logs.All() {
		if e.Level == zapcore.WarnLevel && e.ContextMap()["previousVersion"] == aside {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning names the previous version left at %s; logged: %v", aside, logs.All())
	}
}
