package mockdb

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// insertURLs inserts one mock per URL into set through ys.
func insertURLs(t *testing.T, ys *MockYaml, set string, urls ...string) {
	t.Helper()
	for _, u := range urls {
		if err := ys.InsertMock(context.Background(), httpMockURL(u), set); err != nil {
			t.Fatalf("InsertMock %s: %v", u, err)
		}
	}
}

// closeOK closes ys and fails the test on any error: every mock it was handed
// is written.
func closeOK(t *testing.T, ys *MockYaml) {
	t.Helper()
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// setURLs reads set back as replay reads it (mocks.gob first when present),
// with a db of its own.
func setURLs(t *testing.T, dir, set string) []string {
	t.Helper()
	var got []string
	err := New(zap.NewNop(), dir, "mocks").WalkTestSetMocks(context.Background(), set, func(m *models.Mock, _, _ bool) {
		got = append(got, m.Spec.HTTPReq.URL)
	})
	if err != nil {
		t.Fatalf("reading %s: %v", set, err)
	}
	return got
}

func wantURLs(t *testing.T, dir, set string, want ...string) {
	t.Helper()
	if got := setURLs(t, dir, set); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s holds %v, want %v", set, got, want)
	}
}

// A writer that adds to a set another writer left on disk keeps the mocks
// the set holds, in either format: keploy mock replay --on-miss record does
// this to the set it replayed (SetCounterID, then InsertMock). The gob writer
// used to open the set's file with O_TRUNC, which left the added mock alone.
func TestInsertMock_AddingToAnExistingSetKeepsItsMocks(t *testing.T) {
	for _, format := range []string{"yaml", mockFormatGob} {
		t.Run(format, func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", format)
			dir := t.TempDir()
			rec := newMockYamlForFormat(dir, format)
			insertURLs(t, rec, "set-0", "http://a", "http://b", "http://c")
			closeOK(t, rec)

			add := newMockYamlForFormat(dir, format)
			add.SetCounterID(3)
			insertURLs(t, add, "set-0", "http://d")
			closeOK(t, add)

			wantURLs(t, dir, "set-0", "http://a", "http://b", "http://c", "http://d")
		})
	}
}

// One gob writer that comes back to a set's file keeps what it wrote there:
// after another set's mocks in between, and after a Close in between (a
// re-record cycle on the same db).
func TestInsertMock_AGobWriterBackAtASetKeepsItsMocks(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", mockFormatGob)
	t.Run("another set in between", func(t *testing.T) {
		dir := t.TempDir()
		ys := New(zap.NewNop(), dir, "mocks")
		insertURLs(t, ys, "set-0", "http://a")
		insertURLs(t, ys, "set-1", "http://x")
		insertURLs(t, ys, "set-0", "http://b")
		closeOK(t, ys)
		wantURLs(t, dir, "set-0", "http://a", "http://b")
		wantURLs(t, dir, "set-1", "http://x")
	})
	t.Run("a close in between", func(t *testing.T) {
		dir := t.TempDir()
		ys := New(zap.NewNop(), dir, "mocks")
		insertURLs(t, ys, "set-0", "http://a", "http://b")
		closeOK(t, ys)
		insertURLs(t, ys, "set-0", "http://c")
		closeOK(t, ys)
		wantURLs(t, dir, "set-0", "http://a", "http://b", "http://c")
	})
}

