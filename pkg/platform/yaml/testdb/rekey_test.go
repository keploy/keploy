package testdb

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// fakeRekeyer names a recording by its file's bytes and its body asset's, as
// enterprise's hash covers a test's assets, and keeps every re-key it is told.
type fakeRekeyer struct {
	mu       sync.Mutex
	rekeys   []string
	hashErr  func(path string) error
	rekeyErr error
}

func (f *fakeRekeyer) Hash(keployDir, path string) (string, error) {
	if f.hashErr != nil {
		if err := f.hashErr(path); err != nil {
			return "", err
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(raw)
	test := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	set := filepath.Base(filepath.Dir(filepath.Dir(path)))
	if asset, err := os.ReadFile(filepath.Join(keployDir, set, "assets", test, "body.txt")); err == nil {
		h.Write([]byte("\x00asset\x00"))
		h.Write(asset)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func (f *fakeRekeyer) Rekey(keployDir, set, test, old, newHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rekeys = append(f.rekeys, set+"/"+test+" "+old+" -> "+newHash)
	return f.rekeyErr
}

func (f *fakeRekeyer) told() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rekeys...)
}

func useRekeyer(t *testing.T, r Rekeyer) {
	t.Helper()
	SetRekeyer(r)
	t.Cleanup(func() { SetRekeyer(nil) })
}

// hashOf is the fake's name for the recording at path now.
func hashOf(t *testing.T, r *fakeRekeyer, keployDir, path string) string {
	t.Helper()
	h, err := r.Hash(keployDir, path)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Under the proven switch a write that rewrites a test's recording where it
// lies tells the registered Rekeyer, right after the write, the recording's
// hash before and after (design v14 §2.4): UpdateTestCase (normalize, the
// replay's denoise, auto-noise, cloud sync) and InsertTestCase over a named
// test alike. The old hash is taken before anything of the recording is
// replaced, its assets included. A write that replaces no recording (a new
// test, a name just claimed) re-keys nothing, nor does one that leaves it as
// it was, nor anything without the switch.
func TestAWriteThatRewritesARecordingReKeysIt(t *testing.T) {
	for _, format := range []yamlLib.Format{yamlLib.FormatYAML, yamlLib.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			dir := t.TempDir()
			ts := NewWithFormat(zap.NewNop(), dir, format)
			r := &fakeRekeyer{}
			useRekeyer(t, r)
			path := filepath.Join(dir, "set-1", "tests", "t1."+format.FileExtension())

			// A new recording names nothing yet.
			if err := ts.InsertTestCase(t.Context(), namedTC("t1", "http://api.test/a"), "set-1", false); err != nil {
				t.Fatal(err)
			}
			unnamed := httpTC("GET", "http://api.test/claimed")
			if err := ts.InsertTestCase(t.Context(), unnamed, "set-1", false); err != nil {
				t.Fatal(err)
			}
			if got := r.told(); len(got) != 0 {
				t.Fatalf("new recordings were re-keyed: %v", got)
			}

			old := hashOf(t, r, dir, path)
			if err := ts.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/b"), "set-1", false); err != nil {
				t.Fatal(err)
			}
			want := []string{"set-1/t1 " + old + " -> " + hashOf(t, r, dir, path)}
			if got := r.told(); len(got) != 1 || got[0] != want[0] {
				t.Fatalf("re-keyed %v, want %v", got, want)
			}

			// The same recording written again changes nothing to re-key.
			if err := ts.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/b"), "set-1", false); err != nil {
				t.Fatal(err)
			}
			if got := r.told(); len(got) != 1 {
				t.Fatalf("an unchanged recording was re-keyed: %v", got)
			}

			// InsertTestCase over a named test rewrites it too.
			old = hashOf(t, r, dir, path)
			if err := ts.InsertTestCase(t.Context(), namedTC("t1", "http://api.test/c"), "set-1", false); err != nil {
				t.Fatal(err)
			}
			if got := r.told(); len(got) != 2 || got[1] != "set-1/t1 "+old+" -> "+hashOf(t, r, dir, path) {
				t.Fatalf("re-keyed %v after InsertTestCase over t1", got)
			}

			t.Setenv(provenswitch.Env, "")
			if err := ts.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/d"), "set-1", false); err != nil {
				t.Fatal(err)
			}
			if got := r.told(); len(got) != 2 {
				t.Fatalf("without the switch a write was re-keyed: %v", got)
			}
		})
	}
}

