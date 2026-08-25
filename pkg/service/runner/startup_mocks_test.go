package integration

import (
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// expectedMocksForTest feeds MockFilterParams.MockMapping, which the agent
// turns into disk.LoadByNames. Startup names missing here means the app's boot
// mocks are absent from the pool for every test on this path — the same bug
// that was fixed in pkg/service/replay.
func TestExpectedMocksForTestIncludesStartup(t *testing.T) {
	setup := &testSetSetup{
		mappings: map[string][]models.MockEntry{
			"test-1": {{Name: "mock-3"}, {Name: "mock-4"}},
			"test-2": {},
		},
		startupMockNames: []string{"boot-0", "boot-1"},
	}

	t.Run("per-test names then startup names", func(t *testing.T) {
		got := expectedMocksForTest(setup, "test-1")
		want := []string{"mock-3", "mock-4", "boot-0", "boot-1"}
		assertNames(t, got, want)
	})

	// A test with no mocks of its own still needs the app to boot.
	t.Run("test with no per-test mocks still gets startup", func(t *testing.T) {
		assertNames(t, expectedMocksForTest(setup, "test-2"), []string{"boot-0", "boot-1"})
	})

	// Unknown test names used to early-return nil; they must still boot.
	t.Run("unknown test still gets startup", func(t *testing.T) {
		assertNames(t, expectedMocksForTest(setup, "does-not-exist"), []string{"boot-0", "boot-1"})
	})

	t.Run("no startup section leaves behaviour unchanged", func(t *testing.T) {
		plain := &testSetSetup{mappings: map[string][]models.MockEntry{"t": {{Name: "m"}}}}
		assertNames(t, expectedMocksForTest(plain, "t"), []string{"m"})
		if got := expectedMocksForTest(plain, "missing"); got != nil {
			t.Fatalf("expected nil for an unknown test with no startup, got %v", got)
		}
	})

	t.Run("nil setup", func(t *testing.T) {
		if got := expectedMocksForTest(nil, "t"); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
}

// GetFilteredMocks prunes on `isMappedToSpecificTest && !isNeededForCurrentRun`,
// so a startup name seeded into only the first map would be dropped.
func TestLoadMappingsForSetSeedsStartupIntoBothPruneMaps(t *testing.T) {
	r := &Runner{mappingDB: &stubMappingDB{
		mappings:      map[string][]models.MockEntry{"test-1": {{Name: "mock-3"}}},
		hasMeaningful: true,
		startup:       []models.MockEntry{{Name: "boot-0"}},
	}, logger: zap.NewNop()}

	_, mocksThatHaveMappings, mocksWeNeed, startupNames, err := r.loadMappingsForSet(t.Context(), "set-1")
	if err != nil {
		t.Fatalf("loadMappingsForSet: %v", err)
	}
	for _, n := range []string{"boot-0", "mock-3"} {
		if !mocksThatHaveMappings[n] {
			t.Fatalf("mocksThatHaveMappings missing %q", n)
		}
		if !mocksWeNeed[n] {
			t.Fatalf("mocksWeNeed missing %q — it would be pruned on a subset run", n)
		}
	}
	assertNames(t, startupNames, []string{"boot-0"})
}

// A read failure must cost the startup names, not the run.
func TestLoadMappingsForSetToleratesStartupReadFailure(t *testing.T) {
	r := &Runner{mappingDB: &stubMappingDB{
		mappings:      map[string][]models.MockEntry{"test-1": {{Name: "mock-3"}}},
		hasMeaningful: true,
		startupErr:    errBoom{},
	}, logger: zap.NewNop()}

	_, mthm, _, startupNames, err := r.loadMappingsForSet(t.Context(), "set-1")
	if err != nil {
		t.Fatalf("a startup read failure must not fail the run: %v", err)
	}
	if len(startupNames) != 0 {
		t.Fatalf("expected no startup names, got %v", startupNames)
	}
	if !mthm["mock-3"] {
		t.Fatalf("per-test mapping lost")
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

func assertNames(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
