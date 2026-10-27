package proxy

import (
	"fmt"
	"math/rand"
	"slices"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// liveReadCost answers, in recorded order, every mock of one test that owns
// n, one call at a time, as a matcher that stops at the test's next recording
// does (RangePerTestMocksInWindow, stopped at the first mock). It returns the
// most owned positions one such read looked at, and the most a whole
// per-test read (GetPerTestMocksInWindow) looked at beyond the mocks it
// handed.
func liveReadCost(t *testing.T, n int) (maxFirst, maxBeyond int) {
	t.Helper()
	mm, mocks, _, _ := keyedSet(t, n)
	for k := 0; k < n; k++ {
		before := unansweredVisited.Load()
		var first *models.Mock
		if err := mm.RangePerTestMocksInWindow(func(mk *models.Mock) bool {
			first = mk
			return false
		}); err != nil {
			t.Fatal(err)
		}
		maxFirst = max(maxFirst, int(unansweredVisited.Load()-before))
		if first == nil || first.Name != mocks[k].Name {
			t.Fatalf("call %d: the per-test walk handed %v first, want %s (the next in recorded order)", k, first, mocks[k].Name)
		}
		before = unansweredVisited.Load()
		all, err := mm.GetPerTestMocksInWindow()
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != n-k {
			t.Fatalf("call %d: the per-test read handed %d mocks, want %d", k, len(all), n-k)
		}
		maxBeyond = max(maxBeyond, int(unansweredVisited.Load()-before)-len(all))
		if got := mm.exactConsume(first, true); got != exactServed {
			t.Fatalf("call %d: consume of %s gave %v, want served", k, first.Name, got)
		}
	}
	return maxFirst, maxBeyond
}

// TestAPerTestReadOfATestThatOwnsManyRecordingsCostsTheSameAtAnySize: a test
// that owns every recording of the set (one top-level Go test whose subtests
// make all the calls) is read on every call. A read that stops at the test's
// next recording looks at the same few owned positions whether the test owns
// 300 or 3,000, and a whole read looks at the same few beyond what it hands:
// each run of consumed positions is skipped by its link, not walked. With the
// links off (liveReadsWalk, how the read walked before) the first read looks
// at every mock the test answered, and the row fails.
func TestAPerTestReadOfATestThatOwnsManyRecordingsCostsTheSameAtAnySize(t *testing.T) {
	const bound = 4
	for _, n := range []int{300, 3000} {
		first, beyond := liveReadCost(t, n)
		if first > bound || beyond > bound {
			t.Fatalf("a test owning %d: a read stopped at the next recording looked at up to %d owned positions, a whole read at up to %d beyond what it handed; want at most %d each, at any size", n, first, beyond, bound)
		}
	}
	liveReadsWalk.Store(true)
	defer liveReadsWalk.Store(false)
	if first, _ := liveReadCost(t, 3000); first <= bound {
		t.Fatalf("with the links off a read stopped at the next recording looked at up to %d owned positions: the row cannot tell the walk from the links", first)
	}
}

// liveMock is a recording of kind for the live-list rows, staged in the
// session tier with session set.
func liveMock(name string, kind models.Kind, at time.Time, session bool) *models.Mock {
	mk := &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: kind, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "mocks"},
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
	}}
	if session {
		mk.TestModeInfo.Lifetime = models.LifetimeSession
	}
	return mk
}

