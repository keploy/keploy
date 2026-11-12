package replay

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

type fakeMappingDB struct {
	mappings   map[string][]models.MockEntry
	meaningful bool
	getErr     error
	startup    []models.MockEntry
	startupErr error
}

func (f *fakeMappingDB) Insert(context.Context, *models.Mapping) error { return nil }
func (f *fakeMappingDB) Exists(context.Context, string) (bool, error)  { return true, nil }
func (f *fakeMappingDB) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	return f.mappings, f.meaningful, f.getErr
}
func (f *fakeMappingDB) GetStartup(context.Context, string) ([]models.MockEntry, error) {
	return f.startup, f.startupErr
}

// workerMappingDB is fakeMappingDB that also reads the whole mapping
// (mappingReader), and with it the set's worker scope. fakeMappingDB itself
// stays a store that cannot, as other tests rely on.
type workerMappingDB struct {
	*fakeMappingDB
	worker     []models.MockEntry
	mappingErr error
}

func (f *workerMappingDB) GetMapping(context.Context, string) (*models.Mapping, error) {
	if f.mappingErr != nil {
		return nil, f.mappingErr
	}
	return &models.Mapping{Startup: f.startup, Worker: f.worker}, nil
}

func newReplayerWithMappingDB(db MappingDB) *Replayer {
	return &Replayer{logger: zap.NewNop(), mappingDB: db}
}

func TestDetermineMockingStrategyStartupNames(t *testing.T) {
	startup := []models.MockEntry{{Name: "boot-0"}, {Name: ""}, {Name: "boot-1"}}

	t.Run("meaningful mappings return names alongside the map", func(t *testing.T) {
		r := newReplayerWithMappingDB(&fakeMappingDB{
			mappings:   map[string][]models.MockEntry{"t": {{Name: "m"}}},
			meaningful: true,
			startup:    startup,
		})
		useMapping, mappings, names := r.determineMockingStrategy(context.Background(), "set", true)
		if !useMapping {
			t.Fatal("expected mapping-based strategy")
		}
		if len(mappings) != 1 {
			t.Fatalf("mappings = %v", mappings)
		}
		// Unnamed entries are dropped.
		if len(names) != 2 || names[0] != "boot-0" || names[1] != "boot-1" {
			t.Fatalf("startup names = %v", names)
		}
	})

	// Deliberate: a set whose only traffic is startup keeps the timestamp path,
	// but the names are still returned so the caller can seed its prune maps.
	t.Run("non-meaningful mappings still return startup names", func(t *testing.T) {
		r := newReplayerWithMappingDB(&fakeMappingDB{meaningful: false, startup: startup})
		useMapping, _, names := r.determineMockingStrategy(context.Background(), "set", true)
		if useMapping {
			t.Fatal("expected timestamp strategy when no per-test entries exist")
		}
		if len(names) != 2 {
			t.Fatalf("startup names = %v", names)
		}
	})

	t.Run("a Get failure still returns startup names", func(t *testing.T) {
		r := newReplayerWithMappingDB(&fakeMappingDB{getErr: errors.New("boom"), startup: startup})
		useMapping, _, names := r.determineMockingStrategy(context.Background(), "set", true)
		if useMapping {
			t.Fatal("expected fallback to timestamp strategy")
		}
		if len(names) != 2 {
			t.Fatalf("startup names = %v", names)
		}
	})

	// A startup read failure must cost the names, not the run.
	t.Run("a GetStartup failure degrades quietly", func(t *testing.T) {
		r := newReplayerWithMappingDB(&fakeMappingDB{
			mappings:   map[string][]models.MockEntry{"t": {{Name: "m"}}},
			meaningful: true,
			startupErr: errors.New("boom"),
		})
		useMapping, _, names := r.determineMockingStrategy(context.Background(), "set", true)
		if !useMapping {
			t.Fatal("a startup read failure must not change the strategy")
		}
		if len(names) != 0 {
			t.Fatalf("expected no startup names, got %v", names)
		}
	})

	t.Run("mapping disabled reads nothing", func(t *testing.T) {
		db := &fakeMappingDB{meaningful: true, startup: startup}
		r := newReplayerWithMappingDB(db)
		useMapping, _, names := r.determineMockingStrategy(context.Background(), "set", false)
		if useMapping || len(names) != 0 {
			t.Fatalf("disabled mapping must yield no strategy and no names, got %v / %v", useMapping, names)
		}
	})

	t.Run("nil mappingDB", func(t *testing.T) {
		r := newReplayerWithMappingDB(nil)
		r.mappingDB = nil
		useMapping, _, names := r.determineMockingStrategy(context.Background(), "set", true)
		if useMapping || names != nil {
			t.Fatalf("got %v / %v", useMapping, names)
		}
	})
}

