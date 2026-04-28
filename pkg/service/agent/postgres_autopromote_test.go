// postgres_autopromote_test.go pins the cross-window autopromote
// glue on the agent layer. The integrations side has its own coverage
// for the heuristic correctness; these tests focus on the applier's
// job: route promoted mocks out of the per-test pool, mutate Lifetime
// in place so downstream readers see a single source of truth, respect
// the env off-switch, and short-circuit cleanly when no heuristic hook
// has been registered (public OSS build path).
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

// withCrossWindowHook installs a test-local stub for the autopromote
// hook and restores the prior value (typically nil) on test exit. The
// stub mirrors v3types.EvaluateCrossWindowAutopromote's contract: pure,
// non-mutating on inputs, returns a per-Mock-name Lifetime decision map.
func withCrossWindowHook(t *testing.T, fn func(allMocks []*models.Mock, logger *zap.Logger) map[string]models.Lifetime) {
	t.Helper()
	prev := crossWindowAutopromoteHook
	crossWindowAutopromoteHook = fn
	t.Cleanup(func() { crossWindowAutopromoteHook = prev })
}

// promoteAllToSession is a test-only stub that pretends every input
// mock qualifies for autopromote. Lets the applier-level tests
// exercise routing/mutation/slice management without dragging in the
// integrations heuristic.
func promoteAllToSession(allMocks []*models.Mock, _ *zap.Logger) map[string]models.Lifetime {
	out := make(map[string]models.Lifetime, len(allMocks))
	for _, m := range allMocks {
		if m == nil {
			continue
		}
		out[m.Name] = models.LifetimeSession
	}
	return out
}

// promoteByName promotes only the named mocks, leaving others
// unchanged. Exercises the partial-promote path.
func promoteByName(names ...string) func([]*models.Mock, *zap.Logger) map[string]models.Lifetime {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	return func(allMocks []*models.Mock, _ *zap.Logger) map[string]models.Lifetime {
		out := map[string]models.Lifetime{}
		for _, m := range allMocks {
			if m == nil || !want[m.Name] {
				continue
			}
			out[m.Name] = models.LifetimeSession
		}
		return out
	}
}

func mkTestPgMock(name string) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.PostgresV3,
		TestModeInfo: models.TestModeInfo{
			Lifetime: models.LifetimePerTest,
		},
	}
}

