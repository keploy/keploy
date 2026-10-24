//go:build unix

package testdb

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// unreadableTests records t1 in set-1 under dir and makes set-1's tests
// folder one that cannot be looked into:
//   - "permission denied": mode 0300, so it cannot be listed, yet t1 is still
//     read and rewritten by its name;
//   - "listed but not searched": mode 0600, so it can be listed, but t1 in it
//     cannot be looked at;
//   - "symlink loop": a symlink to itself, with no t1.
//
// It returns t1's path ("" for the loop). A process that can still do what
// the mode forbids (root) skips the case.
func unreadableTests(t *testing.T, dir, how string) string {
	t.Helper()
	tests := filepath.Join(dir, "set-1", "tests")
	if how == "symlink loop" {
		if err := os.MkdirAll(filepath.Dir(tests), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(tests, tests); err != nil {
			t.Fatal(err)
		}
		return ""
	}
	if err := New(zap.NewNop(), dir).UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/a"), "set-1", false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tests, "t1.yaml")
	mode := map[string]os.FileMode{"permission denied": 0o300, "listed but not searched": 0o600}[how]
	if mode == 0 {
		t.Fatalf("unknown case %q", how)
	}
	if err := os.Chmod(tests, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tests, 0o755) })
	if how == "permission denied" {
		if _, err := os.ReadDir(tests); err == nil {
			t.Skipf("this process (euid %d) can still list a mode-0300 folder, so the permission error cannot be made here, and this case is not tested as root", os.Geteuid())
		}
	} else if _, err := os.Lstat(path); err == nil {
		t.Skipf("this process (euid %d) can still search a mode-0600 folder, so the permission error cannot be made here, and this case is not tested as root", os.Geteuid())
	}
	return path
}

// A set rewrite names every recording of the set before the command rewrites
// them. A tests folder that is there but cannot be listed, or a recording in
// it that cannot be looked at, is an error, not something with nothing to
// name: the command could still rewrite a recording nobody named, and the
// entry of verdicts.yaml that names it would go stale without a word. The set
// is not left marked as being rewritten.
func TestBeginSetRewrite_AnUnreadableTestsFolderIsAnError(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	useRekeyer(t, &fakeRekeyer{})
	for _, tc := range []struct {
		how  string
		want error
	}{
		{"permission denied", fs.ErrPermission},
		{"listed but not searched", fs.ErrPermission},
		{"symlink loop", syscall.ELOOP},
	} {
		t.Run(tc.how, func(t *testing.T) {
			dir := t.TempDir()
			unreadableTests(t, dir, tc.how)
			w, err := BeginSetRewrite(zap.NewNop(), dir, "set-1")
			if !errors.Is(err, tc.want) || w != nil {
				t.Fatalf("BeginSetRewrite = %v, %v; want no rewrite and %v", w, err, tc.want)
			}
			if rewriting(dir, "set-1") {
				t.Fatal("a rewrite that failed to begin left set-1 marked as being rewritten: no later write to it would re-key on its own")
			}
		})
	}
}

// A set without a tests folder, or no set folder at all, has no recording to
// name, and no error.
func TestBeginSetRewrite_AMissingTestsFolderNamesNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	r := &fakeRekeyer{}
	useRekeyer(t, r)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "set-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, set := range []string{"set-1", "set-2"} {
		w, err := BeginSetRewrite(zap.NewNop(), dir, set)
		if err != nil || w == nil {
			t.Fatalf("BeginSetRewrite(%s) = %v, %v; want a rewrite naming nothing, and no error", set, w, err)
		}
		w.Done()
	}
	if got := r.told(); len(got) != 0 {
		t.Fatalf("re-keyed %v for sets with no recordings", got)
	}
}

