package mockdb

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

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
