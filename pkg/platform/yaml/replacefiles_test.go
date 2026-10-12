package yaml

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// groupFixture is a directory with a.yaml ("a-old") and b.yaml ("b-old"),
// and the group that replaces both and creates c.yaml.
func groupFixture(t *testing.T) (dir string, files []File) {
	t.Helper()
	dir = t.TempDir()
	for name, text := range map[string]string{"a.yaml": "a-old", "b.yaml": "b-old"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, []File{
		{Path: filepath.Join(dir, "a.yaml"), Data: []byte("a-new")},
		{Path: filepath.Join(dir, "b.yaml"), Data: []byte("b-new")},
		{Path: filepath.Join(dir, "c.yaml"), Data: []byte("c-new")},
	}
}

// refusing is a platform that will not rename any file whose name ends in
// one of suffixes.
func refusing(suffixes ...string) func(src, dst string) error {
	return func(src, dst string) error {
		for _, s := range suffixes {
			if strings.HasSuffix(src, s) {
				return &os.LinkError{Op: "rename", Old: src, New: dst, Err: errRefused}
			}
		}
		return os.Rename(src, dst)
	}
}

func mustEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %q, want %q", what, got, want)
	}
}

// Every file of the group is in place, a file that did not exist is created,
// each file keeps its mode, and nothing is left beside them.
func TestReplaceFiles_PutsEveryFileInPlace(t *testing.T) {
	dir, files := groupFixture(t)
	if err := ReplaceFiles(zap.NewNop(), files); err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "files", filesIn(t, dir), []string{"a.yaml", "b.yaml", "c.yaml"})
	mustEqual(t, "a.yaml", mustRead(t, files[0].Path), "a-new")
	mustEqual(t, "b.yaml", mustRead(t, files[1].Path), "b-new")
	mustEqual(t, "c.yaml", mustRead(t, files[2].Path), "c-new")
	info, err := os.Stat(files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("a.yaml's mode = %v, want the 0600 it had", info.Mode().Perm())
	}
	if left, err := FindLeftovers(dir); err != nil || len(left.Files) != 0 {
		t.Fatalf("left behind: %v, %v", left.Files, err)
	}
}

// ALL OR NONE. Whichever file cannot be put in place, the first, one in the
// middle or the last, every file is as it was: those already replaced are put
// back, one that did not exist is not there, and nothing is left beside them.
func TestReplaceFiles_AFileThatCannotBePutInPlaceLeavesEveryFileAsItWas(t *testing.T) {
	for _, name := range []string{"a.yaml", "b.yaml", "c.yaml"} {
		t.Run(name, func(t *testing.T) {
			dir, files := groupFixture(t)
			err := ReplaceFilesWith(zap.NewNop(), ReplaceHooks{Rename: refusing(name + ReplaceNewSuffix)}, files)
			if err == nil {
				t.Fatal("no error")
			}
			if want := "failed to put " + name + " in place, so every file was put back as it was"; !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q, want it to say %q", err, want)
			}
			if !errors.Is(err, errRefused) {
				t.Fatalf("error %q does not carry what refused the file", err)
			}
			mustEqual(t, "files", filesIn(t, dir), []string{"a.yaml", "b.yaml"})
			mustEqual(t, "a.yaml", mustRead(t, files[0].Path), "a-old")
			mustEqual(t, "b.yaml", mustRead(t, files[1].Path), "b-old")
		})
	}
}

// A file created by the group is removed again when a later file cannot be
// put in place.
func TestReplaceFiles_AFileTheGroupCreatedIsRemovedWithIt(t *testing.T) {
	dir, files := groupFixture(t)
	// c.yaml, which does not exist, first: it is in place when b.yaml refuses.
	files = []File{files[2], files[0], files[1]}
	if err := ReplaceFilesWith(zap.NewNop(), ReplaceHooks{Rename: refusing("b.yaml" + ReplaceNewSuffix)}, files); err == nil {
		t.Fatal("no error")
	}
	mustEqual(t, "files", filesIn(t, dir), []string{"a.yaml", "b.yaml"})
	mustEqual(t, "a.yaml", mustRead(t, filepath.Join(dir, "a.yaml")), "a-old")
}

