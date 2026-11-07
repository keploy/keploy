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
