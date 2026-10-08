package proxy

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Staging must not write into a mock a reader may be holding. A matcher takes
// mocks from the pools (GetSessionMocks, GetPerTestMocksInWindow, ...) and reads
// them while other connections are matched and the next test is staged; the
// manager used to stamp each staged mock's tree ID and sort order into the
// caller's object, so re-staging a mock that a live tree still served (a caller
// handing back mocks it got from the pools, or listing one mock in two tiers)
// wrote fields a matcher was copying. The pools also hold the copies matchers
// put back through UpdateUnFilteredMock, made with a struct copy or with
// DeepCopy, so those must count as pooled too. Run with -race.
func TestStagingDoesNotWriteIntoMocksReadersHold(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	var perTest, session []*models.Mock
	for i := 0; i < 200; i++ {
		at := base.Add(time.Duration(i) * time.Millisecond)
		session = append(session, newMockForTest(fmt.Sprintf("s%d", i), at, models.LifetimeSession))
		if i%4 == 0 {
			perTest = append(perTest, newMockForTest(fmt.Sprintf("p%d", i), at, models.LifetimePerTest))
		}
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(perTest, session, base, base.Add(time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	// Readers copy pooled mocks the way a matcher's updateMock does.
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				all, _ := mm.GetSessionMocks()
				pt, _ := mm.GetPerTestMocksInWindow()
				for _, mk := range append(all, pt...) {
					cp := *mk
					_ = cp.TestModeInfo
				}
			}
		}()
	}
	// A matcher puts updated copies back, as updateMock does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			s, _ := mm.GetSessionScopedMocks()
			if len(s) == 0 {
				continue
			}
			mk := s[i%len(s)]
			var updated *models.Mock
			if i%2 == 0 {
				cp := *mk
				updated = &cp
			} else {
				updated = mk.DeepCopy()
			}
			updated.TestModeInfo.SortOrder = mk.TestModeInfo.SortOrder + 100_000
			mm.UpdateUnFilteredMock(mk, updated)
		}
	}()
	// The stager re-stages the mocks the pools are serving, in a different
	// order each time, as a caller that builds the next test's pools from
	// the current ones does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil; i++ {
			s, _ := mm.GetSessionScopedMocks()
			p, _ := mm.GetFilteredMocks()
			if i%2 == 1 {
				s = append(s[len(s)/2:len(s):len(s)], s[:len(s)/2]...)
			}
			start := base.Add(time.Duration(i%100) * time.Millisecond)
			mm.SetMocksWithWindow(p, s, start, start.Add(10*time.Millisecond))
			mm.SetUnFilteredMocks(s)
			mm.SetFilteredMocks(p)
		}
	}()
	wg.Wait()
}

// The race that reaches production: BaseTime staging (the first staging call of
// every test set, which runs while the application boots) puts each staged
// session mock in the startup tier and the session tier. The startup tier was
// published first and the session tier's build then stamped tree IDs into the
// same mocks, while bootstrap traffic was matched against the startup tier.
// The callers here hand over fresh mocks on every call, as the agent does.
func TestBaseTimeStagingPublishesNoMockItStillStamps(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	fresh := func() []*models.Mock {
		out := make([]*models.Mock, 0, 200)
		for i := 0; i < 200; i++ {
			out = append(out, newMockForTest(fmt.Sprintf("s%d", i), base.Add(time.Duration(i)*time.Millisecond), models.LifetimeSession))
		}
		return out
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, fresh(), models.BaseTime, time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				st, _ := mm.GetStartupMocks()
				for _, mk := range st {
					cp := *mk
					_ = cp.TestModeInfo
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			mm.SetMocksWithWindow(nil, fresh(), models.BaseTime, time.Now())
		}
	}()
	wg.Wait()
}

