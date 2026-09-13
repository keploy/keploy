package replay

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
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