// A per-process warm-up (a worker-scope entry keploy record marked with
// models.PerProcessWarmupReason) is a call no test owns: any test may be the
// one to make it. Without the proven switch every test is handed it as the
// startup traffic is: its names join the set-wide names merged into each
// test's list (and kept out of the per-test assertion). A worker entry
// without the mark is the proven switch's, served on its rules only; under
// the switch, or with mapping disabled, the names are left alone.
func TestDetermineMockingStrategyWorkerScopeNames(t *testing.T) {
	db := func() *workerMappingDB {
		return &workerMappingDB{
			fakeMappingDB: &fakeMappingDB{
				mappings:   map[string][]models.MockEntry{"t": {{Name: "m"}}},
				meaningful: true,
				startup:    []models.MockEntry{{Name: "boot-0"}},
			},
			worker: []models.MockEntry{
				{Name: "jwks-1", Reason: models.PerProcessWarmupReason},
				{Name: "", Reason: models.PerProcessWarmupReason},
				{Name: "boot-0", Reason: models.PerProcessWarmupReason},
				{Name: "poll-0", Reason: "routed by the proven switch"},
				{Name: "poll-1"},
				{Name: "jwks-2", Reason: models.PerProcessWarmupReason},
			},
		}
	}

	t.Run("without the switch the worker scope joins the set-wide names", func(t *testing.T) {
		r := newReplayerWithMappingDB(db())
		useMapping, _, names := r.determineMockingStrategy(context.Background(), "set", true)
		if !useMapping {
			t.Fatal("expected mapping-based strategy")
		}
		// Startup first, then the warm-ups in their order; unnamed and
		// unmarked entries dropped and a name already present not added twice.
		want := []string{"boot-0", "jwks-1", "jwks-2"}
		if len(names) != len(want) {
			t.Fatalf("names = %v, want %v", names, want)
		}
		for i := range want {
			if names[i] != want[i] {
				t.Fatalf("names = %v, want %v", names, want)
			}
		}
	})

	t.Run("under the proven switch the names are left alone", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		r := newReplayerWithMappingDB(db())
		r.instrument = true
		if !r.exactServing() {
			t.Fatal("the test needs exact serving on")
		}
		_, _, names := r.determineMockingStrategy(context.Background(), "set", true)
		if len(names) != 1 || names[0] != "boot-0" {
			t.Fatalf("names = %v, want only the startup name", names)
		}
	})

	// A set whose tests list nothing keeps the timestamp path, which reads
	// each test's own mocks; its warm-ups, session mocks on either path, do
	// not make it mapped (the mapping-based path would hand each test only
	// the names it lists).
	t.Run("a set whose tests list nothing keeps the timestamp path", func(t *testing.T) {
		f := db()
		f.meaningful = false
		f.mappings = map[string][]models.MockEntry{"t": {}}
		r := newReplayerWithMappingDB(f)
		if useMapping, _, _ := r.determineMockingStrategy(context.Background(), "set", true); useMapping {
			t.Fatal("warm-ups alone must not select the mapping-based strategy")
		}
	})

	t.Run("with mapping disabled the names are left alone", func(t *testing.T) {
		r := newReplayerWithMappingDB(db())
		r.config = &config.Config{DisableMapping: true}
		if got := r.defaultWorkerScope(context.Background(), "set"); got != nil {
			t.Fatalf("worker names = %v, want none", got)
		}
	})

	t.Run("a worker-scope read failure costs the worker names, not the run", func(t *testing.T) {
		f := db()
		f.mappingErr = errors.New("boom")
		r := newReplayerWithMappingDB(f)
		useMapping, _, names := r.determineMockingStrategy(context.Background(), "set", true)
		if !useMapping || len(names) != 1 || names[0] != "boot-0" {
			t.Fatalf("got %v / %v", useMapping, names)
		}
	})
}