// A gob file the writer cannot read is left exactly as it was, and the mock
// meant for it is not written, which Close says: the writer never replaces a
// file whose mocks it could not carry over.
func TestInsertMock_AGobFileItCannotReadIsLeftAsItWas(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", mockFormatGob)
	dir := t.TempDir()
	setDir := filepath.Join(dir, "set-0")
	if err := os.MkdirAll(setDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(setDir, "mocks.gob")
	old := []byte("not a keploy gob mock file, but somebody's bytes\n")
	if err := os.WriteFile(path, old, 0o640); err != nil {
		t.Fatal(err)
	}

	ys := New(zap.NewNop(), dir, "mocks")
	insertURLs(t, ys, "set-0", "http://d")
	err := ys.Close()
	if !errors.Is(err, models.ErrMocksNotWritten) {
		t.Fatalf("Close = %v, want models.ErrMocksNotWritten", err)
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil || !bytes.Equal(got, old) {
		t.Fatalf("the file is now %q (%v), want it as it was: %q", got, rerr, old)
	}
	entries, _ := os.ReadDir(setDir)
	if len(entries) != 1 {
		t.Fatalf("the set's directory holds %d files, want mocks.gob alone (no temp file left)", len(entries))
	}
}

// A gob file cut part-way through a mock (a writer killed mid-write) keeps
// the mocks before the cut, says the cut one is not kept, and takes the new
// mocks after them in a file every reader reads whole. Its mode is kept.
func TestInsertMock_AddingToACutGobFileKeepsTheMocksBeforeTheCut(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", mockFormatGob)
	dir := t.TempDir()
	rec := New(zap.NewNop(), dir, "mocks")
	insertURLs(t, rec, "set-0", "http://a", "http://b")
	closeOK(t, rec)
	path := filepath.Join(dir, "set-0", "mocks.gob")
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, whole[:len(whole)-3], 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zapcore.WarnLevel)
	add := New(zap.New(core), dir, "mocks")
	add.SetCounterID(2)
	insertURLs(t, add, "set-0", "http://c")
	closeOK(t, add)

	wantURLs(t, dir, "set-0", "http://a", "http://c")
	if logs.FilterMessageSnippet("part-way through a mock").Len() != 1 {
		t.Fatalf("the cut mock was dropped without a warning; logs: %v", logs.All())
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mocks.gob mode is %v (%v), want 0640 as it was", info.Mode().Perm(), err)
	}
}

// setFiles is the names of the files in set under dir.
func setFiles(t *testing.T, dir, set string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, set))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// An insert writes to the mock file the set already has, in that file's
// format, whatever format the writer is configured with: the readers read a
// mocks.gob whenever there is one. A yaml writer adding to a gob set used to
// write a mocks.yaml the readers never read, and a gob writer adding to a yaml
// set began a mocks.gob that hid the set's mocks.yaml.
func TestInsertMock_AddsToTheFileTheSetHasInItsFormat(t *testing.T) {
	for _, c := range []struct{ recordedIn, addIn, file string }{
		{mockFormatGob, "yaml", "mocks.gob"},
		{"yaml", mockFormatGob, "mocks.yaml"},
	} {
		t.Run(c.recordedIn+" set, "+c.addIn+" writer", func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", "")
			dir := t.TempDir()
			rec := newMockYamlForFormat(dir, c.recordedIn)
			insertURLs(t, rec, "set-0", "http://a", "http://b", "http://c")
			closeOK(t, rec)

			add := newMockYamlForFormat(dir, c.addIn)
			add.SetCounterID(3)
			insertURLs(t, add, "set-0", "http://d", "http://e")
			closeOK(t, add)

			wantURLs(t, dir, "set-0", "http://a", "http://b", "http://c", "http://d", "http://e")
			if got := setFiles(t, dir, "set-0"); !reflect.DeepEqual(got, []string{c.file}) {
				t.Fatalf("set-0 holds %v, want only %s", got, c.file)
			}
		})
	}
}

