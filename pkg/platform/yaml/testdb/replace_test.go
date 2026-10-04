package testdb

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

var errSharingViolation = errors.New("sharing violation")

// renameRefusingTempFiles is a platform that will not move upsert's temp file
// (a scanner holding it open) but renames anything else.
func renameRefusingTempFiles(from, to string) error {
	if strings.HasSuffix(from, ".tmp") {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: errSharingViolation}
	}
	return os.Rename(from, to)
}

// renameRefusingExistingTargets is a platform that will not rename over an
// existing file (a read-only target on Windows).
func renameRefusingExistingTargets(from, to string) error {
	if _, err := os.Stat(to); err == nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: errors.New("access denied")}
	}
	return os.Rename(from, to)
}

func useRename(t *testing.T, rename func(from, to string) error) {
	t.Helper()
	prev := replaceFile
	t.Cleanup(func() { replaceFile = prev })
	replaceFile = func(logger *zap.Logger, src, dst string) error {
		return yamlLib.ReplaceFileWith(logger, rename, src, dst)
	}
}

func testsDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	sort.Strings(names)
	return names
}

func namedTC(name, url string) *models.TestCase {
	tc := httpTC("GET", url)
	tc.Name = name
	return tc
}

// An update whose new file cannot be put in place leaves the test case it was
// updating as it was. upsert used to delete that file before the rename on
// Windows, so a rename that then failed lost the old test case and the new one.
func TestUpsert_AFailedReplaceKeepsTheExistingTestCase(t *testing.T) {
	for _, format := range []yamlLib.Format{yamlLib.FormatYAML, yamlLib.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			ts := NewWithFormat(zap.NewNop(), dir, format)
			if err := ts.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/old"), "set-1", false); err != nil {
				t.Fatal(err)
			}
			testsDir := filepath.Join(dir, "set-1", "tests")
			out := filepath.Join(testsDir, "t1."+format.FileExtension())
			orig, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}

			useRename(t, renameRefusingTempFiles)
			err = ts.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/new"), "set-1", false)
			if !errors.Is(err, errSharingViolation) {
				t.Fatalf("UpdateTestCase returned %v, want the refused rename", err)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("the test case is gone after a failed update: %v", err)
			}
			if !bytes.Equal(got, orig) {
				t.Fatal("a failed update changed the test case")
			}
			if names := testsDirEntries(t, testsDir); len(names) != 1 {
				t.Fatalf("a failed update left files beside the test case: %v", names)
			}
		})
	}
}

// A new test case whose file cannot be put in place leaves nothing behind:
// not its name's reservation, not its temp file, and tc.Name stays unset.
func TestUpsert_AFailedReplaceOfANewTestCaseLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	ts := New(zap.NewNop(), dir)
	useRename(t, renameRefusingTempFiles)
	tc := httpTC("GET", "http://api.test/users")
	if err := ts.InsertTestCase(t.Context(), tc, "set-1", false); !errors.Is(err, errSharingViolation) {
		t.Fatalf("InsertTestCase returned %v, want the refused rename", err)
	}
	if tc.Name != "" {
		t.Fatalf("tc.Name = %q after a failed insert, want it unset", tc.Name)
	}
	if names := testsDirEntries(t, filepath.Join(dir, "set-1", "tests")); len(names) != 0 {
		t.Fatalf("a failed insert left files behind: %v", names)
	}
}

// A platform that will not rename over an existing file still gets the
// updated test case, and nothing is left beside it.
func TestUpsert_ReplacesATestCaseTheRenameCannotReplace(t *testing.T) {
	dir := t.TempDir()
	ts := New(zap.NewNop(), dir)
	if err := ts.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/old"), "set-1", false); err != nil {
		t.Fatal(err)
	}
	useRename(t, renameRefusingExistingTargets)
	if err := ts.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/new"), "set-1", false); err != nil {
		t.Fatalf("UpdateTestCase: %v", err)
	}
	testsDir := filepath.Join(dir, "set-1", "tests")
	got, err := os.ReadFile(filepath.Join(testsDir, "t1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("http://api.test/new")) || bytes.Contains(got, []byte("http://api.test/old")) {
		t.Fatalf("the test case was not updated:\n%s", got)
	}
	if names := testsDirEntries(t, testsDir); len(names) != 1 {
		t.Fatalf("an update left files beside the test case: %v", names)
	}
}

// renameKeepingTheAsideCopy refuses to rename upsert's temp file over an
// existing file, and moves the existing file aside into a directory of the
// aside copy's name, which os.Remove then cannot delete: the replace puts the
// new test case in place but cannot remove the previous version, as on
// Windows when another process holds it open without sharing delete.
func renameKeepingTheAsideCopy(from, to string) error {
	if _, err := os.Stat(to); err == nil && strings.HasSuffix(from, ".tmp") {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: errors.New("access denied")}
	}
	if strings.Contains(filepath.Base(to), ".replaced.") {
		if err := os.Mkdir(to, 0o755); err != nil {
			return err
		}
		return os.Rename(from, filepath.Join(to, filepath.Base(from)))
	}
	return os.Rename(from, to)
}

// A replace that put the test case in place succeeded, even when the previous
// version it moved aside cannot be removed. For an auto-named case that
// previous version is claimName's placeholder: upsert took the error for a
// failed write and its placeholder cleanup deleted the new test case.
func TestUpsert_APreviousVersionLeftAsideIsNotAFailedWrite(t *testing.T) {
	t.Run("insert", func(t *testing.T) {
		dir := t.TempDir()
		ts := New(zap.NewNop(), dir)
		useRename(t, renameKeepingTheAsideCopy)
		tc := httpTC("GET", "http://api.test/users")
		if err := ts.InsertTestCase(t.Context(), tc, "set-1", false); err != nil {
			t.Fatalf("InsertTestCase: %v", err)
		}
		if tc.Name == "" {
			t.Fatal("tc.Name is unset after a successful insert")
		}
		got, err := os.ReadFile(filepath.Join(dir, "set-1", "tests", tc.Name+".yaml"))
		if err != nil {
			t.Fatalf("the inserted test case is gone: %v", err)
		}
		if !bytes.Contains(got, []byte("http://api.test/users")) {
			t.Fatalf("the inserted test case does not hold the request:\n%s", got)
		}
	})
	t.Run("update", func(t *testing.T) {
		dir := t.TempDir()
		ts := New(zap.NewNop(), dir)
		if err := ts.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/old"), "set-1", false); err != nil {
			t.Fatal(err)
		}
		useRename(t, renameKeepingTheAsideCopy)
		if err := ts.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/new"), "set-1", false); err != nil {
			t.Fatalf("UpdateTestCase: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(dir, "set-1", "tests", "t1.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(got, []byte("http://api.test/new")) {
			t.Fatalf("the test case was not updated:\n%s", got)
		}
	})
}
