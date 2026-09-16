package replay

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

// intentRecordingMappingDB captures the refresh intent each Insert was called
// with, so the DECISION that picks it is pinned — not just the plumbing that
// forwards it.
type intentRecordingMappingDB struct {
	exists    bool
	existsErr error
	inserted  []*models.Mapping
	replaced  []bool
}

func (d *intentRecordingMappingDB) Insert(_ context.Context, m *models.Mapping, replace bool) error {
	d.inserted = append(d.inserted, m)
	d.replaced = append(d.replaced, replace)
	return nil
}
func (d *intentRecordingMappingDB) Exists(context.Context, string) (bool, error) {
	return d.exists, d.existsErr
}
func (d *intentRecordingMappingDB) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	return nil, false, nil
}
func (d *intentRecordingMappingDB) GetStartup(context.Context, string) ([]models.MockEntry, error) {
	return nil, nil
}

func replayerWith(db MappingDB, updateTestMapping bool) *Replayer {
	r := &Replayer{logger: zap.NewNop(), mappingDB: db}
	r.config = &config.Config{}
	r.config.Test.UpdateTestMapping = updateTestMapping
	return r
}

func withOneTest() *models.Mapping {
	return &models.Mapping{
		TestSetID: "set",
		TestCases: []models.MappedTestCase{{ID: "test-A", Mocks: []models.MockEntry{{Name: "mock-1"}}}},
	}
}

// persistMappings picks the refresh intent, and a wrong value here silently
// restores the truncation bug: every write becomes an overwrite, so one subset
// or partly-failed run can wipe a curated per-test list.
func TestPersistMappingsChoosesRefreshIntent(t *testing.T) {
	t.Run("--update-test-mapping replaces", func(t *testing.T) {
		db := &intentRecordingMappingDB{exists: true}
		replayerWith(db, true).persistMappings(context.Background(), "set", withOneTest())

		if len(db.replaced) != 1 {
			t.Fatalf("expected one write, got %d", len(db.replaced))
		}
		if !db.replaced[0] {
			t.Fatal("an explicit refresh must replace, or a wrong mapping can never be corrected")
		}
	})

	t.Run("create-if-absent reports, it does not refresh", func(t *testing.T) {
		db := &intentRecordingMappingDB{exists: false}
		replayerWith(db, false).persistMappings(context.Background(), "set", withOneTest())

		if len(db.replaced) != 1 {
			t.Fatalf("expected the create-if-absent write, got %d", len(db.replaced))
		}
		if db.replaced[0] {
			t.Fatal("a run reporting its own consumption must union — replace=true lets one run truncate the pool")
		}
	})
}

// The file already exists and no refresh was asked for: the per-test entries
// this run observed must not be published at all.
func TestPersistMappingsSkipsWriteWhenFileExists(t *testing.T) {
	db := &intentRecordingMappingDB{exists: true}
	replayerWith(db, false).persistMappings(context.Background(), "set", withOneTest())

	for _, m := range db.inserted {
		if len(m.TestCases) != 0 {
			t.Fatalf("published per-test entries without a refresh: %+v", m.TestCases)
		}
	}
}

// An Exists error is treated as "exists" so a failed check cannot clobber a
// mapping that is really there.
func TestPersistMappingsTreatsExistsErrorAsExists(t *testing.T) {
	db := &intentRecordingMappingDB{existsErr: errors.New("stat failed")}
	replayerWith(db, false).persistMappings(context.Background(), "set", withOneTest())

	for _, m := range db.inserted {
		if len(m.TestCases) != 0 {
			t.Fatalf("wrote per-test entries after a failed existence check: %+v", m.TestCases)
		}
	}
}