// A recording named when the rewrite began that cannot be looked at when it
// is done is said, not taken for one the command removed.
func TestSetRewriteDone_ARecordingThatCannotBeLookedAtIsSaid(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	if err := New(zap.NewNop(), dir).UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/a"), "set-1", false); err != nil {
		t.Fatal(err)
	}
	r := &fakeRekeyer{}
	useRekeyer(t, r)
	core, logs := observer.New(zap.WarnLevel)
	w, err := BeginSetRewrite(zap.New(core), dir, "set-1")
	if err != nil || w == nil {
		t.Fatalf("BeginSetRewrite = %v, %v", w, err)
	}
	tests := filepath.Join(dir, "set-1", "tests")
	if err := os.Chmod(tests, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tests, 0o755) })
	if _, err := os.Lstat(filepath.Join(tests, "t1.yaml")); err == nil {
		w.Done()
		t.Skipf("this process (euid %d) can still search a mode-0600 folder, so the permission error cannot be made here, and this case is not tested as root", os.Geteuid())
	}
	w.Done()
	if said := logs.FilterMessageSnippet("could not look at a recording the command may have rewritten").All(); len(said) != 1 {
		t.Fatalf("said %d times, want once: %v", len(said), logs.All())
	}
	if got := r.told(); len(got) != 0 {
		t.Fatalf("re-keyed %v", got)
	}
}

// A recording the command removed is not one that cannot be looked at: it
// re-keys nothing, and nothing is said.
func TestSetRewriteDone_ARemovedRecordingIsNotSaid(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	if err := New(zap.NewNop(), dir).UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/a"), "set-1", false); err != nil {
		t.Fatal(err)
	}
	r := &fakeRekeyer{}
	useRekeyer(t, r)
	core, logs := observer.New(zap.WarnLevel)
	w, err := BeginSetRewrite(zap.New(core), dir, "set-1")
	if err != nil || w == nil {
		t.Fatalf("BeginSetRewrite = %v, %v", w, err)
	}
	if err := os.Remove(filepath.Join(dir, "set-1", "tests", "t1.yaml")); err != nil {
		t.Fatal(err)
	}
	w.Done()
	if logs.Len() != 0 || len(r.told()) != 0 {
		t.Fatalf("a removed recording: said %v, re-keyed %v; want neither", logs.All(), r.told())
	}
}

// A single write over a recording that cannot be looked at says that its
// entry of verdicts.yaml is not moved, as it does for one that cannot be
// named; it used to say nothing. A write of a new recording, with no old one
// to look at, says nothing.
func TestAWriteOverARecordingThatCannotBeLookedAtIsSaid(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	useRekeyer(t, &fakeRekeyer{})
	core, logs := observer.New(zap.WarnLevel)
	ts := New(zap.New(core), dir)
	if err := ts.UpdateTestCase(t.Context(), namedTC("t0", "http://api.test/new"), "set-1", false); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatalf("a new recording said %v", logs.All())
	}
	unreadableTests(t, dir, "listed but not searched")
	// The write itself fails too, on the same folder; what matters here is
	// that the stale entry it may leave is said.
	_ = ts.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/b"), "set-1", false)
	if said := logs.FilterMessageSnippet("could not look at the recording, if any, that it is about to rewrite").All(); len(said) != 1 {
		t.Fatalf("said %d times, want once: %v", len(said), logs.All())
	}
}

// UpdateAssertions reads and rewrites one recording by its name, which a
// tests folder at mode 0300 still allows. It used to rewrite it with nothing
// named, so the entry of verdicts.yaml that named the old recording went
// stale and nobody was told. It now fails before it writes anything.
func TestUpdateAssertions_AnUnlistableTestsFolderFailsBeforeAnyWrite(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	path := unreadableTests(t, dir, "permission denied")
	r := &fakeRekeyer{}
	useRekeyer(t, r)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = New(zap.NewNop(), dir).UpdateAssertions(t.Context(), "t1", "set-1", map[models.AssertionType]interface{}{models.StatusCode: 201})
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("UpdateAssertions = %v; want the permission error", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("UpdateAssertions rewrote t1 although it could not name it:\n%s", after)
	}
	if got := r.told(); len(got) != 0 {
		t.Fatalf("re-keyed %v", got)
	}
}