// TestApplyCrossWindowAutopromote_PromotesAndReroutes pins the
// listmonk-shaped fix: the heuristic promotes every input mock,
// applier mutates Lifetime in place + sets LifetimeDerived, and the
// returned slices route every mock out of perTest into promoted.
func TestApplyCrossWindowAutopromote_PromotesAndReroutes(t *testing.T) {
	resetCrossWindowOnceForTest(t)
	t.Setenv(envCrossWindowAutopromote, "1")
	withCrossWindowHook(t, promoteAllToSession)
	mocks := []*models.Mock{mkTestPgMock("xs-1"), mkTestPgMock("xs-2"), mkTestPgMock("xs-3"), mkTestPgMock("xs-4"), mkTestPgMock("xs-5")}
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

// TestApplyCrossWindowAutopromote_HookNilNoOps pins the public OSS
// build path: when no integrations wire has registered a hook,
// applyCrossWindowAutopromote returns the input untouched.
func TestApplyCrossWindowAutopromote_HookNilNoOps(t *testing.T) {
	resetCrossWindowOnceForTest(t)
	t.Setenv(envCrossWindowAutopromote, "1")
	withCrossWindowHook(t, nil)
	mocks := []*models.Mock{mkTestPgMock("nh-1"), mkTestPgMock("nh-2")}
	keep, promoted := applyCrossWindowAutopromote(zap.NewNop(), mocks)
	if len(keep) != 2 {
		t.Errorf("nil-hook path must return input untouched: got len=%d", len(keep))
	}
	if len(promoted) != 0 {
		t.Errorf("nil-hook path must not promote: got %d", len(promoted))
	}
	for _, m := range mocks {
		if m.TestModeInfo.Lifetime != models.LifetimePerTest {
			t.Errorf("%s: nil-hook must not mutate", m.Name)
		}
	}
}

// TestApplyCrossWindowAutopromote_HeterogeneousLeavesPerTest pins
// the negative path: a hook that returns no decisions leaves the
// per-test pool intact.
func TestApplyCrossWindowAutopromote_HeterogeneousLeavesPerTest(t *testing.T) {
	resetCrossWindowOnceForTest(t)
	t.Setenv(envCrossWindowAutopromote, "1")
	withCrossWindowHook(t, func([]*models.Mock, *zap.Logger) map[string]models.Lifetime { return nil })
	mocks := []*models.Mock{mkTestPgMock("u-1"), mkTestPgMock("u-2"), mkTestPgMock("u-3")}
	keep, promoted := applyCrossWindowAutopromote(zap.NewNop(), mocks)
	if len(keep) != 3 {
		t.Errorf("no-decision path: keep slice must equal input; got %d", len(keep))
	}
	if len(promoted) != 0 {
		t.Errorf("no-decision path: promoted slice must be empty; got %d", len(promoted))
	}
	for _, m := range mocks {
		if m.TestModeInfo.Lifetime != models.LifetimePerTest {
			t.Errorf("%s: helper must not mutate when no decisions returned", m.Name)
		}
	}
}

// TestApplyCrossWindowAutopromote_DisabledByEnvFlag pins the agent-
// local off-switch: KEPLOY_PG_V3_CROSS_WINDOW_AUTOPROMOTE=0 returns
// the input unchanged with no promoted slice, regardless of whether
// a hook is registered.
func TestApplyCrossWindowAutopromote_DisabledByEnvFlag(t *testing.T) {
	resetCrossWindowOnceForTest(t)
	t.Setenv(envCrossWindowAutopromote, "0")
	withCrossWindowHook(t, promoteAllToSession)
	mocks := []*models.Mock{mkTestPgMock("d-1"), mkTestPgMock("d-2")}
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
// without ever calling the hook (cheap pre-check).
func TestApplyCrossWindowAutopromote_BelowFloorPassthrough(t *testing.T) {
	resetCrossWindowOnceForTest(t)
	t.Setenv(envCrossWindowAutopromote, "1")
	called := 0
	withCrossWindowHook(t, func([]*models.Mock, *zap.Logger) map[string]models.Lifetime {
		called++
		return nil
	})
	keep, promoted := applyCrossWindowAutopromote(zap.NewNop(), nil)
	if keep != nil || promoted != nil {
		t.Errorf("nil input: expected (nil, nil); got (%v, %v)", keep, promoted)
	}
	one := []*models.Mock{mkTestPgMock("solo")}
	keep, promoted = applyCrossWindowAutopromote(zap.NewNop(), one)
	if len(keep) != 1 || len(promoted) != 0 {
		t.Errorf("single-mock input: expected pass-through; got keep=%d promoted=%d", len(keep), len(promoted))
	}
	if called != 0 {
		t.Errorf("hook must NOT be invoked on below-floor input; called=%d", called)
	}
}

// TestApplyCrossWindowAutopromote_PartialPromote pins the partial
// path: the heuristic returns a decision for some but not all mocks.
// Promoted ones land in the promoted slice; the rest stay in keep.
func TestApplyCrossWindowAutopromote_PartialPromote(t *testing.T) {
	resetCrossWindowOnceForTest(t)
	t.Setenv(envCrossWindowAutopromote, "1")
	withCrossWindowHook(t, promoteByName("p-1", "p-2"))
	mocks := []*models.Mock{
		mkTestPgMock("p-1"), mkTestPgMock("p-2"), mkTestPgMock("h-1"), mkTestPgMock("h-2"),
	}
	keep, promoted := applyCrossWindowAutopromote(zap.NewNop(), mocks)
	if len(promoted) != 2 {
		t.Fatalf("expected 2 promoted, got %d", len(promoted))
	}
	if len(keep) != 2 {
		t.Fatalf("expected 2 kept, got %d", len(keep))
	}
	keepNames := map[string]bool{}
	for _, m := range keep {
		keepNames[m.Name] = true
	}
	if !keepNames["h-1"] || !keepNames["h-2"] {
		t.Errorf("expected non-promoted mocks h-1, h-2 in keep slice; got %v", keepNames)
	}
}

// TestRegisterCrossWindowAutopromoteHook_Idempotent pins the
// register API: a second registration overwrites the first.
func TestRegisterCrossWindowAutopromoteHook_Idempotent(t *testing.T) {
	prev := crossWindowAutopromoteHook
	t.Cleanup(func() { crossWindowAutopromoteHook = prev })
	resetCrossWindowOnceForTest(t)
	t.Setenv(envCrossWindowAutopromote, "1")
	mocks := []*models.Mock{mkTestPgMock("r-1"), mkTestPgMock("r-2")}
	RegisterCrossWindowAutopromoteHook(func([]*models.Mock, *zap.Logger) map[string]models.Lifetime { return nil })
	if _, p := applyCrossWindowAutopromote(zap.NewNop(), mocks); len(p) != 0 {
		t.Errorf("first registration: expected no-op; got %d promoted", len(p))
	}
	RegisterCrossWindowAutopromoteHook(promoteAllToSession)
	_, p := applyCrossWindowAutopromote(zap.NewNop(), mocks)
	if len(p) != 2 {
		t.Errorf("second registration must overwrite: expected 2 promoted; got %d", len(p))
	}
}
