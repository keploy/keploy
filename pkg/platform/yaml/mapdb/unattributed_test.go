package mapdb

import (
	"context"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
)

func names(entries []models.MockEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

// The defect this fixes: a mock the recorder could not attribute to a test
// arrives under an EMPTY test name. Written as a test entry it is reachable by
// NOBODY — replay loads strictly by name and nothing is named "". Routed to the
// startup section it is loaded by EVERY test.
func TestUpsertBatchRoutesUnattributedMocksToStartup(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "mappings")
	ctx := context.Background()

	if err := db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{
		"post-query-1": {{Name: "mock-1", Kind: "Mongo"}},
		"":             {{Name: "groups-0", Kind: "Mongo"}, {Name: "groups-1", Kind: "Mongo"}},
	}); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}

	startup, err := db.GetStartup(ctx, "set")
	if err != nil {
		t.Fatalf("GetStartup: %v", err)
	}
	if got := names(startup); len(got) != 2 || got[0] != "groups-0" || got[1] != "groups-1" {
		t.Fatalf("unattributed mocks must land in the startup section, got %v", got)
	}

	perTest, meaningful, err := db.Get(ctx, "set")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, leaked := perTest[""]; leaked {
		t.Fatal(`an entry keyed "" is reachable by no test and must not be a per-test mapping`)
	}
	if !meaningful || len(perTest["post-query-1"]) != 1 {
		t.Fatalf("the attributed mapping must survive untouched: %v", perTest)
	}
}

// Insert takes the same route, via models.Mapping.TestCases.
func TestInsertRoutesUnattributedMocksToStartup(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "mappings")
	ctx := context.Background()

	if err := db.Insert(ctx, &models.Mapping{
		Version: string(models.GetVersion()), Kind: models.MappingKind, TestSetID: "set",
		TestCases: []models.MappedTestCase{
			{ID: "post-query-1", Mocks: []models.MockEntry{{Name: "mock-1"}}},
			{ID: "", Mocks: []models.MockEntry{{Name: "groups-0"}}},
		},
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	startup, err := db.GetStartup(ctx, "set")
	if err != nil {
		t.Fatalf("GetStartup: %v", err)
	}
	if got := names(startup); len(got) != 1 || got[0] != "groups-0" {
		t.Fatalf("startup section = %v", got)
	}
}

// Routed entries are ADDITIVE. A caller that also supplies a real startup
// section must not have it replaced by the rescued ones.
func TestRoutedUnattributedMocksUnionWithAnExistingStartupSection(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "mappings")
	ctx := context.Background()

	if err := db.Insert(ctx, &models.Mapping{
		Version: string(models.GetVersion()), Kind: models.MappingKind, TestSetID: "set",
		TestCases: []models.MappedTestCase{
			{ID: "t", Mocks: []models.MockEntry{{Name: "mock-1"}}},
			{ID: "", Mocks: []models.MockEntry{{Name: "rescued-0"}}},
		},
		Startup: []models.MockEntry{{Name: "boot-0"}},
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	startup, err := db.GetStartup(ctx, "set")
	if err != nil {
		t.Fatalf("GetStartup: %v", err)
	}
	got := names(startup)
	if len(got) != 2 || got[0] != "boot-0" || got[1] != "rescued-0" {
		t.Fatalf("expected the boot capture and the rescued entry, got %v", got)
	}
}

// REPAIR PATH. A mapping ALREADY on disk in the old shape — the staging
// recording that surfaced this carried 448 of 549 mocks that way — must become
// replayable without a re-record.
func TestLegacyUnattributedEntryIsReadAsStartup(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	ctx := context.Background()

	// Write the legacy shape directly, bypassing the fixed write paths.
	legacy := CreateMappingStructure("set", map[string][]models.MockEntry{
		"post-query-1": {{Name: "mock-1"}},
		"":             {{Name: "groups-0"}, {Name: "groups-1"}},
	}, zap.NewNop())
	encoded, err := EncodeMappingF(legacy, zap.NewNop(), db.Format)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := writeLegacy(ctx, db, dir, encoded); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	startup, err := db.GetStartup(ctx, "set")
	if err != nil {
		t.Fatalf("GetStartup: %v", err)
	}
	if len(startup) != 2 {
		t.Fatalf("a legacy \"\" entry must be readable as startup so the set replays; got %v", names(startup))
	}

	perTest, _, err := db.Get(ctx, "set")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, leaked := perTest[""]; leaked {
		t.Fatal(`Get must not surface the "" entry as a per-test mapping`)
	}
}

// The dangerous corner: a set whose ONLY non-empty entry is the unattributed
// one. Reporting it as meaningful switches replay to by-name loading for a set
// whose every mock is unreachable — strictly worse than the timestamp fallback.
func TestUnattributedOnlyMappingIsNotMeaningful(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	ctx := context.Background()

	legacy := CreateMappingStructure("set", map[string][]models.MockEntry{
		"": {{Name: "groups-0"}},
	}, zap.NewNop())
	encoded, err := EncodeMappingF(legacy, zap.NewNop(), db.Format)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := writeLegacy(ctx, db, dir, encoded); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	_, meaningful, err := db.Get(ctx, "set")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if meaningful {
		t.Fatal("a mapping whose only entry is unattributed must fall back to timestamp filtering, not by-name loading")
	}
	startup, err := db.GetStartup(ctx, "set")
	if err != nil || len(startup) != 1 {
		t.Fatalf("the mocks themselves must still be reachable via startup: %v (err %v)", names(startup), err)
	}
}

// writeLegacy drops an encoded mapping straight onto disk, so a test can build
// the pre-fix shape that the write paths no longer produce.
func writeLegacy(ctx context.Context, db *MappingDb, dir string, encoded []byte) error {
	return yaml.WriteFileF(ctx, zap.NewNop(), filepath.Join(dir, "set"), "mappings", encoded, false, db.Format)
}
