package replay

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

// intentRecordingMappingDB captures the refresh intent StoreMappings hands to
// Insert, so the wiring that chooses it is pinned rather than just the leaf
// that consumes it.
type intentRecordingMappingDB struct {
	replaced []bool
}

func (d *intentRecordingMappingDB) Insert(_ context.Context, _ *models.Mapping, replace bool) error {
	d.replaced = append(d.replaced, replace)
	return nil
}
func (d *intentRecordingMappingDB) Exists(context.Context, string) (bool, error) { return true, nil }
func (d *intentRecordingMappingDB) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	return nil, false, nil
}
func (d *intentRecordingMappingDB) GetStartup(context.Context, string) ([]models.MockEntry, error) {
	return nil, nil
}

// SCOPE, stated honestly: this pins the StoreMappings -> Insert forwarding
// only. It does NOT reach the caller that chooses the argument — that lives
// inside the test-set loop in replay.go, and flipping it to a literal `true`
// leaves this test green. The backfill call site IS pinned, by replaced[0] in
// startup_backfill_test.go. Covering the main one needs the loop to be
// reachable in a unit test, which it is not today.
//
// replace must mean "the operator asked for a refresh", never "this run wrote
// a file". A run that merely reports the mocks it consumed has to union: its
// observation is not necessarily complete (a subset run, a short-circuited
// run, or one degraded by an earlier mock miss all see less than the test
// needs), so writing it as authoritative truncates the pool — and because
// every later run does the same, the pool can only ever shrink.
//
// Conversely --update-test-mapping has to replace, or a wrong mapping is
// permanent: union alone can add but never correct, and MappingDb has no
// Delete.
func TestStoreMappingsPassesOperatorRefreshIntent(t *testing.T) {
	for _, tc := range []struct {
		name              string
		updateTestMapping bool
		want              bool
	}{
		{"a run reporting consumption unions", false, false},
		{"--update-test-mapping replaces", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := &intentRecordingMappingDB{}
			r := &Replayer{logger: zap.NewNop(), mappingDB: db}
			r.config = &config.Config{}
			r.config.Test.UpdateTestMapping = tc.updateTestMapping

			if err := r.StoreMappings(context.Background(), &models.Mapping{TestSetID: "set"}, r.config.Test.UpdateTestMapping); err != nil {
				t.Fatalf("StoreMappings: %v", err)
			}
			if len(db.replaced) != 1 {
				t.Fatalf("expected one Insert, got %d", len(db.replaced))
			}
			if db.replaced[0] != tc.want {
				t.Fatalf("refresh intent not carried through: got replace=%v, want %v", db.replaced[0], tc.want)
			}
		})
	}
}