// The recording's hash before the write covers the assets the write
// replaces: a request body keploy keeps beside the test case is rewritten
// before the test case file, which itself may not change at all.
func TestTheOldHashIsTakenBeforeTheAssetsAreReplaced(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	ts := New(zap.NewNop(), dir)
	r := &fakeRekeyer{}
	useRekeyer(t, r)
	big := func(c string) *models.TestCase {
		tc := namedTC("t1", "http://api.test/upload")
		tc.HTTPReq.Method = "POST"
		tc.HTTPReq.Body = strings.Repeat(c, LargeBodyThreshold+1)
		return tc
	}
	if err := ts.UpdateTestCase(t.Context(), big("a"), "set-1", false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "set-1", "tests", "t1.yaml")
	old := hashOf(t, r, dir, path)
	if err := ts.UpdateTestCase(t.Context(), big("b"), "set-1", false); err != nil {
		t.Fatal(err)
	}
	want := "set-1/t1 " + old + " -> " + hashOf(t, r, dir, path)
	if got := r.told(); len(got) != 1 || got[0] != want {
		t.Fatalf("re-keyed %v, want [%s]", got, want)
	}
}

// keploy templatize stores its test cases as one group (ReplaceFiles), with
// the set's config.yaml: each test case of the group is re-keyed once the
// group is in place, and the config names no recording. A group that could
// not be stored re-keys nothing.
func TestAGroupOfFilesReKeysEachTestCaseItRewrites(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	ts := New(zap.NewNop(), dir)
	for _, name := range []string{"t1", "t2"} {
		if err := ts.UpdateTestCase(t.Context(), namedTC(name, "http://api.test/"+name), "set-1", false); err != nil {
			t.Fatal(err)
		}
	}
	r := &fakeRekeyer{}
	useRekeyer(t, r)
	group := func(suffix string) []yamlLib.File {
		var files []yamlLib.File
		for _, name := range []string{"t1", "t2"} {
			f, err := ts.TestCaseFile(t.Context(), namedTC(name, "http://api.test/"+name+suffix), "set-1")
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
		return append(files, yamlLib.File{Path: filepath.Join(dir, "set-1", "config.yaml"), Data: []byte("template: {}\n")})
	}
	p1, p2 := filepath.Join(dir, "set-1", "tests", "t1.yaml"), filepath.Join(dir, "set-1", "tests", "t2.yaml")

	prev := replaceFiles
	replaceFiles = func(*zap.Logger, []yamlLib.File) error { return errSharingViolation }
	if err := ts.ReplaceFiles(t.Context(), group("?x=1")); !errors.Is(err, errSharingViolation) {
		t.Fatalf("ReplaceFiles: %v", err)
	}
	replaceFiles = prev
	if got := r.told(); len(got) != 0 {
		t.Fatalf("a group that was not stored re-keyed %v", got)
	}

	old1, old2 := hashOf(t, r, dir, p1), hashOf(t, r, dir, p2)
	if err := ts.ReplaceFiles(t.Context(), group("?x=1")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"set-1/t1 " + old1 + " -> " + hashOf(t, r, dir, p1),
		"set-1/t2 " + old2 + " -> " + hashOf(t, r, dir, p2),
	}
	got := r.told()
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("re-keyed %v, want %v", got, want)
	}
}

// A recording keploy cannot name before the write, or a re-key that fails,
// is said: the write stands, the entry that named the recording goes stale,
// and the test runs. Neither fails the write.
func TestAReKeyThatCannotBeDoneIsSaidAndTheWriteStands(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, tc := range []struct {
		name string
		r    *fakeRekeyer
	}{
		{"the old recording cannot be named", &fakeRekeyer{hashErr: func(string) error { return errors.New("does not parse") }}},
		{"the re-key fails", &fakeRekeyer{rekeyErr: errors.New("verdicts.yaml is unreadable")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			core, logs := observer.New(zap.WarnLevel)
			ts := New(zap.New(core), dir)
			if err := ts.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/a"), "set-1", false); err != nil {
				t.Fatal(err)
			}
			useRekeyer(t, tc.r)
			if err := ts.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/b"), "set-1", false); err != nil {
				t.Fatalf("UpdateTestCase: %v; the write stands", err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "set-1", "tests", "t1.yaml"))
			if err != nil || !strings.Contains(string(raw), "http://api.test/b") {
				t.Fatalf("the new recording is not in place: %v", err)
			}
			said := logs.FilterMessageSnippet("is now stale, and the test runs").All()
			if len(said) != 1 {
				t.Fatalf("said %d times, want once", len(said))
			}
		})
	}
}

