package proxy

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func newTestClaimManager() *MockManager {
	return NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
}

// Two separate MockManagers (as parallel test workers hold) each own their own
// ClaimOnce set: claiming a key on one manager never starves the other, and
// staging one manager never resets the other's claims.
func TestMockManager_ClaimOnce_TwoManagersIndependent(t *testing.T) {
	m1 := newTestClaimManager()
	m2 := newTestClaimManager()
	const key = "occ-1\x00mock-7\x000"

	if !m1.ClaimOnce(key) {
		t.Fatal("m1 first ClaimOnce = false, want true")
	}
	if m1.ClaimOnce(key) {
		t.Fatal("m1 repeat ClaimOnce = true, want false")
	}
	if !m2.ClaimOnce(key) {
		t.Fatal("m2 first ClaimOnce = false, want true (m1 must not starve m2)")
	}
	if m2.ClaimOnce(key) {
		t.Fatal("m2 repeat ClaimOnce = true, want false")
	}

	// Staging m2 resets only m2, leaving m1's claim intact.
	m2.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if !m2.ClaimOnce(key) {
		t.Fatal("m2 ClaimOnce after own staging = false, want true")
	}
	if m1.ClaimOnce(key) {
		t.Fatal("m1 ClaimOnce after m2 staging = true, want false (m2 staging must not reset m1)")
	}

	// Staging m1 resets only m1, leaving m2's claim intact.
	m1.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if !m1.ClaimOnce(key) {
		t.Fatal("m1 ClaimOnce after own staging = false, want true")
	}
	if m2.ClaimOnce(key) {
		t.Fatal("m2 ClaimOnce after m1 staging = true, want false")
	}
}

// ClaimOnce resets at each staging (SetMocksWithWindow with models.BaseTime,
// SetScopeMocks open=true, ResetForReplaySession) and stays claimed across
// per-test window advances within one staging.
func TestMockManager_ClaimOnce_ResetsAtStaging(t *testing.T) {
	m := newTestClaimManager()
	const key = "occ-stage\x00mock-1\x000"

	if !m.ClaimOnce(key) {
		t.Fatal("initial ClaimOnce = false, want true")
	}
	if m.ClaimOnce(key) {
		t.Fatal("repeat ClaimOnce = true, want false")
	}

	// Advancing the per-test window within the same staging does not reset.
	now := time.Now()
	m.SetCurrentTestWindow(now.Add(-time.Second), now)
	if m.ClaimOnce(key) {
		t.Fatal("ClaimOnce after SetCurrentTestWindow = true, want false")
	}

	// A new BaseTime staging resets the claimed set.
	m.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if !m.ClaimOnce(key) {
		t.Fatal("ClaimOnce after BaseTime SetMocksWithWindow = false, want true")
	}
	if m.ClaimOnce(key) {
		t.Fatal("repeat ClaimOnce after BaseTime staging = true, want false")
	}

	// SetScopeMocks(open=true) bumps stagingEpoch and resets the claimed set.
	m.SetScopeMocks(nil, nil, true)
	if !m.ClaimOnce(key) {
		t.Fatal("ClaimOnce after SetScopeMocks(open=true) = false, want true")
	}
	if m.ClaimOnce(key) {
		t.Fatal("repeat ClaimOnce after SetScopeMocks = true, want false")
	}

	// ResetForReplaySession also clears the claimed set.
	m.ResetForReplaySession()
	if !m.ClaimOnce(key) {
		t.Fatal("ClaimOnce after ResetForReplaySession = false, want true")
	}
	if m.ClaimOnce(key) {
		t.Fatal("repeat ClaimOnce after ResetForReplaySession = true, want false")
	}
}

