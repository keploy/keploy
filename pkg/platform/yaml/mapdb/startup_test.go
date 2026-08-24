package mapdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// Round-trips the startup section through Insert -> on-disk YAML -> GetStartup,
// and asserts Get() still reports only per-test entries.
func TestStartupSectionRoundTripsThroughDisk(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")

	in := &models.Mapping{
		Version:   string(models.GetVersion()),
		Kind:      models.MappingKind,
		TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{
			{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-5", Kind: "Postgres"}}},
		},
		Startup: []models.MockEntry{
			{Name: "mock-0", Kind: "Postgres", Timestamp: 11},
			{Name: "mock-1", Kind: "Redis", Timestamp: 12},
		},
	}
	if err := db.Insert(context.Background(), in); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := db.GetStartup(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("GetStartup: %v", err)
	}
	if len(got) != 2 || got[0].Name != "mock-0" || got[1].Name != "mock-1" {
		t.Fatalf("startup section did not survive the round trip: %+v", got)
	}
	if got[0].Kind != "Postgres" {
		t.Fatalf("startup entry lost its Kind: %+v", got[0])
	}

	// The per-test map must be unaffected — startup mocks are not test mocks.
	perTest, meaningful, err := db.Get(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !meaningful {
		t.Fatalf("per-test mappings should still be meaningful")
	}
	if len(perTest["test-1"]) != 1 || perTest["test-1"][0].Name != "mock-5" {
		t.Fatalf("per-test mapping disturbed by the startup section: %+v", perTest)
	}
	if _, leaked := perTest["mock-0"]; leaked {
		t.Fatalf("startup mock leaked into the per-test map")
	}
}

// Every mapping written before the section existed must read back as nil, not
// as an error — otherwise determineMockingStrategy logs a failure on every
// pre-existing test set.
func TestGetStartupOnMappingWithoutSection(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")

	if err := db.Insert(context.Background(), &models.Mapping{
		Version:   string(models.GetVersion()),
		Kind:      models.MappingKind,
		TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{{ID: "t", Mocks: []models.MockEntry{{Name: "m"}}}},
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := db.GetStartup(context.Background(), "test-set-0")
	if err != nil {
		t.Fatalf("legacy mapping must not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no startup mocks, got %+v", got)
	}
}

// A test set with no mappings.yaml at all.
func TestGetStartupOnMissingFile(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "mappings")
	got, err := db.GetStartup(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// omitempty: a mapping with no startup mocks must not gain a `startup:` key, so
// existing files stay byte-identical.
func TestStartupKeyOmittedWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	if err := db.Insert(context.Background(), &models.Mapping{
		Version:   string(models.GetVersion()),
		Kind:      models.MappingKind,
		TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{{ID: "t", Mocks: []models.MockEntry{{Name: "m"}}}},
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "test-set-0", "mappings.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if containsKey(string(raw), "startup:") {
		t.Fatalf("empty startup section must be omitted, got:\n%s", raw)
	}
}

func containsKey(s, key string) bool {
	for i := 0; i+len(key) <= len(s); i++ {
		if s[i:i+len(key)] == key {
			return true
		}
	}
	return false
}

// Insert flattens the mapping to a per-test map and rebuilds it, so the startup
// section has to be carried across explicitly. A second Insert that supplies no
// startup mocks must not wipe the section written by the first.
func TestInsertPreservesStartupWhenCallerSuppliesNone(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	ctx := context.Background()

	if err := db.Insert(ctx, &models.Mapping{
		Version: string(models.GetVersion()), Kind: models.MappingKind, TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-5"}}}},
		Startup:   []models.MockEntry{{Name: "mock-0", Kind: "Postgres"}},
	}); err != nil {
		t.Fatalf("first Insert: %v", err)
	}

	// Second write carries test entries only.
	if err := db.Insert(ctx, &models.Mapping{
		Version: string(models.GetVersion()), Kind: models.MappingKind, TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{{ID: "test-2", Mocks: []models.MockEntry{{Name: "mock-6"}}}},
	}); err != nil {
		t.Fatalf("second Insert: %v", err)
	}

	got, err := db.GetStartup(ctx, "test-set-0")
	if err != nil {
		t.Fatalf("GetStartup: %v", err)
	}
	if len(got) != 1 || got[0].Name != "mock-0" {
		t.Fatalf("startup section was wiped by a startup-less Insert: %+v", got)
	}
}

// A caller that does supply startup mocks is the fresh authority for the set.
func TestInsertReplacesStartupWhenCallerSuppliesSome(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	ctx := context.Background()

	for _, name := range []string{"old-mock", "new-mock"} {
		if err := db.Insert(ctx, &models.Mapping{
			Version: string(models.GetVersion()), Kind: models.MappingKind, TestSetID: "test-set-0",
			TestCases: []models.MappedTestCase{{ID: "t", Mocks: []models.MockEntry{{Name: "m"}}}},
			Startup:   []models.MockEntry{{Name: name}},
		}); err != nil {
			t.Fatalf("Insert %s: %v", name, err)
		}
	}

	got, err := db.GetStartup(ctx, "test-set-0")
	if err != nil {
		t.Fatalf("GetStartup: %v", err)
	}
	if len(got) != 1 || got[0].Name != "new-mock" {
		t.Fatalf("expected the newer capture to win, got %+v", got)
	}
}

// UpsertBatch decodes into *models.Mapping and mutates TestCases in place, so
// the section must survive without any special handling. Pinned so a future
// refactor to the flatten-and-rebuild shape is caught.
func TestUpsertBatchPreservesStartup(t *testing.T) {
	dir := t.TempDir()
	db := New(zap.NewNop(), dir, "mappings")
	ctx := context.Background()

	if err := db.Insert(ctx, &models.Mapping{
		Version: string(models.GetVersion()), Kind: models.MappingKind, TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-5"}}}},
		Startup:   []models.MockEntry{{Name: "mock-0"}},
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := db.UpsertBatch(ctx, "test-set-0", map[string][]models.MockEntry{
		"test-2": {{Name: "mock-7"}},
	}); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}

	got, err := db.GetStartup(ctx, "test-set-0")
	if err != nil {
		t.Fatalf("GetStartup: %v", err)
	}
	if len(got) != 1 || got[0].Name != "mock-0" {
		t.Fatalf("UpsertBatch dropped the startup section: %+v", got)
	}
}
