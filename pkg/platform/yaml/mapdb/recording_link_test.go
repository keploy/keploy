package mapdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A set's mappings file is read only inside the recording: one that links
// out of it is refused, with why, by every read and every change (which
// reads it first, and so writes nothing), and one that links to a file
// inside it is read.
func TestASetsMappingsAreReadOnlyInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	db := New(zap.NewNop(), root, "")
	for _, set := range []string{"set", "other"} {
		if err := db.UpsertBatch(ctx, set, map[string][]models.MockEntry{"t1": {{Name: "mock-0"}}}); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(root, "set", "mappings.yaml")
	out := filepath.Join(base, "outside-mappings.yaml")
	if err := os.Rename(file, out); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "outside-mappings.yaml"), file); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	entries := []models.MockEntry{{Name: "mock-1"}}
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"GetMapping", func() error { _, err := db.GetMapping(ctx, "set"); return err }},
		{"Get", func() error { _, _, err := db.Get(ctx, "set"); return err }},
		{"Insert", func() error {
			return db.Insert(ctx, &models.Mapping{TestSetID: "set", TestCases: []models.MappedTestCase{{ID: "t2", Mocks: entries}}})
		}},
		{"Upsert", func() error { return db.Upsert(ctx, "set", "t2", entries) }},
		{"UpsertBatch", func() error { return db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{"t2": entries}) }},
		{"UpsertWorker", func() error { return db.UpsertWorker(ctx, "set", entries) }},
		{"MoveToWorker", func() error { _, err := db.MoveToWorker(ctx, "set", []models.MockEntry{{Name: "mock-0"}}); return err }},
		{"MarkRecordedWith", func() error { return db.MarkRecordedWith(ctx, "set", "x") }},
		{"UpsertBoots", func() error { return db.UpsertBoots(ctx, "set", []models.BootSpec{{Key: "a#1", Program: "a", N: 1}}) }},
		{"UpsertCases", func() error {
			return db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{"t1": {Dir: "pkg"}}, nil, nil)
		}},
		{"UpsertConcurrency", func() error {
			return db.UpsertConcurrency(ctx, "set", []models.ConcurrencyGroup{{Tests: []string{"t1"}}}, nil)
		}},
	} {
		err := c.call()
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q is read only inside its recording %q", filepath.Join("set", "mappings.yaml"), root)) {
			t.Errorf("%s: %v, want %q refused as read only inside its recording", c.name, err, file)
		}
		if after, _ := os.ReadFile(out); string(after) != string(before) {
			t.Fatalf("%s wrote the file outside the recording", c.name)
		}
		if fi, err := os.Lstat(file); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s replaced the link: %v", c.name, err)
		}
	}

	// A link inside the recording, to the other set's mappings, is read.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "other", "mappings.yaml"), file); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetMapping(ctx, "set")
	if err != nil || len(m.TestCases) != 1 || m.TestCases[0].ID != "t1" {
		t.Fatalf("GetMapping with a link inside: %+v, %v", m, err)
	}
}

// linkOut makes a relative link at path to target, which need not exist.
func linkOut(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Dir(path), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, path); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
}

// A mappings file that links to nothing, out of the recording, is refused
// like any other link out: the write leaves the link as it is, and a set
// folder that links out gets no file, and loses none.
func TestASetsMappingsAreWrittenOnlyInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	refused := func(t *testing.T, what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "only inside its recording") {
			t.Fatalf("%s: %v, want it refused as outside the recording", what, err)
		}
	}
	byTest := map[string][]models.MockEntry{"t1": {{Name: "mock-0"}}}
	t.Run("a link to nothing", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		file := filepath.Join(root, "set", "mappings.yaml")
		linkOut(t, filepath.Join(base, "nothing.yaml"), file)
		db := New(zap.NewNop(), root, "")
		refused(t, "UpsertBatch", db.UpsertBatch(ctx, "set", byTest))
		if info, err := os.Lstat(file); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the link is gone: %v, %v", info, err)
		}
		if _, err := os.Lstat(filepath.Join(base, "nothing.yaml")); !os.IsNotExist(err) {
			t.Fatalf("the write created the file the link names: %v", err)
		}
	})
	t.Run("a set folder that links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		linkOut(t, out, filepath.Join(root, "set"))
		db := New(zap.NewNop(), root, "")
		for _, c := range []struct {
			name string
			call func() error
		}{
			{"Insert", func() error { return db.Insert(ctx, &models.Mapping{TestSetID: "set"}) }},
			{"UpsertBatch", func() error { return db.UpsertBatch(ctx, "set", byTest) }},
			{"UpsertWorker", func() error { return db.UpsertWorker(ctx, "set", []models.MockEntry{{Name: "mock-0"}}) }},
			{"UpsertConcurrency", func() error {
				return db.UpsertConcurrency(ctx, "set", []models.ConcurrencyGroup{{ID: "g", Tests: []string{"t1"}}}, nil)
			}},
			{"UpsertBoots", func() error { return db.UpsertBoots(ctx, "set", []models.BootSpec{{}}) }},
			{"UpsertCases", func() error {
				return db.UpsertCases(ctx, "set", map[string]models.MappedTestCase{"t1": {}}, nil, nil)
			}},
		} {
			refused(t, c.name, c.call())
			if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
				t.Fatalf("%s left %v (%v) outside the recording", c.name, entries, err)
			}
		}
		kept := filepath.Join(out, "mappings.yaml")
		if err := os.WriteFile(kept, []byte("KEEP ME\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		refused(t, "Delete", db.Delete(ctx, "set"))
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("Delete removed the file outside the recording: %v", err)
		}
	})
}
