// postgres_autopromote_test.go pins the cross-window autopromote
// glue on the agent layer. The integrations side has its own coverage
// for EvaluateCrossWindowAutopromote (heuristic correctness); these
// tests focus on the applier's job: route promoted mocks out of the
// per-test pool, mutate Lifetime in place so downstream readers see
// a single source of truth, and respect the env off-switch.
package agent

import (
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// resetCrossWindowOnceForTest re-arms the once-resolved env gate so
// a test that flips KEPLOY_PG_V3_CROSS_WINDOW_AUTOPROMOTE via
// t.Setenv is observed instead of an earlier-test-cached value.
//
// Each test that touches the gate MUST register a t.Cleanup that
// re-resets the once after the test exits — otherwise an earlier
// override survives via the memo'd crossWindowEnabled and the next
// test sees stale state.
func resetCrossWindowOnceForTest(t *testing.T) {
	t.Helper()
	doResetCrossWindow()
	t.Cleanup(doResetCrossWindow)
}

func doResetCrossWindow() {
	crossWindowEnabledOnce = sync.Once{}
	crossWindowEnabled = false
}

func mkTestPgMock(name, sqlAstHash, sqlNorm string, binds []string, payload string) *models.Mock {
	cells := make(models.PostgresV3Cells, len(binds))
	for i, b := range binds {
		cells[i] = models.NewValueCell(b)
	}
	oids := make([]uint32, len(binds))
	for i := range oids {
		oids[i] = 25
	}
	return &models.Mock{
		Name: name,
		Kind: models.PostgresV3,
		Spec: models.MockSpec{
			Metadata: map[string]string{"lifetime": "perTest"},
			PostgresV3: &models.PostgresV3Spec{
				Type: models.PostgresV3TypeQuery,
				Query: &models.PostgresV3QuerySpec{
					SQLAstHash:    sqlAstHash,
					SQLNormalized: sqlNorm,
					InvocationID:  name,
					Lifetime:      "perTest",
					BindValues:    cells,
					ParamOIDs:     oids,
					Response: &models.PostgresV3Response{
						CommandComplete: "SELECT 1",
						Rows:            []models.PostgresV3Cells{{models.NewValueCell(payload)}},
					},
				},
			},
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime: models.LifetimePerTest,
		},
	}
}

// TestApplyCrossWindowAutopromote_PromotesAndReroutes pins the
// listmonk-shaped fix: 5 invocations of the same SQL+bind tuple, all
// PerTest, returning varying responses → all 5 promoted to Session,
// all 5 removed from the returned per-test slice and present in the
// returned promoted slice. Verifies the in-place Lifetime mutation
// and LifetimeDerived flag set so downstream DeriveLifetime cannot
// revert the decision.
func TestApplyCrossWindowAutopromote_PromotesAndReroutes(t *testing.T) {
	resetCrossWindowOnceForTest(t)
	t.Setenv(envCrossWindowAutopromote, "1")
	const sessID = "abcdefabcdefabcd"
	mocks := []*models.Mock{
		mkTestPgMock("xs-1", "sha256:xwin-glue", "select data from sessions where id = $1", []string{sessID}, "p-A"),
		mkTestPgMock("xs-2", "sha256:xwin-glue", "select data from sessions where id = $1", []string{sessID}, "p-B"),
		mkTestPgMock("xs-3", "sha256:xwin-glue", "select data from sessions where id = $1", []string{sessID}, "p-C"),
		mkTestPgMock("xs-4", "sha256:xwin-glue", "select data from sessions where id = $1", []string{sessID}, "p-D"),
		mkTestPgMock("xs-5", "sha256:xwin-glue", "select data from sessions where id = $1", []string{sessID}, "p-E"),
	}
	keep, promoted := applyCrossWindowAutopromote(zap.NewNop(), mocks)
	if len(keep) != 0 {
		t.Errorf("keep slice: expected 0 (all 5 promoted), got %d", len(keep))
	}
	if len(promoted) != 5 {
		t.Fatalf("promoted slice: expected 5, got %d", len(promoted))
	}
	for _, m := range promoted {
		if m.TestModeInfo.Lifetime != models.LifetimeSession {
			t.Errorf("%s: TestModeInfo.Lifetime not mutated to Session: got %v", m.Name, m.TestModeInfo.Lifetime)
		}
		if !m.TestModeInfo.LifetimeDerived {
			t.Errorf("%s: LifetimeDerived flag must be set to suppress downstream re-derivation", m.Name)
		}
	}
}

// TestApplyCrossWindowAutopromote_HeterogeneousLeavesPerTest pins
// the negative path: a cohort with distinct binds + distinct
// responses cannot be safely promoted, the helper returns nil
// promoted slice, and the per-test pool is returned unchanged.
func TestApplyCrossWindowAutopromote_HeterogeneousLeavesPerTest(t *testing.T) {
	resetCrossWindowOnceForTest(t)
	t.Setenv(envCrossWindowAutopromote, "1")
	mocks := []*models.Mock{
		mkTestPgMock("u-1", "sha256:xwin-users", "select * from users where id = $1", []string{"1"}, "alice"),
		mkTestPgMock("u-2", "sha256:xwin-users", "select * from users where id = $1", []string{"2"}, "bob"),
		mkTestPgMock("u-3", "sha256:xwin-users", "select * from users where id = $1", []string{"3"}, "carol"),
	}
	keep, promoted := applyCrossWindowAutopromote(zap.NewNop(), mocks)
	if len(keep) != 3 {
		t.Errorf("heterogeneous cohort must remain in per-test slice: got len=%d", len(keep))
	}
	if len(promoted) != 0 {
		t.Errorf("heterogeneous cohort must NOT promote: got %d", len(promoted))
	}
	for _, m := range mocks {
		if m.TestModeInfo.Lifetime != models.LifetimePerTest {
			t.Errorf("%s: helper must not mutate non-promoted mocks", m.Name)
		}
	}
}

// TestApplyCrossWindowAutopromote_DisabledByEnvFlag pins the agent-
// local off-switch: KEPLOY_PG_V3_CROSS_WINDOW_AUTOPROMOTE=0 returns
// the input unchanged with no promoted slice, regardless of cohort
// shape.
func TestApplyCrossWindowAutopromote_DisabledByEnvFlag(t *testing.T) {
	resetCrossWindowOnceForTest(t)
	t.Setenv(envCrossWindowAutopromote, "0")
	const sessID = "abcdefabcdefabcd"
	mocks := []*models.Mock{
		mkTestPgMock("d-1", "sha256:xwin-disabled", "select data from sessions where id = $1", []string{sessID}, "p-A"),
		mkTestPgMock("d-2", "sha256:xwin-disabled", "select data from sessions where id = $1", []string{sessID}, "p-B"),
	}
	keep, promoted := applyCrossWindowAutopromote(zap.NewNop(), mocks)
	if len(keep) != 2 {
		t.Errorf("disabled gate must return input unchanged: got len=%d", len(keep))
	}
	if len(promoted) != 0 {
		t.Errorf("disabled gate must return nil promoted: got %d", len(promoted))
	}
	for _, m := range mocks {
		if m.TestModeInfo.Lifetime != models.LifetimePerTest {
			t.Errorf("%s: disabled gate must not mutate", m.Name)
		}
	}
}

// TestApplyCrossWindowAutopromote_BelowFloorPassthrough pins the
// N<2 short-circuit: a single-mock or empty input returns unchanged
// with no allocations on the promoted side.
func TestApplyCrossWindowAutopromote_BelowFloorPassthrough(t *testing.T) {
	resetCrossWindowOnceForTest(t)
	t.Setenv(envCrossWindowAutopromote, "1")
	keep, promoted := applyCrossWindowAutopromote(zap.NewNop(), nil)
	if keep != nil || promoted != nil {
		t.Errorf("nil input: expected (nil, nil); got (%v, %v)", keep, promoted)
	}
	one := []*models.Mock{
		mkTestPgMock("solo", "sha256:xwin-floor", "select 1", nil, "ok"),
	}
	keep, promoted = applyCrossWindowAutopromote(zap.NewNop(), one)
	if len(keep) != 1 || len(promoted) != 0 {
		t.Errorf("single-mock input: expected pass-through; got keep=%d promoted=%d", len(keep), len(promoted))
	}
}

// TestApplyCrossWindowAutopromote_PreservesNonPromotedAcrossCohorts
// pins the partial-promote case: mixed pool with one promotable
// cohort and one heterogeneous cohort. Only the promotable cohort
// should land in the promoted slice; the heterogeneous mocks must
// remain in the keep slice unchanged.
func TestApplyCrossWindowAutopromote_PreservesNonPromotedAcrossCohorts(t *testing.T) {
	resetCrossWindowOnceForTest(t)
	t.Setenv(envCrossWindowAutopromote, "1")
	const sessID = "abcdefabcdefabcd"
	mocks := []*models.Mock{
		// Promotable cohort (bind-invariant + read-only)
		mkTestPgMock("p-1", "sha256:xwin-mix-good", "select data from sessions where id = $1", []string{sessID}, "p-A"),
		mkTestPgMock("p-2", "sha256:xwin-mix-good", "select data from sessions where id = $1", []string{sessID}, "p-B"),
		// Heterogeneous cohort (different binds, different responses)
		mkTestPgMock("h-1", "sha256:xwin-mix-bad", "select * from users where id = $1", []string{"1"}, "alice"),
		mkTestPgMock("h-2", "sha256:xwin-mix-bad", "select * from users where id = $1", []string{"2"}, "bob"),
	}
	keep, promoted := applyCrossWindowAutopromote(zap.NewNop(), mocks)
	if len(promoted) != 2 {
		t.Fatalf("expected 2 promoted (the bind-invariant cohort), got %d", len(promoted))
	}
	if len(keep) != 2 {
		t.Fatalf("expected 2 kept (the heterogeneous cohort), got %d", len(keep))
	}
	keepNames := map[string]bool{}
	for _, m := range keep {
		keepNames[m.Name] = true
	}
	if !keepNames["h-1"] || !keepNames["h-2"] {
		t.Errorf("expected heterogeneous mocks h-1, h-2 in keep slice; got %v", keepNames)
	}
}
