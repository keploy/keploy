package replay

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
)

// Spans setStartupMocks -> Insert -> GetStartup.
//
// NOTE ON WHAT THIS DOES AND DOES NOT PIN: mapdb.Insert and GetStartup already
// worked before the backfill fix, so steps 1-3 and 5 pass on the old code too.
// The bug was never in mapdb — it was the shouldWriteMappings gate in
// RunTestSet, which is covered by TestStartupBackfill* below. This test guards
// the serialization contract between the two halves; that one guards the gate.
func TestStartupSectionSurvivesWriteThenRead(t *testing.T) {
	dir := t.TempDir()
	db := mapdb.NewWithFormat(zap.NewNop(), dir, "mappings", yaml.FormatYAML)
	ctx := context.Background()

	// 1. WRITE — exactly what RunTestSet does with the boot-time drain.
	mapping := &models.Mapping{
		Version:   string(models.GetVersion()),
		Kind:      models.MappingKind,
		TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{
			{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-5", Kind: "Postgres"}}},
		},
	}
	setStartupMocks(mapping, []models.MockState{
		{Name: "boot-0", Kind: models.Postgres, ReqTimestampMock: "2026-08-25T10:00:00Z"},
		{Name: "boot-1", Kind: models.REDIS},
	})
	if len(mapping.Startup) != 2 {
		t.Fatalf("setStartupMocks did not capture: %+v", mapping.Startup)
	}

	if err := db.Insert(ctx, mapping); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// 2. READ BACK through the same API determineMockingStrategy uses.
	got, err := db.GetStartup(ctx, "test-set-0")
	if err != nil {
		t.Fatalf("GetStartup: %v", err)
	}
	if len(got) != 2 || got[0].Name != "boot-0" || got[1].Name != "boot-1" {
		t.Fatalf("section did not survive write -> disk -> read: %+v", got)
	}
	if got[0].Kind != string(models.Postgres) {
		t.Fatalf("Kind lost across the round trip: %+v", got[0])
	}

	// 3. The names reach the agent for a test that HAS per-test mocks.
	names := models.MergeStartupMockNames(
		[]models.MockEntry{{Name: "mock-5"}},
		(&models.Mapping{Startup: got}).StartupMockNames(),
	)
	want := []string{"mock-5", "boot-0", "boot-1"}
	if len(names) != len(want) {
		t.Fatalf("got %v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v want %v", names, want)
		}
	}

	// 4. ...and NOT for a test with no per-test mocks, which must keep the wide
	//    pool rather than being narrowed to boot mocks only.
	if n := models.MergeStartupMockNames(nil, (&models.Mapping{Startup: got}).StartupMockNames()); len(n) != 0 {
		t.Fatalf("a test with no per-test entry must stay empty, got %v", n)
	}

	// 5. Per-test mappings are undisturbed by the section.
	perTest, meaningful, err := db.Get(ctx, "test-set-0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !meaningful || len(perTest["test-1"]) != 1 || perTest["test-1"][0].Name != "mock-5" {
		t.Fatalf("per-test mapping disturbed: %+v", perTest)
	}
}
