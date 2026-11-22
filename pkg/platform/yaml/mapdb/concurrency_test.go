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

// The concurrency groups and shared calls are written whole by
// UpsertConcurrency, kept by every other write (UpsertBatch, UpsertWorker,
// Insert), and ApplyShares writes the full round's pins onto them: once,
// idempotently, in recorded order, dropping the groups no entry names.
func TestConcurrencyRoundTripsAndApplySharesWritesThePins(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	entry := func(name, ts, group string) models.MockEntry {
		return models.MockEntry{Name: name, Kind: "PostgresV3", ReqTimestampMock: ts, Group: group}
	}
	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
		"a": {entry("m1", "2026-10-21T10:00:01Z", "g1"), entry("m2", "2026-10-21T10:00:02Z", "g1")},
		"b": {},
	}); err != nil {
		t.Fatal(err)
	}
	groups := []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}, {ID: "g2", Tests: []string{"b"}, Unkept: 1}}
	shared := []models.MockEntry{entry("s1", "2026-10-21T10:00:03Z", "g2")}
	if err := db.UpsertConcurrency(ctx, "test-set-0", groups, shared); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertWorker(ctx, "test-set-0", []models.MockEntry{{Name: "w", Kind: "Http"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Insert(ctx, &models.Mapping{Version: string(models.GetVersion()), Kind: models.MappingKind, TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{{ID: "c", Mocks: []models.MockEntry{{Name: "own-c", Kind: "Http"}}}}}); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetMapping(ctx, "test-set-0")
	if err != nil || m == nil {
		t.Fatalf("GetMapping = %v, %v", m, err)
	}
	if !reflect.DeepEqual(m.Groups, groups) || !reflect.DeepEqual(m.Shared, shared) {
		t.Fatalf("groups %+v shared %+v after the other writes; want %+v %+v", m.Groups, m.Shared, groups, shared)
	}
	path := filepath.Join(dir, "test-set-0", "mappings.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pins := models.SetShares{State: models.SharesComplete, Pins: map[string]string{"m1": "b", "m2": "a", "s1": "b"}}
	wrote, err := db.ApplyShares(ctx, "test-set-0", pins)
	if err != nil || !wrote {
		t.Fatalf("ApplyShares = %v, %v", wrote, err)
	}
	m, err = db.GetMapping(ctx, "test-set-0")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, tc := range m.TestCases {
		for _, e := range tc.Mocks {
			got[tc.ID] = append(got[tc.ID], e.Name+"/"+e.Group)
		}
	}
	if want := map[string][]string{"a": {"m2/"}, "b": {"m1/", "s1/"}, "c": {"own-c/"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the pins %v; want %v", got, want)
	}
	if len(m.Groups) != 0 || len(m.Shared) != 0 || len(m.Worker) != 1 {
		t.Fatalf("groups %+v shared %+v worker %+v; want no group, nothing shared and the worker scope kept", m.Groups, m.Shared, m.Worker)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == string(before) {
		t.Fatal("ApplyShares wrote nothing")
	}
	wrote, err = db.ApplyShares(ctx, "test-set-0", pins)
	if err != nil || wrote {
		t.Fatalf("ApplyShares applied twice = %v, %v; want no write", wrote, err)
	}
	again, err := os.ReadFile(path)
	if err != nil || string(again) != string(after) {
		t.Fatalf("applied twice, the file changed: %v", err)
	}
	if _, err := db.ApplyShares(ctx, "no-such-set", pins); err == nil {
		t.Fatal("ApplyShares onto a set with no mapping file is no error")
	}
}

// A recording with no concurrency hands UpsertConcurrency none: a file that
// holds none is left as it is, byte for byte (the proven switch on, a set
// recorded one request at a time, writes mappings.yaml as it did); one that
// holds some from before has them cleared; a set with no file gets none.
func TestUpsertConcurrencyWithNoneChangesOnlyAFileThatHeldSome(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{"a": {{Name: "m1", Kind: "Http"}}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test-set-0", "mappings.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertConcurrency(ctx, "test-set-0", nil, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("a file with no groups changed: %v\n%s\n---\n%s", err, before, after)
	}
	if again, err := os.Stat(path); err != nil || !again.ModTime().Equal(info.ModTime()) {
		t.Fatalf("a file with no groups was written again: %v", err)
	}

	groups := []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a"}}}
	if err := db.UpsertConcurrency(ctx, "test-set-0", groups, []models.MockEntry{{Name: "s1", Group: "g1"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertConcurrency(ctx, "test-set-0", nil, nil); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetMapping(ctx, "test-set-0")
	if err != nil || m == nil {
		t.Fatalf("GetMapping = %v, %v", m, err)
	}
	if len(m.Groups) != 0 || len(m.Shared) != 0 || len(m.TestCases) != 1 {
		t.Fatalf("groups %+v shared %+v tests %+v; want the groups cleared and the tests kept", m.Groups, m.Shared, m.TestCases)
	}

	if err := db.UpsertConcurrency(ctx, "test-set-9", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "test-set-9")); !os.IsNotExist(err) {
		t.Fatalf("a set with no file got one for no groups: %v", err)
	}
}
