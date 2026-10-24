//go:build unix

package yaml

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"go.uber.org/zap"
)

// seedTestSet lays out root/test-set-0/tests/test-1.yaml, a folder that
// already holds a recording.
func seedTestSet(t *testing.T, root string) {
	t.Helper()
	tests := filepath.Join(root, "test-set-0", "tests")
	if err := os.MkdirAll(tests, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tests, "test-1.yaml"), []byte("kind: Http\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeUnlistable takes the read bit off dir (0300: writable and searchable,
// not listable) and restores it at cleanup so t.TempDir can remove it. A
// process that can still list the folder afterwards (root, or anything else
// holding CAP_DAC_OVERRIDE / CAP_DAC_READ_SEARCH) cannot be shown the
// permission error at all, so the test is skipped there -- the symlink-loop
// tests below cover the same fail-closed path without needing a non-root
// user.
func makeUnlistable(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if f, err := os.Open(dir); err == nil {
		_, rerr := f.Readdirnames(1)
		_ = f.Close()
		if rerr == nil || errors.Is(rerr, io.EOF) {
			t.Skipf("this process (euid %d) can still list a mode-0300 folder, so it cannot be made unlistable here", os.Geteuid())
		}
	}
}

// makeLoop makes path a symlink to itself: os.Stat on it fails with ELOOP,
// an error that is neither "not exist" nor "not a directory", for any user,
// root included.
func makeLoop(t *testing.T, path string) {
	t.Helper()
	if err := os.Symlink(path, path); err != nil {
		t.Fatal(err)
	}
}

type indexLister struct {
	name string
	list func(path string, mode IndexMode) ([]string, error)
}

func indexListers() []indexLister {
	ctx := context.Background()
	log := zap.NewNop()
	return []indexLister{
		{"ReadSessionIndicesF", func(p string, m IndexMode) ([]string, error) {
			return ReadSessionIndicesF(ctx, p, log, m, FormatYAML)
		}},
		{"ReadSessionIndicesAny", func(p string, m IndexMode) ([]string, error) {
			return ReadSessionIndicesAny(ctx, p, log, m)
		}},
	}
}

// A keploy folder that exists but cannot be listed is not an empty one.
// Reporting it empty made `keploy record` name its new test set test-set-0
// and write it into the user's existing test-set-0.
func TestReadSessionIndices_AnUnlistableFolderIsAnError(t *testing.T) {
	for _, l := range indexListers() {
		t.Run(l.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "keploy")
			seedTestSet(t, root)
			makeUnlistable(t, root)
			ids, err := l.list(root, ModeDir)
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("listing an unlistable folder = %q, %v; want the permission error", ids, err)
			}
			if len(ids) != 0 {
				t.Fatalf("listing failed but returned ids %q", ids)
			}
		})
	}
}

// Any open error other than "not exist" fails the listing: here ELOOP, which
// every user (root too) gets.
func TestReadSessionIndices_AnUnopenableFolderIsAnError(t *testing.T) {
	for _, l := range indexListers() {
		t.Run(l.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "keploy")
			makeLoop(t, root)
			ids, err := l.list(root, ModeDir)
			if !errors.Is(err, syscall.ELOOP) {
				t.Fatalf("listing a symlink loop = %q, %v; want ELOOP", ids, err)
			}
		})
	}
}

// A folder that does not exist yet -- the first recording -- is still empty,
// not an error, whether the folder or its parent is missing.
func TestReadSessionIndices_AMissingFolderIsEmpty(t *testing.T) {
	for _, l := range indexListers() {
		t.Run(l.name, func(t *testing.T) {
			base := t.TempDir()
			for _, p := range []string{
				filepath.Join(base, "keploy"),
				filepath.Join(base, "missing", "keploy", "reports"),
			} {
				for _, m := range []IndexMode{ModeDir, ModeFile} {
					ids, err := l.list(p, m)
					if err != nil || len(ids) != 0 {
						t.Fatalf("listing missing %s (mode %v) = %q, %v; want empty, nil", p, m, ids, err)
					}
				}
			}
		})
	}
}

