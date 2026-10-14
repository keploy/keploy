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

// hitsOf reads the match count the manager holds for name.
func hitsOf(mm *MockManager, name string) uint64 {
	mm.hitMu.RLock()
	defer mm.hitMu.RUnlock()
	if n := mm.hitIdx[name]; n != nil {
		return n.Load()
	}
	return 0
}

// Counting a match must not write the matched mock. Matchers copy a pooled mock
// whole (updateMock's `updatedMock := *matchedMock`, the consume doors that take
// a models.Mock by value) while other connections count matches against the
// same mock. The count used to be an atomic field on the mock itself, so every
// such copy raced every count. Run with -race.
func TestCountingAMatchDoesNotWriteThePooledMock(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	var session []*models.Mock
	for i := 0; i < 50; i++ {
		session = append(session, newMockForTest(fmt.Sprintf("s%d", i), base.Add(time.Duration(i)*time.Millisecond), models.LifetimeSession))
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, session, base, base.Add(time.Second))
	pooled, _ := mm.GetSessionMocks()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				for _, mk := range pooled {
					mm.MarkMockAsUsed(*mk)
				}
			}
		}()
	}
	wg.Wait()
	if got := mm.SessionMockHitCounts()["s0"]; got == 0 {
		t.Fatal("no match was counted for s0")
	}
}

// A matched session mock is replaced in its pool by the matcher's updated copy
// (UpdateUnFilteredMock). Its count must survive that: it used to stay on the
// replaced object, so the pool's copy reported a count frozen at the update.
func TestHitCountsFollowTheNameAcrossAnUpdate(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	s := newMockForTest("s", base.Add(time.Millisecond), models.LifetimeSession)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, []*models.Mock{s}, base, base.Add(time.Second))

	mm.MarkMockAsUsed(*s)
	mm.MarkMockAsUsed(*s)
	updated := *s
	updated.TestModeInfo.SortOrder = s.TestModeInfo.SortOrder + 1000
	if !mm.UpdateUnFilteredMock(s, &updated) {
		t.Fatal("UpdateUnFilteredMock failed")
	}
	mm.MarkMockAsUsed(updated)

	if got := mm.SessionMockHitCounts()["s"]; got != 3 {
		t.Fatalf("SessionMockHitCounts[s] = %d after three matches across an update, want 3", got)
	}
}

// The agent stages fresh copies of a set's mocks for every test, so a count kept
// on the mock object restarted at every test, and a session mock matched in
// every test still read as matched once. The counts must span the set — a
// BaseTime staging mid-set (keploy mock replay stages one around a
// single-worker test scope) is not a boundary — and restart with the next set:
// names are only unique within a set.
func TestHitCountsSpanTheTestSet(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	s := newMockForTest("s", base.Add(time.Millisecond), models.LifetimeSession)
	fresh := func() []*models.Mock { return []*models.Mock{s.DeepCopy()} }
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	count := func() uint64 { return mm.SessionMockHitCounts()["s"] }

	mm.SetMocksWithWindow(nil, fresh(), models.BaseTime, time.Now())
	mm.SetMocksWithWindow(nil, fresh(), base, base.Add(time.Second))
	mm.MarkMockAsUsed(*s)
	mm.MarkMockAsUsed(*s)
	mm.SetMocksWithWindow(nil, fresh(), models.BaseTime, time.Now())
	mm.SetMocksWithWindow(nil, fresh(), base.Add(time.Second), base.Add(2*time.Second))
	mm.MarkMockAsUsed(*s)
	if got := count(); got != 3 {
		t.Fatalf("count = %d after three matches over two tests of one set, want 3", got)
	}

	// The next set starts afresh.
	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, fresh(), models.BaseTime, time.Now())
	if got := count(); got != 0 {
		t.Fatalf("count = %d after the next set's first staging, want 0", got)
	}
}

// The index holds the names of the pool a staging serves, and no more: in
// strict mode the BaseTime staging loads a set's whole per-test pool, and
// keeping each of those names for the set would hold memory the disk-backed
// store exists to release.
func TestHitIndexHoldsOnlyTheStagedNames(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	var perTest []*models.Mock
	for i := 0; i < 500; i++ {
		perTest = append(perTest, newMockForTest(fmt.Sprintf("p%d", i), base.Add(time.Duration(i)*time.Millisecond), models.LifetimePerTest))
	}
	s := newMockForTest("s", base.Add(time.Millisecond), models.LifetimeSession)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(perTest, []*models.Mock{s.DeepCopy()}, models.BaseTime, time.Now())
	mm.MarkMockAsUsed(*s)
	mm.SetMocksWithWindow(nil, []*models.Mock{s.DeepCopy()}, base.Add(time.Hour), base.Add(time.Hour+time.Second))

	mm.hitMu.RLock()
	n := len(mm.hitIdx)
	mm.hitMu.RUnlock()
	if n != 1 {
		t.Fatalf("hitIdx holds %d names after a staging of one mock, want 1", n)
	}
	if got := mm.SessionMockHitCounts()["s"]; got != 1 {
		t.Fatalf("the session mock's count = %d, want 1", got)
	}
}

// A name first matched while its staging is building the hit index is seeded by
// bumpHitCount's slow path into the index being replaced; the rebuild must keep
// that counter rather than swap in a zeroed one.
func TestHitIndexKeepsACounterSeededDuringItsRebuild(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	a := newMockForTest("a", base.Add(time.Millisecond), models.LifetimeSession)
	b := newMockForTest("b", base.Add(2*time.Millisecond), models.LifetimeSession)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, []*models.Mock{a.DeepCopy()}, base, base.Add(time.Second))

	// The next staging adds b. Its trees are swapped in before the index is
	// rebuilt, so a match on b in between takes the slow path and seeds b.
	mm.hitIndexBuilt = func() {
		mm.hitIndexBuilt = nil
		mm.MarkMockAsUsed(*b)
	}
	mm.SetMocksWithWindow(nil, []*models.Mock{a.DeepCopy(), b.DeepCopy()}, base, base.Add(time.Second))

	if got := hitsOf(mm, "b"); got != 1 {
		t.Fatalf("count of b = %d after a match during the rebuild, want 1", got)
	}
}