// A file that cannot be written beside its target stops the group before any
// target is touched, and what the group had written is removed. A file an
// interrupted group left under one of its names (new bytes, a copy, a copy
// in progress, a marker) is never written over.
func TestReplaceFiles_AFileThatCannotBeStagedTouchesNothing(t *testing.T) {
	for _, left := range []string{"b.yaml" + ReplaceNewSuffix, "b.yaml" + ReplaceOldSuffix, "b.yaml" + ReplacePartSuffix, "c.yaml" + ReplaceAbsentSuffix} {
		t.Run(left, func(t *testing.T) {
			dir, files := groupFixture(t)
			leftPath := filepath.Join(dir, left)
			if err := os.WriteFile(leftPath, []byte("left by a killed run"), 0o644); err != nil {
				t.Fatal(err)
			}
			target := strings.SplitN(left, ".keploy-", 2)[0]
			err := ReplaceFiles(zap.NewNop(), files)
			if err == nil || !strings.Contains(err.Error(), "failed to write "+target+" beside the file it replaces, so no file was changed") {
				t.Fatalf("error %v", err)
			}
			mustEqual(t, "files", filesIn(t, dir), []string{"a.yaml", "b.yaml", left})
			mustEqual(t, "a.yaml", mustRead(t, files[0].Path), "a-old")
			mustEqual(t, "b.yaml", mustRead(t, files[1].Path), "b-old")
			mustEqual(t, "the file left earlier", mustRead(t, leftPath), "left by a killed run")
		})
	}
}

// A target that is there but cannot be read (here, a directory under a file's
// name) is not taken for one that does not exist: the group stops before any
// target is touched, the directory and what it holds stay, and nothing is
// left beside the files. Taken for a missing file, it would be moved aside
// and a file put in its place.
func TestReplaceFiles_ATargetThatCannotBeReadTouchesNothing(t *testing.T) {
	dir, files := groupFixture(t)
	target := filepath.Join(dir, "c.yaml")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(target, "kept.txt")
	if err := os.WriteFile(kept, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ReplaceFiles(zap.NewNop(), files)
	if err == nil || !strings.Contains(err.Error(), "failed to write c.yaml beside the file it replaces, so no file was changed") {
		t.Fatalf("error %v", err)
	}
	mustEqual(t, "files", filesIn(t, dir), []string{"a.yaml", "b.yaml", "c.yaml"})
	mustEqual(t, "a.yaml", mustRead(t, files[0].Path), "a-old")
	mustEqual(t, "b.yaml", mustRead(t, files[1].Path), "b-old")
	mustEqual(t, "the file in the directory", mustRead(t, kept), "kept")
}

// When a replaced file cannot be put back either, the error names it, and the
// file as it was stays beside it for FindLeftovers to find: nothing is
// removed that a person needs to put the group back.
func TestReplaceFiles_AFileThatCannotBePutBackIsNamedAndKept(t *testing.T) {
	dir, files := groupFixture(t)
	// b.yaml's new bytes cannot be put in place, and a.yaml, replaced by
	// then, cannot be put back.
	err := ReplaceFilesWith(zap.NewNop(), ReplaceHooks{Rename: refusing("b.yaml"+ReplaceNewSuffix, "a.yaml"+ReplaceOldSuffix)}, files)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"failed to put b.yaml in place", "could not put back a.yaml", "named with the suffix " + ReplaceOldSuffix} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q, want it to say %q", err, want)
		}
	}
	mustEqual(t, "a.yaml, replaced", mustRead(t, files[0].Path), "a-new")
	mustEqual(t, "a.yaml as it was", mustRead(t, files[0].Path+ReplaceOldSuffix), "a-old")
	mustEqual(t, "b.yaml", mustRead(t, files[1].Path), "b-old")
	left, leftErr := FindLeftovers(dir, filepath.Join(dir, "no-such-dir"))
	if leftErr != nil {
		t.Fatal(leftErr)
	}
	mustEqual(t, "left behind", left.Files, []string{
		filepath.Join(dir, "a.yaml"+ReplaceOldSuffix),
		filepath.Join(dir, "b.yaml"+ReplaceNewSuffix),
		filepath.Join(dir, "b.yaml"+ReplaceOldSuffix),
		filepath.Join(dir, "c.yaml"+ReplaceAbsentSuffix),
		filepath.Join(dir, "c.yaml"+ReplaceNewSuffix),
	})
	// b.yaml's new bytes are still waiting, so the steps put the group back.
	mustEqual(t, "finished", left.Finished, false)
}