// A test set that is a symbolic link fails the listing with an error that
// names the link and says to replace it with a real folder: keploy writes
// into test sets, often under sudo, and a link would have it write wherever
// the link points. It used to be passed over without a word, so the next
// test set id could be the link's own name and keploy record wrote into the
// set it points to. A link that cannot be followed is refused too.
func TestReadSessionIndices_ASymlinkedTestSetIsRefused(t *testing.T) {
	for _, l := range indexListers() {
		for _, how := range []string{"to a folder", "target missing", "loop"} {
			t.Run(l.name+"/"+how, func(t *testing.T) {
				base := t.TempDir()
				root := filepath.Join(base, "keploy")
				seedTestSet(t, root)
				link := filepath.Join(root, "test-set-1")
				switch how {
				case "to a folder":
					seedTestSet(t, filepath.Join(base, "elsewhere"))
					if err := os.Symlink(filepath.Join(base, "elsewhere", "test-set-0"), link); err != nil {
						t.Fatal(err)
					}
				case "target missing":
					if err := os.Symlink(filepath.Join(base, "gone"), link); err != nil {
						t.Fatal(err)
					}
				case "loop":
					makeLoop(t, link)
				}
				ids, err := l.list(root, ModeDir)
				if err == nil || !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), "replace the link with a real folder") {
					t.Fatalf("listing a keploy folder holding a symlinked test set = %q, %v; want an error naming %s and saying to replace it with a real folder", ids, err, link)
				}
				if len(ids) != 0 {
					t.Fatalf("listing failed but returned ids %q", ids)
				}
			})
		}
	}
}

// A symbolic link to a file is no test set, like a file, and fails nothing;
// nor does a link in the place of a folder the listing passes over by name
// (the reports folder).
func TestReadSessionIndices_ALinkThatIsNoTestSetIsPassedOver(t *testing.T) {
	for _, l := range indexListers() {
		t.Run(l.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "keploy")
			seedTestSet(t, root)
			notes := filepath.Join(base, "notes.txt")
			if err := os.WriteFile(notes, []byte("not a set\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(notes, filepath.Join(root, "notes.txt")); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(base, "shared-reports"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(base, "shared-reports"), filepath.Join(root, FolderReports)); err != nil {
				t.Fatal(err)
			}
			ids, err := l.list(root, ModeDir)
			if err != nil || len(ids) != 1 || ids[0] != "test-set-0" {
				t.Fatalf("listing = %q, %v; want [test-set-0], nil", ids, err)
			}
		})
	}
}

// The next test-N index is read the same way: a tests folder that cannot be
// listed must not read as "no tests yet" (index 1).
func TestFindLastIndexAny_AnUnlistableFolderIsAnError(t *testing.T) {
	root := t.TempDir()
	seedTestSet(t, root)
	tests := filepath.Join(root, "test-set-0", "tests")
	makeUnlistable(t, tests)
	if idx, err := FindLastIndexAny(tests, zap.NewNop()); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("FindLastIndexAny on an unlistable folder = %d, %v; want the permission error", idx, err)
	}
}

func TestFindLastIndexAny_AnUnopenableFolderIsAnError(t *testing.T) {
	tests := filepath.Join(t.TempDir(), "tests")
	makeLoop(t, tests)
	if idx, err := FindLastIndexAny(tests, zap.NewNop()); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("FindLastIndexAny on a symlink loop = %d, %v; want ELOOP", idx, err)
	}
}

func TestFindLastIndexAny_AMissingFolderStartsAtOne(t *testing.T) {
	tests := filepath.Join(t.TempDir(), "test-set-0", "tests")
	if idx, err := FindLastIndexAny(tests, zap.NewNop()); err != nil || idx != 1 {
		t.Fatalf("FindLastIndexAny on a missing folder = %d, %v; want 1, nil", idx, err)
	}
	seedTestSet(t, filepath.Dir(filepath.Dir(tests)))
	if idx, err := FindLastIndexAny(tests, zap.NewNop()); err != nil || idx != 2 {
		t.Fatalf("FindLastIndexAny after test-1 = %d, %v; want 2, nil", idx, err)
	}
}