// TestPerTestReadsGiveTheWalksAnswerOverRandomConsumesAndClaims: over random
// consumes (in any order, of the test's own recordings and of the ones it
// shares), claims of shared recordings by another test, and stagings that
// give those claims back, every per-test read of every kind and tier
// (unanswered, rangeUnanswered whole and stopped early, the manager's walk)
// gives what a walk of every owned position gives, with the verify mode on;
// and the manager's walk, which reads the staged tier after the test's own,
// gives what its whole read gives.
func TestPerTestReadsGiveTheWalksAnswerOverRandomConsumesAndClaims(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	defer exactViewVerify.Store(exactViewVerify.Load())
	exactViewVerify.Store(true)
	rng := rand.New(rand.NewSource(7))
	kinds := []models.Kind{models.HTTP, models.MySQL, models.Postgres}
	for round := 0; round < 20; round++ {
		now := time.Now()
		var owned, cands []*models.Mock
		for i := 0; i < 40; i++ {
			owned = append(owned, liveMock(fmt.Sprintf("own-%d", i), kinds[rng.Intn(len(kinds))], now.Add(time.Duration(i)*time.Millisecond), rng.Intn(3) == 0))
		}
		for i := 0; i < 20; i++ {
			cands = append(cands, liveMock(fmt.Sprintf("shared-%d", i), kinds[rng.Intn(len(kinds))], now.Add(time.Duration(40+i)*time.Millisecond), rng.Intn(3) == 0))
		}
		// Protocol-phase recordings (DNS) staged in the per-test tier:
		// every view shows them, after the test's own, so the manager's
		// walk must read the tier once the test's own run out.
		var phase []*models.Mock
		for i := 0; i < 4; i++ {
			phase = append(phase, liveMock(fmt.Sprintf("phase-%d", i), models.DNS, now.Add(time.Duration(60+i)*time.Millisecond), false))
		}
		mm := NewMockManager(nil, nil, zap.NewNop())
		mm.SetMocksWithWindow(phase, nil, now.Add(-time.Hour), now.Add(time.Hour))
		mm.ServeExactTestConcurrent("T", owned, cands)
		v := mm.exactView()
		if v == nil {
			t.Fatal("no exact view after ServeExactTestConcurrent")
		}
		if all, err := mm.GetPerTestMocksInWindow(); err != nil || !slices.Equal(all[len(all)-len(phase):], phase) {
			t.Fatalf("round %d: precondition: the per-test read ends [%s], want the staged tier [%s] (err %v)", round, verifyNames(all), verifyNames(phase), err)
		}
		for step := 0; step < 120; step++ {
			switch op := rng.Intn(10); {
			case op < 6:
				mk := v.owned[rng.Intn(len(v.owned))]
				mm.exactConsume(mk, true)
			case op < 8:
				mk := v.owned[len(owned)+rng.Intn(len(cands))]
				mm.exact.claims.claim(mm.stagingEpoch.Load(), mk.Name, "other")
			default:
				// A staging moves the epoch: the claims of the last one
				// are given back, and a shared recording shows again.
				mm.stagingEpoch.Add(1)
			}
			for _, kind := range append([]models.Kind{""}, kinds...) {
				for _, only := range []bool{false, true} {
					v.mu.Lock()
					want := v.unansweredWalkLocked(kind, only, 0, len(v.owned))
					v.mu.Unlock()
					what := fmt.Sprintf("round %d step %d kind %q session %v", round, step, kind, only)
					var got []*models.Mock
					if only {
						got = v.unanswered(kind, true)
					} else {
						got = v.unanswered(kind)
					}
					liveSame(t, what+": unanswered", want, got)
					var ranged []*models.Mock
					v.rangeUnanswered(kind, only, func(mk *models.Mock) bool {
						ranged = append(ranged, mk)
						return true
					})
					liveSame(t, what+": rangeUnanswered", want, ranged)
					stop := rng.Intn(len(want) + 1)
					var early []*models.Mock
					whole := v.rangeUnanswered(kind, only, func(mk *models.Mock) bool {
						early = append(early, mk)
						return len(early) < stop
					})
					if stop == 0 {
						stop = min(1, len(want))
					}
					liveSame(t, what+": rangeUnanswered stopped early", want[:stop], early)
					if whole != (len(want) == 0) {
						t.Fatalf("%s: rangeUnanswered reported %v stopping after %d of %d", what, whole, len(early), len(want))
					}
				}
			}
			all, err := mm.GetPerTestMocksInWindow()
			if err != nil {
				t.Fatal(err)
			}
			var walked []*models.Mock
			if err := mm.RangePerTestMocksInWindow(func(mk *models.Mock) bool {
				walked = append(walked, mk)
				return true
			}); err != nil {
				t.Fatal(err)
			}
			liveSame(t, fmt.Sprintf("round %d step %d: the manager's per-test walk", round, step), all, walked)
		}
		mm.Close()
	}
}

func liveSame(t *testing.T, what string, want, got []*models.Mock) {
	t.Helper()
	if !slices.Equal(want, got) {
		t.Fatalf("%s: got [%s], want [%s]", what, verifyNames(got), verifyNames(want))
	}
}