// A target the platform's own fallback moved aside and could not move back
// (ReplaceFile's last resort failing too) is not left missing: the file as
// it was is put at its name, and the group is as it was.
func TestReplaceFiles_ATargetLeftAsideIsPutBackAtItsName(t *testing.T) {
	dir, files := groupFixture(t)
	// b.yaml's new bytes are refused, and so is moving b.yaml back from where
	// the fallback moved it.
	err := ReplaceFilesWith(zap.NewNop(), ReplaceHooks{Rename: refusingAny("b.yaml"+ReplaceNewSuffix, ".replaced.")}, files)
	if err == nil || !strings.Contains(err.Error(), "failed to put b.yaml in place, so every file was put back as it was") {
		t.Fatalf("error %v", err)
	}
	mustEqual(t, "a.yaml", mustRead(t, files[0].Path), "a-old")
	mustEqual(t, "b.yaml", mustRead(t, files[1].Path), "b-old")
	if _, statErr := os.Stat(files[2].Path); !os.IsNotExist(statErr) {
		t.Fatalf("c.yaml, which did not exist, is there: %v", statErr)
	}
	if left, leftErr := FindLeftovers(dir); leftErr != nil || len(left.Files) != 0 {
		t.Fatalf("left behind: %v, %v", left.Files, leftErr)
	}

	// And when the file as it was cannot be put there either, the error
	// names the target, and the file as it was stays beside it.
	dir, files = groupFixture(t)
	err = ReplaceFilesWith(zap.NewNop(), ReplaceHooks{Rename: refusingAny("b.yaml"+ReplaceNewSuffix, ".replaced.", "b.yaml"+ReplaceOldSuffix)}, files)
	if err == nil || !strings.Contains(err.Error(), "could not put back b.yaml") {
		t.Fatalf("error %v", err)
	}
	mustEqual(t, "a.yaml", mustRead(t, files[0].Path), "a-old")
	mustEqual(t, "b.yaml as it was", mustRead(t, files[1].Path+ReplaceOldSuffix), "b-old")
}

// refusingAny is a platform that will not rename any file whose name holds
// one of parts.
func refusingAny(parts ...string) func(src, dst string) error {
	return func(src, dst string) error {
		for _, p := range parts {
			if strings.Contains(filepath.Base(src), p) {
				return &os.LinkError{Op: "rename", Old: src, New: dst, Err: errRefused}
			}
		}
		return os.Rename(src, dst)
	}
}

// killed is what a test's Step panics with to stop a ReplaceFiles call where
// a kill would.
type killed struct{}

// replaceKilledAt runs ReplaceFiles over files and stops it right after its
// n-th change on disk, as a kill would. It reports false when the call
// finished before making n changes.
func replaceKilledAt(t *testing.T, files []File, n int) (stopped bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(killed); !ok {
				panic(r)
			}
			stopped = true
		}
	}()
	count := 0
	err := ReplaceFilesWith(zap.NewNop(), ReplaceHooks{Step: func() {
		if count++; count == n {
			panic(killed{})
		}
	}}, files)
	if err != nil {
		t.Fatal(err)
	}
	return false
}