// Per-connection wrappers (scopedMockDb, roundScope, heldScope) delegate
// ClaimOnce to their underlying MockManager so all connections over one
// manager share one once-per-staging claim set.
func TestScopedMockDb_ClaimOnce_DelegatesToManager(t *testing.T) {
	m := newTestClaimManager()
	w1 := &scopedMockDb{MockMemDb: m}
	w2 := &scopedMockDb{MockMemDb: m}
	rs := &roundScope{MockManager: m}
	hs := &heldScope{roundScope: roundScope{MockManager: m}}

	var c1 integrations.MockMemDb = w1
	var c2 integrations.MockMemDb = w2
	var cr integrations.MockMemDb = rs
	var ch integrations.MockMemDb = hs

	const key = "occ-wrap\x00mock-3\x000"
	if !c1.ClaimOnce(key) {
		t.Fatal("wrapper 1 first ClaimOnce = false, want true")
	}
	if c2.ClaimOnce(key) {
		t.Fatal("wrapper 2 ClaimOnce in same staging = true, want false")
	}
	if cr.ClaimOnce(key) {
		t.Fatal("roundScope ClaimOnce in same staging = true, want false")
	}
	if ch.ClaimOnce(key) {
		t.Fatal("heldScope ClaimOnce in same staging = true, want false")
	}

	// Concurrent claims through 32 distinct wrappers after a staging reset
	// succeed exactly once.
	m.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	const goroutines = 32
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			view := &scopedMockDb{MockMemDb: m}
			<-start
			if view.ClaimOnce(key) {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Fatalf("concurrent ClaimOnce across %d distinct wrappers won %d times, want 1", goroutines, got)
	}
}

// A retry cycle of keploy test (--retry-passing-test) runs the set's passing
// test cases again without staging the set again: the agent hands the manager
// the cycle's count of consumed-history rewinds (SeedConsumedRewinds) before
// the cycle's first test. Each cycle is a new pass, so an occurrence claimed in
// one cycle is claimed again, once, in the next; the same count handed again
// within a cycle, a lower one (a filter that carries none), and the deferred
// streaming tests going back to an earlier window after the last cycle, start
// no pass. A staging starts the count over from what it carries. Seen through
// a per-connection wrapper, as a parser claims. (keploy runs up to three
// cycles; the manager takes any number.)
func TestMockManager_ClaimOnce_EachRetryCycleIsANewPass(t *testing.T) {
	m := newTestClaimManager()
	var db integrations.MockMemDb = &scopedMockDb{MockMemDb: m}
	const key = "occ-retry\x00mock-1\x000"
	base := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)

	m.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	m.SeedConsumedRewinds(0)
	m.SetCurrentTestWindow(base, base.Add(time.Second))
	if !db.ClaimOnce(key) {
		t.Fatal("the first cycle's ClaimOnce = false, want true")
	}
	if db.ClaimOnce(key) {
		t.Fatal("a repeat in the first cycle = true, want false")
	}
	for cycle := uint64(1); cycle <= 4; cycle++ {
		m.SeedConsumedRewinds(cycle)
		m.SetCurrentTestWindow(base, base.Add(time.Second))
		if !db.ClaimOnce(key) {
			t.Fatalf("retry cycle %d's ClaimOnce = false, want true: the retried test misses what the recording delivered it", cycle+1)
		}
		m.SeedConsumedRewinds(cycle)
		if db.ClaimOnce(key) {
			t.Fatalf("retry cycle %d: the same count handed again started a pass; a repeat = true, want false", cycle+1)
		}
		m.SeedConsumedRewinds(0)
		m.SeedConsumedRewinds(cycle)
		if db.ClaimOnce(key) {
			t.Fatalf("retry cycle %d: a filter carrying no count, then the cycle's again, started a pass; a repeat = true, want false", cycle+1)
		}
	}
	// The deferred streaming tests, after the last cycle, go back to an
	// earlier window with the same count: no new pass, nothing twice.
	m.SeedConsumedRewinds(4)
	m.SetCurrentTestWindow(base.Add(-time.Minute), base.Add(-time.Minute+time.Second))
	if db.ClaimOnce(key) {
		t.Fatal("the deferred streaming tail's ClaimOnce = true, want false: one recorded occurrence delivered twice in one pass")
	}
	// A staging that carries the set's count (keploy test registering the
	// set's mocks again part way, on an agent it found replaced) starts a
	// pass; the cycle's next filter, with the same count, does not.
	m.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if !db.ClaimOnce(key) {
		t.Fatal("ClaimOnce after a staging that carries the count = false, want true")
	}
	m.SeedConsumedRewinds(4)
	if db.ClaimOnce(key) {
		t.Fatal("the cycle's next filter after a staging that carried its count started a pass; a repeat = true, want false")
	}
	// The next staging (another set, or another attempt of it) starts the
	// count over from its own, zero, and a pass with it; that set's first
	// retry cycle is a pass again, below the last set's count.
	m.SeedConsumedRewinds(0)
	m.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if !db.ClaimOnce(key) {
		t.Fatal("ClaimOnce after the next staging = false, want true")
	}
	m.SeedConsumedRewinds(1)
	if !db.ClaimOnce(key) {
		t.Fatal("the next set's first retry cycle ClaimOnce = false, want true: the last set's count held it back")
	}
}