// A write that fails re-keys nothing, even when it got as far as replacing
// the recording's assets: the entry stays on what it named, and goes stale
// if that is no longer on disk.
func TestAFailedWriteReKeysNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	ts := New(zap.NewNop(), dir)
	big := func(c string) *models.TestCase {
		tc := namedTC("t1", "http://api.test/upload")
		tc.HTTPReq.Method = "POST"
		tc.HTTPReq.Body = strings.Repeat(c, LargeBodyThreshold+1)
		return tc
	}
	if err := ts.UpdateTestCase(t.Context(), big("a"), "set-1", false); err != nil {
		t.Fatal(err)
	}
	r := &fakeRekeyer{}
	useRekeyer(t, r)
	useRename(t, renameRefusingTempFiles)
	if err := ts.UpdateTestCase(t.Context(), big("b"), "set-1", false); !errors.Is(err, errSharingViolation) {
		t.Fatalf("UpdateTestCase: %v, want the refused rename", err)
	}
	if got := r.told(); len(got) != 0 {
		t.Fatalf("a failed write re-keyed %v", got)
	}
}

// A file of a group that lies outside the store's keploy directory is no
// test case of it, and is not re-keyed.
func TestAGroupFileOutsideTheStoreIsNotReKeyed(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir, other := t.TempDir(), t.TempDir()
	ts, elsewhere := New(zap.NewNop(), dir), New(zap.NewNop(), other)
	if err := elsewhere.UpdateTestCase(t.Context(), namedTC("t1", "http://api.test/a"), "set-1", false); err != nil {
		t.Fatal(err)
	}
	f, err := elsewhere.TestCaseFile(t.Context(), namedTC("t1", "http://api.test/b"), "set-1")
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRekeyer{}
	useRekeyer(t, r)
	if err := ts.ReplaceFiles(t.Context(), []yamlLib.File{f}); err != nil {
		t.Fatal(err)
	}
	if got := r.told(); len(got) != 0 {
		t.Fatalf("a file outside the store was re-keyed: %v", got)
	}
}

// A command that rewrites a set in several writes, some outside testdb
// (keploy sanitize; normalize of a sanitized set), names every recording
// before the first and re-keys each once, when it is done (SetRewrite): a
// write in between re-keys nothing, and a rewrite of the set inside it is
// its own. Nothing without the switch.
func TestASetRewriteReKeysEachRecordingOnceWhenItIsDone(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	dir := t.TempDir()
	ts := New(zap.NewNop(), dir)
	for _, name := range []string{"t1", "t2", "t3"} {
		if err := ts.UpdateTestCase(t.Context(), namedTC(name, "http://api.test/"+name), "set-1", false); err != nil {
			t.Fatal(err)
		}
	}
	r := &fakeRekeyer{}
	useRekeyer(t, r)
	tests := filepath.Join(dir, "set-1", "tests")
	p1, p2 := filepath.Join(tests, "t1.yaml"), filepath.Join(tests, "t2.yaml")
	old1, old2 := hashOf(t, r, dir, p1), hashOf(t, r, dir, p2)

	// The keploy directory as another spelling of it names the same set.
	w := BeginSetRewrite(zap.NewNop(), dir+string(filepath.Separator)+".", "set-1")
	if w == nil {
		t.Fatal("no rewrite under the switch with a Rekeyer")
	}
	if inner := BeginSetRewrite(zap.NewNop(), dir, "set-1"); inner != nil {
		t.Fatal("a rewrite inside another of the same set is not the outer one's")
	}
	for _, url := range []string{"http://api.test/t1?x=1", "http://api.test/t1?x=2"} {
		if err := ts.UpdateTestCase(t.Context(), namedTC("t1", url), "set-1", false); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(p2)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte(strings.Replace(string(raw), "http://api.test/t2", "http://api.test/t2?redacted", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := r.told(); len(got) != 0 {
		t.Fatalf("writes inside the rewrite re-keyed %v", got)
	}
	w.Done()
	want := []string{
		"set-1/t1 " + old1 + " -> " + hashOf(t, r, dir, p1),
		"set-1/t2 " + old2 + " -> " + hashOf(t, r, dir, p2),
	}
	if got := r.told(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("re-keyed %v, want %v: once each, t3 left as it was", got, want)
	}
	// Once done, a write re-keys on its own again.
	if err := ts.UpdateTestCase(t.Context(), namedTC("t3", "http://api.test/t3?y"), "set-1", false); err != nil {
		t.Fatal(err)
	}
	if got := r.told(); len(got) != 3 {
		t.Fatalf("a write after the rewrite re-keyed %v", got)
	}

	t.Setenv(provenswitch.Env, "")
	if w := BeginSetRewrite(zap.NewNop(), dir, "set-1"); w != nil {
		t.Fatal("a rewrite without the switch")
	}
}