// applySteps does what the leftovers' steps say: rename each file as it was
// back, delete what the run created, delete the rest.
func applySteps(t *testing.T, l Leftovers) {
	t.Helper()
	for _, m := range l.Restore {
		if err := os.Rename(m[0], m[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range append(append([]string(nil), l.Created...), l.Remove...) {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
}

// contentsOf is every file in dir and its bytes.
func contentsOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range filesIn(t, dir) {
		out[name] = mustRead(t, filepath.Join(dir, name))
	}
	return out
}

// A KILL ANYWHERE LEAVES WHAT ITS STEPS PUT IN ONE STATE. Stopped after any
// change it makes on disk (a file created and not yet written, a file written,
// a rename, a removal), a group leaves files whose steps (FindLeftovers) give
// back either every file as it was, when new bytes were still waiting, or
// every file as the group wrote it, when only its copies were left, and
// nothing beside them. Before, one line said to put back every copy: right
// only between the renames. A kill while the copies were removed put back
// some files and left others written (and the next run called the set done),
// and a kill while a copy was being written put an empty file over its
// target. Whichever file the group creates is first or last.
func TestReplaceFiles_AKillAnywhereIsPutInOneStateByItsSteps(t *testing.T) {
	asWas := map[string]string{"a.yaml": "a-old", "b.yaml": "b-old"}
	asWritten := map[string]string{"a.yaml": "a-new", "b.yaml": "b-new", "c.yaml": "c-new"}
	for _, order := range [][]int{{0, 1, 2}, {2, 0, 1}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			var restored, finished, emptyCopy, createdRemoved int
			n := 1
			for ; ; n++ {
				dir, fixture := groupFixture(t)
				files := []File{fixture[order[0]], fixture[order[1]], fixture[order[2]]}
				if !replaceKilledAt(t, files, n) {
					break
				}
				left, err := FindLeftovers(dir)
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range left.Files {
					if info, statErr := os.Stat(f); statErr == nil && info.Size() == 0 && strings.HasSuffix(f, ReplacePartSuffix) {
						emptyCopy++
					}
				}
				want := asWas
				switch {
				case len(left.Files) == 0:
					// Stopped after its last change: it had finished.
					want = asWritten
				case left.Finished:
					finished++
					want = asWritten
				case contentsOf(t, dir)["a.yaml"] == "a-new":
					restored++
				}
				createdRemoved += len(left.Created)
				applySteps(t, left)
				if got := contentsOf(t, dir); !reflect.DeepEqual(got, want) {
					t.Fatalf("killed after change %d, leftovers %v (finished %v): the steps give %q, want %q", n, left.Files, left.Finished, got, want)
				}
			}
			// Each window was met: a target replaced and put back, the
			// copies being removed, a copy created and not yet written.
			if restored == 0 || finished == 0 || emptyCopy == 0 || n < 15 {
				t.Fatalf("%d changes; kills that put back a replaced target %d, after every target was replaced %d, with an empty copy %d", n-1, restored, finished, emptyCopy)
			}
			if order[0] == 2 && createdRemoved == 0 {
				t.Fatal("no kill left the created c.yaml for the steps to remove")
			}
		})
	}
}

// The steps name each file: the renames and deletions that put the group back
// as it was, a target the run created named as such; or only the copies to
// delete when every file was in place.
func TestLeftovers_StepsNameEachFile(t *testing.T) {
	name := func(p string) string { return p }
	l := Leftovers{
		Restore: [][2]string{{"tests/test-1.yaml" + ReplaceOldSuffix, "tests/test-1.yaml"}},
		Created: []string{"config.yaml"},
		Remove:  []string{"config.yaml" + ReplaceAbsentSuffix, "tests/test-2.yaml" + ReplaceNewSuffix},
	}
	mustEqual(t, "steps", l.Steps(name), "it had not put every file in place, so put them back as they were (or restore them from version control): rename tests/test-1.yaml.keploy-old to tests/test-1.yaml; delete config.yaml, which that run created; delete config.yaml.keploy-absent, tests/test-2.yaml.keploy-new")
	l = Leftovers{Finished: true, Remove: []string{"tests/test-1.yaml" + ReplaceOldSuffix}}
	mustEqual(t, "steps", l.Steps(name), "it had put every file in place and was removing the copies it kept, so the files are as that run wrote them: delete tests/test-1.yaml.keploy-old")
}

// A FAILED GROUP KILLED WHILE IT PUTS BACK AND CLEANS UP leaves what its steps
// put back as it was. The last file's new bytes cannot be put in place, so the
// group puts back what it replaced and removes what it wrote beside the
// files; killed after any of those changes, the files left say a group that
// had not put every file in place (its new bytes are removed last), and the
// steps give every file back as it was. Removing the new bytes first left
// only copies, which read as a group that had finished.
func TestReplaceFiles_AFailedGroupKilledWhileItCleansUpIsPutBackByItsSteps(t *testing.T) {
	asWas := map[string]string{"a.yaml": "a-old", "b.yaml": "b-old"}
	for _, order := range [][]int{{0, 1, 2}, {2, 0, 1}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			last := []string{"a.yaml", "b.yaml", "c.yaml"}[order[2]]
			kills := 0
			for n := 1; ; n++ {
				dir, fixture := groupFixture(t)
				files := []File{fixture[order[0]], fixture[order[1]], fixture[order[2]]}
				count, stopped := 0, false
				err := func() (err error) {
					defer func() {
						if r := recover(); r != nil {
							if _, ok := r.(killed); !ok {
								panic(r)
							}
							stopped = true
						}
					}()
					return ReplaceFilesWith(zap.NewNop(), ReplaceHooks{Rename: refusing(last + ReplaceNewSuffix), Step: func() {
						if count++; count == n {
							panic(killed{})
						}
					}}, files)
				}()
				if !stopped {
					if err == nil {
						t.Fatal("the group whose last file cannot be put in place reported no error")
					}
					break
				}
				kills++
				left, leftErr := FindLeftovers(dir)
				if leftErr != nil {
					t.Fatal(leftErr)
				}
				if left.Finished && len(left.Files) > 0 {
					t.Fatalf("killed after change %d of a failed group, the leftovers %v read as a group that had finished", n, left.Files)
				}
				applySteps(t, left)
				if got := contentsOf(t, dir); !reflect.DeepEqual(got, asWas) {
					t.Fatalf("killed after change %d, leftovers %v: the steps give %q, want %q", n, left.Files, got, asWas)
				}
			}
			if kills < 10 {
				t.Fatalf("only %d kill points", kills)
			}
		})
	}
}

// A directory FindLeftovers cannot read is an error, not one that holds no
// leftovers: only a directory that does not exist holds none.
func TestFindLeftovers_ADirectoryItCannotReadIsAnError(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FindLeftovers(notADir); err == nil {
		t.Fatal("no error for a path it cannot read as a directory")
	}
	if l, err := FindLeftovers(filepath.Join(t.TempDir(), "missing")); err != nil || len(l.Files) != 0 {
		t.Fatalf("a missing directory: %v, %v", l.Files, err)
	}
}
