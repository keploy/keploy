package replay

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// recordingMappingDB captures what the backfill decides to write, so the tests
// can assert on the DOCUMENT rather than on side effects.
type recordingMappingDB struct {
	startupOnDisk []models.MockEntry
	startupErr    error
	inserted      []*models.Mapping
	// replaced[i] is the refresh intent Insert was called with for
	// inserted[i]. Recorded so a call site cannot silently start
	// republishing a run's per-test consumption: replace=true on this path
	// would overwrite the operator's curated lists with whatever a subset or
	// partly-failed run happened to observe.
	replaced []bool
}

func (f *recordingMappingDB) Insert(_ context.Context, m *models.Mapping, replace bool) error {
	f.inserted = append(f.inserted, m)
	f.replaced = append(f.replaced, replace)
	return nil
}
func (f *recordingMappingDB) Exists(context.Context, string) (bool, error) { return true, nil }
func (f *recordingMappingDB) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	return map[string][]models.MockEntry{}, false, nil
}
func (f *recordingMappingDB) GetStartup(context.Context, string) ([]models.MockEntry, error) {
	return f.startupOnDisk, f.startupErr
}

// The gate that made the whole feature unreachable in the default flow, and the
// clobber the first fix for it introduced. Exercises the decision directly:
// backfillStartupSection is the extracted form of the block in RunTestSet.
func TestStartupBackfill(t *testing.T) {
	captured := []models.MockEntry{{Name: "boot-0", Kind: "Postgres"}}
	perTest := []models.MappedTestCase{
		{ID: "test-A", Mocks: []models.MockEntry{{Name: "mock-1"}}},
	}

	t.Run("writes a STARTUP-ONLY document, never the per-test entries", func(t *testing.T) {
		db := &recordingMappingDB{}
		r := &Replayer{logger: zap.NewNop(), mappingDB: db}

		r.backfillStartupSection(context.Background(), "test-set-0", &models.Mapping{
			Version: "v1", Kind: "Mappings", TestSetID: "test-set-0",
			TestCases: perTest, Startup: captured,
		})

		if len(db.inserted) != 1 {
			t.Fatalf("expected exactly one write, got %d", len(db.inserted))
		}
		got := db.inserted[0]
		// THE point of this test. A document carrying TestCases would publish
		// this run's per-test consumption — on a subset or partly-failed run,
		// a strict subset — and the per-test list is the allow-list the agent
		// enforces, so a stale name there makes a foreign mock servable.
		if len(got.TestCases) != 0 {
			t.Fatalf("backfill must not carry per-test entries, got %+v", got.TestCases)
		}
		// And it must go in as a report, not a refresh. replace=true would
		// overwrite the operator's curated lists rather than leave them alone,
		// which is the whole reason this writes a startup-only document.
		if db.replaced[0] {
			t.Fatal("backfill must not request a refresh — replace=true overwrites curated per-test lists")
		}
		if len(got.Startup) != 1 || got.Startup[0].Name != "boot-0" {
			t.Fatalf("startup section not written: %+v", got.Startup)
		}
		if got.TestSetID != "test-set-0" {
			t.Fatalf("wrong test set: %q", got.TestSetID)
		}
	})

	t.Run("does not fire when the section is already on disk", func(t *testing.T) {
		db := &recordingMappingDB{startupOnDisk: []models.MockEntry{{Name: "boot-0"}}}
		r := &Replayer{logger: zap.NewNop(), mappingDB: db}

		r.backfillStartupSection(context.Background(), "test-set-0", &models.Mapping{
			TestCases: perTest, Startup: captured,
		})

		if len(db.inserted) != 0 {
			t.Fatalf("must not rewrite when the section exists — churn on every run: %+v", db.inserted)
		}
	})

	t.Run("does not fire when nothing was captured", func(t *testing.T) {
		db := &recordingMappingDB{}
		r := &Replayer{logger: zap.NewNop(), mappingDB: db}
		r.backfillStartupSection(context.Background(), "test-set-0", &models.Mapping{TestCases: perTest})
		if len(db.inserted) != 0 {
			t.Fatalf("nothing captured, nothing to write: %+v", db.inserted)
		}
	})

	// A read failure must cost the section, not the run — and must not write
	// blind, which could clobber a section it simply failed to see.
	t.Run("a GetStartup failure skips the write", func(t *testing.T) {
		db := &recordingMappingDB{startupErr: errors.New("boom")}
		r := &Replayer{logger: zap.NewNop(), mappingDB: db}
		r.backfillStartupSection(context.Background(), "test-set-0", &models.Mapping{Startup: captured})
		if len(db.inserted) != 0 {
			t.Fatalf("must not write blind after a read failure: %+v", db.inserted)
		}
	})

	t.Run("nil mappingDB is a no-op", func(t *testing.T) {
		r := &Replayer{logger: zap.NewNop()}
		r.backfillStartupSection(context.Background(), "test-set-0", &models.Mapping{Startup: captured})
	})
}