// TestRacingCallsOfOneTestAnswerEachOfItsRecordingsOnce: 8 goroutines each
// take the test's next recording from the per-test walk and consume it, as
// an 8-way client's calls do; every recording answers exactly once.
func TestRacingCallsOfOneTestAnswerEachOfItsRecordingsOnce(t *testing.T) {
	const n = 2000
	mm, mocks, _, _ := keyedSet(t, n)
	var mu sync.Mutex
	served := map[string]int{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				var took *models.Mock
				if err := mm.RangePerTestMocksInWindow(func(mk *models.Mock) bool {
					if mm.exactConsume(mk, true) == exactServed {
						took = mk
						return false
					}
					return true
				}); err != nil {
					t.Error(err)
					return
				}
				if took == nil {
					return
				}
				mu.Lock()
				served[took.Name]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, mk := range mocks {
		if served[mk.Name] != 1 {
			t.Fatalf("%s answered %d times, want once", mk.Name, served[mk.Name])
		}
	}
}

// A scoped worker's per-test walk sees what its GetPerTestMocksInWindow sees,
// through the manager's walk or, when the wrapped store has none, through a
// walk of that read. A recording the worker's open test consumed is in
// neither: the consume holds it for the scope and leaves it in the store, so
// only the walk's filter hides it.
func TestScopedMockDb_PerTestWalkIsScopedToTheWorker(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	var perTest []*models.Mock
	for i, n := range []string{"mine", "theirs", "shared", "mine-too"} {
		perTest = append(perTest, newMockForTest(n, base.Add(time.Duration(i)*time.Millisecond), models.LifetimePerTest))
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(perTest, nil, base, base.Add(time.Second))
	allow := []string{"mine", "mine-too"}
	universe := []string{"mine", "theirs", "mine-too"}
	walk := func(db integrations.MockMemDb) []string {
		var got []string
		if err := db.(integrations.PerTestRanger).RangePerTestMocksInWindow(func(mk *models.Mock) bool {
			got = append(got, mk.Name)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	want := []string{"mine", "shared", "mine-too"}
	for _, db := range []integrations.MockMemDb{workerScoped(t, mm, allow, universe), workerScoped(t, &fakeMockDb{mocks: perTest}, allow, universe)} {
		read, err := db.GetPerTestMocksInWindow()
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, mk := range read {
			names = append(names, mk.Name)
		}
		if got := walk(db); !slices.Equal(got, want) || !slices.Equal(names, want) {
			t.Fatalf("the scoped per-test walk gave %v and the read %v; want %v", got, names, want)
		}
	}
	db := workerScoped(t, mm, allow, universe)
	if !db.DeleteFilteredMock(*perTest[0]) {
		t.Fatal("precondition: the worker's open test could not consume its own recording")
	}
	if got, _ := mm.GetPerTestMocksInWindow(); len(got) != len(perTest) {
		t.Fatalf("precondition: the store holds %d per-test recordings after the scoped consume, want all %d (the consume holds, it does not delete)", len(got), len(perTest))
	}
	read, err := db.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, mk := range read {
		names = append(names, mk.Name)
	}
	want = []string{"shared", "mine-too"}
	if got := walk(db); !slices.Equal(got, want) || !slices.Equal(names, want) {
		t.Fatalf("after the worker's test consumed %q the scoped per-test walk gave %v and the read %v; want %v", perTest[0].Name, got, names, want)
	}
}

// TestAPerTestWalkOfAHeldStoreStopsWhereTheStagingMoved: the held store's
// per-test walk gives its caller no mock read once the replay stages a test
// set again, even when the staging moves partway through the walk.
func TestAPerTestWalkOfAHeldStoreStopsWhereTheStagingMoved(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	now := time.Now()
	first := r3Mock("t-jobs-1", now, "GET", "/jobs/1", "", `{"job":1}`)
	second := r3Mock("t-jobs-2", now.Add(time.Millisecond), "GET", "/jobs/2", "", `{"job":2}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	stage := func() {
		mocks := []*models.Mock{first, second}
		mm.SetMocksWithWindow(nil, mocks, models.BaseTime, now.Add(time.Hour))
		mm.ServeExactTest("T", mocks)
	}
	walk := func(db integrations.MockMemDb, each func()) (names []string) {
		t.Helper()
		if err := db.(integrations.PerTestRanger).RangePerTestMocksInWindow(func(mk *models.Mock) bool {
			names = append(names, mk.Name)
			each()
			return true
		}); err != nil {
			t.Fatal(err)
		}
		return names
	}
	stage()
	if got := walk(mm, func() {}); len(got) != 2 {
		t.Fatalf("precondition: the walk reaches %v; want both of the test's recordings", got)
	}
	held, restaged := mm.HoldToRound(mm.MarkCallRound())
	if got := walk(held, stage); len(got) != 1 {
		t.Fatalf("a per-test walk the staging moved under gave %v; want only the mock it read before the move", got)
	}
	if !restaged() {
		t.Fatal("a per-test walk the staging moved under does not say the set was staged again")
	}
}
