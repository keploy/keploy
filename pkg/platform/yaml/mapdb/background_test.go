package mapdb

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// MarkBackground writes a judging's background mark on the named test's
// entry only, keeps every other entry and section, takes a mark off with an
// empty line, and writes nothing when no entry changes.
func TestMarkBackgroundMarksOnlyTheNamedTestsEntry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
		"test-1": {{Name: "mock-1", Kind: "Http"}, {Name: "mock-2", Kind: "Mongo"}},
		"test-2": {{Name: "mock-2", Kind: "Mongo"}, {Name: "mock-3", Kind: "Http"}},
	}); err != nil {
		t.Fatal(err)
	}
	worker := []models.MockEntry{{Name: "w-1", Kind: "Http"}}
	if err := db.UpsertWorker(ctx, "test-set-0", worker); err != nil {
		t.Fatal(err)
	}
	entries := func() map[string]map[string]string {
		t.Helper()
		m, err := db.GetMapping(ctx, "test-set-0")
		if err != nil || m == nil {
			t.Fatalf("GetMapping = %v, %v", m, err)
		}
		if !reflect.DeepEqual(m.Worker, worker) {
			t.Fatalf("worker scope %+v, want %+v kept", m.Worker, worker)
		}
		got := map[string]map[string]string{}
		for _, tc := range m.TestCases {
			got[tc.ID] = map[string]string{}
			for _, e := range tc.Mocks {
				got[tc.ID][e.Name] = e.Kind + "|" + e.Background
			}
		}
		return got
	}

	changed, err := db.MarkBackground(ctx, "test-set-0", map[string]map[string]string{
		"test-1": {"mock-2": "count_varied: why", "mock-9": "no such recording"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changed, []string{"mock-2"}) {
		t.Fatalf("changed %v, want [mock-2]", changed)
	}
	want := map[string]map[string]string{
		"test-1": {"mock-1": "Http|", "mock-2": "Mongo|count_varied: why"},
		"test-2": {"mock-2": "Mongo|", "mock-3": "Http|"},
	}
	if got := entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after marking: %v, want %v", got, want)
	}

	// The same marks again change nothing, and the file is not written.
	path := filepath.Join(dir, "test-set-0", "mappings.yaml")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err = db.MarkBackground(ctx, "test-set-0", map[string]map[string]string{"test-1": {"mock-2": "count_varied: why"}})
	if err != nil || len(changed) != 0 {
		t.Fatalf("marking again: changed %v, err %v; want nothing changed", changed, err)
	}
	// A write replaces the file (a rename over it): the same file is unwritten.
	if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
		t.Fatalf("marking again rewrote the file (err %v)", err)
	}

	// An empty line takes the mark off.
	changed, err = db.MarkBackground(ctx, "test-set-0", map[string]map[string]string{"test-1": {"mock-2": ""}})
	if err != nil || !reflect.DeepEqual(changed, []string{"mock-2"}) {
		t.Fatalf("taking the mark off: changed %v, err %v; want [mock-2]", changed, err)
	}
	want["test-1"]["mock-2"] = "Mongo|"
	if got := entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after taking the mark off: %v, want %v", got, want)
	}

	// A set with no mappings file has nothing to mark.
	if _, err := db.MarkBackground(ctx, "test-set-missing", map[string]map[string]string{"test-1": {"mock-1": "x"}}); err == nil {
		t.Fatal("marking a set with no mappings file: no error")
	}
}

// RemoveBackground takes a recording off the named test's list only while
// its entry there still carries a background mark, keeps every other entry
// and section, writes nothing when nothing is taken off, and names as
// unlisted only what no list names any more.
func TestRemoveBackgroundTakesOnlyAMarkedEntryOffItsTestsList(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
		"test-1": {{Name: "mock-1", Kind: "Http"}, {Name: "tick", Kind: "Mongo"}, {Name: "shared", Kind: "Mongo"}, {Name: "sweep", Kind: "Mongo"}, {Name: "w-1", Kind: "Http"}},
		"test-2": {{Name: "shared", Kind: "Mongo"}, {Name: "mock-3", Kind: "Http"}},
	}); err != nil {
		t.Fatal(err)
	}
	worker := []models.MockEntry{{Name: "w-1", Kind: "Http"}}
	if err := db.UpsertWorker(ctx, "test-set-0", worker); err != nil {
		t.Fatal(err)
	}
	if _, err := db.MarkBackground(ctx, "test-set-0", map[string]map[string]string{
		"test-1": {"tick": "a timer", "shared": "a timer", "w-1": "a timer"},
	}); err != nil {
		t.Fatal(err)
	}
	lists := func() map[string][]string {
		t.Helper()
		m, err := db.GetMapping(ctx, "test-set-0")
		if err != nil || m == nil {
			t.Fatalf("GetMapping = %v, %v", m, err)
		}
		if !reflect.DeepEqual(m.Worker, worker) {
			t.Fatalf("worker scope %+v, want %+v kept", m.Worker, worker)
		}
		got := map[string][]string{}
		for _, tc := range m.TestCases {
			for _, e := range tc.Mocks {
				got[tc.ID] = append(got[tc.ID], e.Name+"|"+e.Background)
			}
		}
		return got
	}

	// sweep carries no mark: it stays. mock-3 is test-2's, not test-1's.
	removed, unlisted, err := db.RemoveBackground(ctx, "test-set-0", map[string][]string{
		"test-1": {"tick", "shared", "sweep", "w-1", "mock-3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{"shared", "tick", "w-1"}) || !reflect.DeepEqual(unlisted, []string{"tick"}) {
		t.Fatalf("removed %v, unlisted %v; want [shared tick w-1], [tick] (test-2 lists shared, the worker scope w-1)", removed, unlisted)
	}
	want := map[string][]string{
		"test-1": {"mock-1|", "sweep|"},
		"test-2": {"shared|", "mock-3|"},
	}
	if got := lists(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after removing: %v, want %v", got, want)
	}

	// Nothing marked to take off: the file is not written.
	path := filepath.Join(dir, "test-set-0", "mappings.yaml")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	removed, unlisted, err = db.RemoveBackground(ctx, "test-set-0", map[string][]string{"test-1": {"sweep", "tick"}})
	if err != nil || removed != nil || unlisted != nil {
		t.Fatalf("removing again: %v, %v, %v; want nothing", removed, unlisted, err)
	}
	if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
		t.Fatalf("removing nothing rewrote the file (err %v)", err)
	}

	if _, _, err := db.RemoveBackground(ctx, "test-set-missing", map[string][]string{"test-1": {"tick"}}); err == nil {
		t.Fatal("removing from a set with no mappings file: no error")
	}
}