// A set that holds a mocks.gob and a mocks.yaml (an older keploy's --on-miss
// record left such sets) is not added to, under either format: the insert
// fails naming both files and neither file changes, since the readers read
// the mocks.gob alone and a mock added to either would be lost or hide the
// other's. Reading the set says the mocks.yaml is not read.
func TestInsertMock_ASetWithAGobAndATextFileIsRefused(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	dir := t.TempDir()
	gobDB := newMockYamlForFormat(dir, mockFormatGob)
	insertURLs(t, gobDB, "set-0", "http://g")
	closeOK(t, gobDB)
	set := filepath.Join(dir, "set-0")
	text := []byte("# a mock file an older keploy wrote beside the gob one\n")
	if err := os.WriteFile(filepath.Join(set, "mocks.yaml"), text, 0o644); err != nil {
		t.Fatal(err)
	}
	gobBytes, err := os.ReadFile(filepath.Join(set, "mocks.gob"))
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []string{"yaml", mockFormatGob} {
		t.Run(format+" writer", func(t *testing.T) {
			ys := newMockYamlForFormat(dir, format)
			err := ys.InsertMock(context.Background(), httpMockURL("http://new"), "set-0")
			if !errors.Is(err, ErrMixedMockFiles) || !errors.Is(err, models.ErrMocksNotWritten) {
				t.Fatalf("InsertMock = %v, want ErrMixedMockFiles and ErrMocksNotWritten", err)
			}
			for _, f := range []string{"mocks.gob", "mocks.yaml"} {
				if !bytes.Contains([]byte(err.Error()), []byte(filepath.Join(set, f))) {
					t.Fatalf("the error does not name %s: %v", f, err)
				}
			}
			closeOK(t, ys)
			if got, _ := os.ReadFile(filepath.Join(set, "mocks.gob")); !bytes.Equal(got, gobBytes) {
				t.Fatal("mocks.gob changed")
			}
			if got, _ := os.ReadFile(filepath.Join(set, "mocks.yaml")); !bytes.Equal(got, text) {
				t.Fatal("mocks.yaml changed")
			}
		})
	}

	core, logs := observer.New(zapcore.WarnLevel)
	if _, err := New(zap.New(core), dir, "mocks").GetTestSetMocks(context.Background(), "set-0", models.BaseTime, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), nil, nil); err != nil {
		t.Fatal(err)
	}
	warned := logs.FilterMessageSnippet("only the gob one is read").All()
	if len(warned) != 1 || warned[0].ContextMap()["not_read"] != filepath.Join(set, "mocks.yaml") {
		t.Fatalf("reading the set warned %v, want one warning naming %s", warned, filepath.Join(set, "mocks.yaml"))
	}
}