// A mock the pools serve, handed back to be staged again, is replaced by a copy
// and left exactly as it was: a matcher may be copying it. A mock no pool has
// held yet is taken over as it is, and one listed in two tiers of a staging
// stays one object in the pools (GetSessionMocks dedups by pointer).
func TestStagingCopiesOnlyMocksThePoolsServe(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	boot := newMockForTest("boot", base.Add(-time.Millisecond), models.LifetimePerTest)
	session := []*models.Mock{
		newMockForTest("a", base.Add(time.Millisecond), models.LifetimeSession),
		newMockForTest("b", base.Add(2*time.Millisecond), models.LifetimeSession),
		boot, // the same mock in the per-test and the session lists
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow([]*models.Mock{boot}, session, base, base.Add(10*time.Millisecond))

	all, _ := mm.GetSessionMocks()
	served := map[string]*models.Mock{}
	for _, mk := range all {
		if served[mk.Name] != nil {
			t.Fatalf("%q appears twice in GetSessionMocks", mk.Name)
		}
		served[mk.Name] = mk
	}
	for _, mk := range session {
		if served[mk.Name] != mk {
			t.Errorf("a fresh mock %q was copied; the manager takes fresh mocks over", mk.Name)
		}
		if !mk.Pooled() {
			t.Errorf("%q is served but not marked pooled", mk.Name)
		}
	}

	// Stage the served mocks again, in another order, which would stamp each
	// one, boot included, with a new tree ID.
	before := map[string]models.TestModeInfo{}
	again := []*models.Mock{served["boot"], served["a"], served["b"]}
	for _, mk := range again {
		before[mk.Name] = mk.TestModeInfo
	}
	mm.SetMocksWithWindow([]*models.Mock{served["boot"]}, again, base, base.Add(10*time.Millisecond))
	for _, mk := range again {
		if mk.TestModeInfo != before[mk.Name] {
			t.Errorf("staging wrote into the served %q: %+v, was %+v", mk.Name, mk.TestModeInfo, before[mk.Name])
		}
	}
	all, _ = mm.GetSessionMocks()
	seen := map[string]int{}
	for _, mk := range all {
		seen[mk.Name]++
		for _, old := range again {
			if mk == old {
				t.Errorf("the pools still serve the old %q object after it was staged again", mk.Name)
			}
		}
	}
	if seen["boot"] != 1 || seen["a"] != 1 || seen["b"] != 1 {
		t.Errorf("GetSessionMocks after re-staging: %v; want each mock once", seen)
	}
}

// SetMocksWithWindowThreeTier stamps its explicit startup slice with the rest
// of the staging, before it publishes any tier. It used to add the slice to the
// startup tier after the staging had published every tier and filed the
// carry-over pool, stamping a sort order into each mock that had none, so a
// mock the slice shares with the per-test input that the carry-over pool kept
// was written while carry-over readers copied it. Run with -race.
func TestThreeTierStagingPublishesNoMockItStillStamps(t *testing.T) {
	registerSends(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.ResetForReplaySession()
	mm.SeedStartupCutoff(winStart(1))
	mm.SeedRecordedWindows(carryWindows())
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			carry, _ := mm.GetCarryOverMocks()
			for _, mk := range carry {
				cp := *mk // a matcher's copy
				_ = cp.TestModeInfo.SortOrder
			}
		}
	}()
	filed := false
	for i := 0; i < 200; i++ {
		// A SEND recorded in the gap after W2 is reachable from W1, so W1's
		// staging files it in the carry-over pool.
		send := brokerMockAt(fmt.Sprintf("send-%d", i), pulsarKind, "SEND", 2500)
		mm.SetMocksWithWindowThreeTier([]*models.Mock{send}, nil, []*models.Mock{send}, winStart(1), winEnd(1))
		if carry, _ := mm.GetCarryOverMocks(); containsMockNamed(carry, send.Name) {
			filed = true
		}
	}
	close(done)
	wg.Wait()
	if !filed {
		t.Fatal("precondition: the carry-over pool never held the staged SEND")
	}
}

// A connection mock a matcher adds at runtime (AddConnectionMock) is in a pool
// from then on: handed back to a staging, it is copied, not stamped.
func TestStagingCopiesAConnectionMockAMatcherAdded(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, nil, base, base.Add(10*time.Millisecond))

	conn := newMockForTest("conn-stmt", base.Add(time.Millisecond), models.LifetimeConnection)
	conn.Spec.Metadata = map[string]string{"type": "connection", "connID": "c1"}
	conn.TestModeInfo.ID, conn.TestModeInfo.SortOrder = 41, 7
	mm.AddConnectionMock(conn)
	held, _ := mm.GetConnectionMocks("c1")
	if len(held) != 1 || held[0] != conn {
		t.Fatalf("connection pool = %v, want the added mock", held)
	}

	before := conn.TestModeInfo
	other := newMockForTest("s", base.Add(2*time.Millisecond), models.LifetimeSession)
	mm.SetMocksWithWindow(nil, []*models.Mock{other, conn}, base, base.Add(10*time.Millisecond))
	if conn.TestModeInfo != before {
		t.Fatalf("staging wrote into the pooled connection mock: %+v, was %+v", conn.TestModeInfo, before)
	}
	session, _ := mm.GetSessionMocks()
	for _, mk := range session {
		if mk == conn {
			t.Fatal("the session pool holds the connection pool's object; staging should have copied it")
		}
	}
}