// A writer looks at a set's files again once they may have changed under it:
// after DeleteMocksForSet (a re-record), and in its next session (after
// Close: PromoteStagedSet and DiscardStagedSet close it too). A yaml writer
// that added to a gob set and kept writing gob there after the set was
// deleted would record the new set in a format it was not configured for.
func TestInsertMock_ASetsFilesAreLookedAtAgainOnceTheyMayHaveChanged(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	for _, c := range []struct {
		name  string
		clear func(t *testing.T, ys *MockYaml, dir string)
	}{
		{"deleted", func(t *testing.T, ys *MockYaml, _ string) {
			if err := ys.DeleteMocksForSet(context.Background(), "set-0"); err != nil {
				t.Fatal(err)
			}
		}},
		{"next session", func(t *testing.T, ys *MockYaml, dir string) {
			closeOK(t, ys)
			if err := os.Remove(filepath.Join(dir, "set-0", "mocks.gob")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			rec := newMockYamlForFormat(dir, mockFormatGob)
			insertURLs(t, rec, "set-0", "http://a")
			closeOK(t, rec)

			ys := newMockYamlForFormat(dir, "yaml")
			insertURLs(t, ys, "set-0", "http://b")
			closeOK(t, ys)
			wantURLs(t, dir, "set-0", "http://a", "http://b")

			// The writer has found the set to write in gob (as an insert into
			// it does), with no mock queued that would land after the change.
			if useGob, _, err := ys.insertFormat(context.Background(), filepath.Join(dir, "set-0"), "mocks"); err != nil || !useGob {
				t.Fatalf("insertFormat = %v, %v; want gob", useGob, err)
			}
			c.clear(t, ys, dir)
			insertURLs(t, ys, "set-0", "http://c")
			closeOK(t, ys)
			if got := setFiles(t, dir, "set-0"); !reflect.DeepEqual(got, []string{"mocks.yaml"}) {
				t.Fatalf("set-0 holds %v, want only mocks.yaml", got)
			}
			wantURLs(t, dir, "set-0", "http://c")
		})
	}
}

// What a carry puts in mocks.gob's place already holds every mock the set
// held, written to the disk before the replace: a kill, an OOM or a second
// Ctrl+C right after it leaves the set whole. Put in place before the
// writer's buffer is written out, it was an empty file until the next flush:
// a stop in that window left the set as empty as the truncation did.
func TestInsertMock_TheFilePutInTheSetsPlaceHoldsItsMocks(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", mockFormatGob)
	dir := t.TempDir()
	rec := New(zap.NewNop(), dir, "mocks")
	insertURLs(t, rec, "set-0", "http://a", "http://b", "http://c")
	closeOK(t, rec)

	var atReplace []string
	defer func(r func(*zap.Logger, string, string) error) { replaceFile = r }(replaceFile)
	saved := replaceFile
	replaceFile = func(l *zap.Logger, src, dst string) error {
		ms, _, err := readGobMocksCut(src)
		if err != nil {
			t.Errorf("the file put in the set's place does not read: %v", err)
		}
		for _, m := range ms {
			atReplace = append(atReplace, m.Spec.HTTPReq.URL)
		}
		return saved(l, src, dst)
	}
	add := New(zap.NewNop(), dir, "mocks")
	add.SetCounterID(3)
	insertURLs(t, add, "set-0", "http://d")
	closeOK(t, add)
	if want := []string{"http://a", "http://b", "http://c"}; !reflect.DeepEqual(atReplace, want) {
		t.Fatalf("at the replace the file held %v, want the set's %v", atReplace, want)
	}
	wantURLs(t, dir, "set-0", "http://a", "http://b", "http://c", "http://d")
}

// A carry whose replace fails (on Windows, a mocks.gob another process holds
// open) leaves the set as it was: its mocks.gob byte for byte, with no temp
// file beside it, and Close says the new mock was not written. Had the
// writer gone on into the old file, it would have written its own stream
// after the old one, which a reader cannot read past.
func TestInsertMock_AReplaceThatFailsLeavesTheSetAsItWas(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", mockFormatGob)
	dir := t.TempDir()
	rec := New(zap.NewNop(), dir, "mocks")
	insertURLs(t, rec, "set-0", "http://a", "http://b", "http://c")
	closeOK(t, rec)
	path := filepath.Join(dir, "set-0", "mocks.gob")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	defer func(r func(*zap.Logger, string, string) error) { replaceFile = r }(replaceFile)
	replaceFile = func(*zap.Logger, string, string) error { return errors.New("the file is held open") }
	add := New(zap.NewNop(), dir, "mocks")
	add.SetCounterID(3)
	insertURLs(t, add, "set-0", "http://d")
	if err := add.Close(); !errors.Is(err, models.ErrMocksNotWritten) {
		t.Fatalf("Close = %v, want ErrMocksNotWritten", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("mocks.gob changed (%d -> %d bytes)", len(before), len(after))
	}
	wantURLs(t, dir, "set-0", "http://a", "http://b", "http://c")
	if got := setFiles(t, dir, "set-0"); !reflect.DeepEqual(got, []string{"mocks.gob"}) {
		t.Fatalf("set-0 holds %v, want mocks.gob alone", got)
	}
}

// An empty mocks.gob, which the truncating writer left in sets it was killed
// in, holds no mock to keep: it takes new mocks, rather than refusing them as
// a file it cannot read.
func TestInsertMock_AnEmptyGobFileTakesNewMocks(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", mockFormatGob)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "set-0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "set-0", "mocks.gob"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	add := New(zap.NewNop(), dir, "mocks")
	insertURLs(t, add, "set-0", "http://d")
	closeOK(t, add)
	wantURLs(t, dir, "set-0", "http://d")
	if got := setFiles(t, dir, "set-0"); !reflect.DeepEqual(got, []string{"mocks.gob"}) {
		t.Fatalf("set-0 holds %v, want mocks.gob alone", got)
	}
}
