package proxy

import (
	"fmt"
	"math/rand"
	"runtime"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// byKindAndName files a mock under its kind, under its name, and under
// "all": a walk under "all" reads the open test's own and then the staged
// tier, so a walk stopped among the test's own must not go on to the tier.
var byKindAndName = &integrations.MockIndex{Keys: func(mk *models.Mock) []string {
	return []string{string(mk.Kind), "name:" + mk.Name, "all"}
}}

// perTestUnder is what a keyed per-test walk must hand: the whole per-test
// read, in its order, narrowed to the mocks ix files under key.
func perTestUnder(t *testing.T, db integrations.MockMemDb, ix *integrations.MockIndex, key string) []*models.Mock {
	t.Helper()
	all, err := db.GetPerTestMocksInWindow()
	if err != nil {
		t.Fatal(err)
	}
	var out []*models.Mock
	for _, mk := range all {
		if mk != nil && slices.Contains(ix.Keys(mk), key) {
			out = append(out, mk)
		}
	}
	return out
}

// keyedPerTestWalk is the keyed per-test walk of db under key, stopped after
// stop mocks (never with stop < 0).
func keyedPerTestWalk(t *testing.T, db integrations.MockMemDb, ix *integrations.MockIndex, key string, stop int) []*models.Mock {
	t.Helper()
	var got []*models.Mock
	if err := db.(integrations.PerTestKeyRanger).RangePerTestMocksWithKey(ix, key, func(mk *models.Mock) bool {
		got = append(got, mk)
		return stop < 0 || len(got) < stop
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestAKeyedPerTestWalkIsThePerTestReadUnderItsKey: over random consumes (of
// the open test's own recordings and of the ones it shares), claims of shared
// recordings by another test, and stagings that give those claims back, the
// keyed per-test walk under every kind and under single names hands exactly
// the whole per-test read narrowed to the key, in its order, with the verify
// mode on: the test's own first, then the staged tier (protocol-phase DNS
// recordings the read takes after them); stopped early, its first ones. With
// exact serving off the walk is the read narrowed alike.
func TestAKeyedPerTestWalkIsThePerTestReadUnderItsKey(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	defer exactViewVerify.Store(exactViewVerify.Load())
	exactViewVerify.Store(true)
	rng := rand.New(rand.NewSource(11))
	kinds := []models.Kind{models.HTTP, models.MySQL, models.Mongo}
	for round := 0; round < 12; round++ {
		now := time.Now()
		var owned, cands, phase []*models.Mock
		for i := 0; i < 30; i++ {
			owned = append(owned, liveMock(fmt.Sprintf("own-%d", i), kinds[rng.Intn(len(kinds))], now.Add(time.Duration(i)*time.Millisecond), rng.Intn(3) == 0))
		}
		for i := 0; i < 12; i++ {
			cands = append(cands, liveMock(fmt.Sprintf("shared-%d", i), kinds[rng.Intn(len(kinds))], now.Add(time.Duration(30+i)*time.Millisecond), rng.Intn(3) == 0))
		}
		for i := 0; i < 3; i++ {
			phase = append(phase, liveMock(fmt.Sprintf("phase-%d", i), models.DNS, now.Add(time.Duration(50+i)*time.Millisecond), false))
		}
		mm := NewMockManager(nil, nil, zap.NewNop())
		mm.SetMocksWithWindow(phase, nil, now.Add(-time.Hour), now.Add(time.Hour))
		mm.ServeExactTestConcurrent("T", owned, cands)
		v := mm.exactView()
		if v == nil {
			t.Fatal("no exact view after ServeExactTestConcurrent")
		}
		keys := []string{string(models.DNS)}
		for _, k := range kinds {
			keys = append(keys, string(k))
		}
		for step := 0; step < 60; step++ {
			switch op := rng.Intn(10); {
			case op < 6:
				mm.exactConsume(v.owned[rng.Intn(len(v.owned))], true)
			case op < 8:
				mk := v.owned[len(owned)+rng.Intn(len(cands))]
				mm.exact.claims.claim(mm.stagingEpoch.Load(), mk.Name, "other")
			default:
				mm.stagingEpoch.Add(1)
			}
			name := "name:" + v.owned[rng.Intn(len(v.owned))].Name
			for _, key := range append(keys, name, "name:phase-1", "all", "none") {
				what := fmt.Sprintf("round %d step %d key %q", round, step, key)
				want := perTestUnder(t, mm, byKindAndName, key)
				liveSame(t, what+": whole", want, keyedPerTestWalk(t, mm, byKindAndName, key, -1))
				stop := 1 + rng.Intn(len(want)+1)
				liveSame(t, what+": stopped early", want[:min(stop, len(want))], keyedPerTestWalk(t, mm, byKindAndName, key, stop))
			}
		}
		if got := perTestUnder(t, mm, byKindAndName, string(models.DNS)); !slices.Equal(got, phase) {
			t.Fatalf("round %d: precondition: the per-test read under DNS is [%s], want the staged tier [%s]", round, verifyNames(got), verifyNames(phase))
		}
		mm.Close()
	}

	// Exact serving off: the read walked, narrowed alike.
	t.Setenv(provenswitch.Env, "")
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	var perTest []*models.Mock
	for i, n := range []string{"a", "b", "c", "d"} {
		mk := newMockForTest(n, base.Add(time.Duration(i)*time.Millisecond), models.LifetimePerTest)
		mk.Kind = kinds[i%2]
		perTest = append(perTest, mk)
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(perTest, nil, base, base.Add(time.Second))
	if mm.exactView() != nil {
		t.Fatal("precondition: an exact view with exact serving off")
	}
	for _, key := range []string{string(models.HTTP), string(models.MySQL), "name:c"} {
		want := perTestUnder(t, mm, byKindAndName, key)
		if len(want) == 0 {
			t.Fatalf("precondition: nothing under %q", key)
		}
		liveSame(t, "exact serving off, key "+key, want, keyedPerTestWalk(t, mm, byKindAndName, key, -1))
		liveSame(t, "exact serving off, key "+key+", stopped at one", want[:1], keyedPerTestWalk(t, mm, byKindAndName, key, 1))
	}
}

// keyedCallCost answers, in recorded order, every even mock of one test that
// owns n (keyedSet), one call at a time, as a matcher that reads the per-test
// tier under its call's key and stops at the key's next recording does. It
// returns what one such read allocated, the median over the calls (a runtime
// goroutine allocating while one read is measured moves no median): how many
// times and how many bytes; and the median bytes of a whole per-test read
// (GetPerTestMocksInWindow), the read the keyed walk spares the call.
func keyedCallCost(t *testing.T, n int) (allocs, bytes, wholeBytes uint64) {
	t.Helper()
	defer exactViewVerify.Store(exactViewVerify.Load())
	exactViewVerify.Store(false) // the verify mode walks every position: measured alone
	mm, mocks, ix, _ := keyedSet(t, n)
	const calls = 40
	read := func() *models.Mock {
		var first *models.Mock
		if err := mm.RangePerTestMocksWithKey(ix, "even", func(mk *models.Mock) bool {
			first = mk
			return false
		}); err != nil {
			t.Fatal(err)
		}
		return first
	}
	read() // the view keys its mocks on its first keyed read, once
	var before, after runtime.MemStats
	var mallocs, total, whole []uint64
	for k := 0; k < calls; k++ {
		runtime.ReadMemStats(&before)
		first := read()
		runtime.ReadMemStats(&after)
		mallocs = append(mallocs, after.Mallocs-before.Mallocs)
		total = append(total, after.TotalAlloc-before.TotalAlloc)
		if want := mocks[2*k].Name; first == nil || first.Name != want {
			t.Fatalf("call %d: the keyed walk handed [%s] first, want %s (the key's next in recorded order)", k, verifyNames([]*models.Mock{first}), want)
		}
		runtime.ReadMemStats(&before)
		if _, err := mm.GetPerTestMocksInWindow(); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		whole = append(whole, after.TotalAlloc-before.TotalAlloc)
		if got := mm.exactConsume(first, true); got != exactServed {
			t.Fatalf("call %d: consume of %s gave %v, want served", k, first.Name, got)
		}
	}
	median := func(xs []uint64) uint64 {
		slices.Sort(xs)
		return xs[len(xs)/2]
	}
	return median(mallocs), median(total), median(whole)
}

// TestAKeyedPerTestReadAllocatesTheSameAtAnySize: a call that reads the
// per-test tier under its key and stops at the key's next recording
// allocates as often and as much when the open test owns 100 recordings as
// when it owns 5,000: it copies none of them. Counted, not timed. The whole
// per-test read, which copies every unanswered one on each call, allocates
// tens of kilobytes more at 5,000, so the row tells a copy from none.
func TestAKeyedPerTestReadAllocatesTheSameAtAnySize(t *testing.T) {
	smallAllocs, smallBytes, smallWhole := keyedCallCost(t, 100)
	bigAllocs, bigBytes, bigWhole := keyedCallCost(t, 5000)
	t.Logf("a keyed read: %d allocations, %d bytes a call at 100 recordings; %d, %d at 5,000; a whole read: %d bytes at 100, %d at 5,000", smallAllocs, smallBytes, bigAllocs, bigBytes, smallWhole, bigWhole)
	if bigWhole < smallWhole+20000 {
		t.Fatalf("precondition: a whole per-test read allocated %d bytes a call at 100 recordings and %d at 5,000: the row cannot tell a copy of the read", smallWhole, bigWhole)
	}
	if bigAllocs != smallAllocs || bigBytes != smallBytes {
		t.Fatalf("a keyed per-test read that stops at the key's next recording allocated %d times and %d bytes a call when the test owns 100 recordings, %d times and %d bytes when it owns 5,000; want the same at any size", smallAllocs, smallBytes, bigAllocs, bigBytes)
	}
}

// TestScopedMockDb_KeyedPerTestWalkIsScopedToTheWorker: a scoped worker's
// keyed per-test walk is its GetPerTestMocksInWindow narrowed to the key,
// through the manager's walk or, when the wrapped store has none, through a
// walk of that read; a recording the worker's open test consumed is in
// neither.
func TestScopedMockDb_KeyedPerTestWalkIsScopedToTheWorker(t *testing.T) {
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
	kind := string(perTest[0].Kind)
	for _, db := range []integrations.MockMemDb{workerScoped(t, mm, allow, universe), workerScoped(t, &fakeMockDb{mocks: perTest}, allow, universe)} {
		want := perTestUnder(t, db, byKindAndName, kind)
		if got := verifyNames(want); got != "mine, shared, mine-too" {
			t.Fatalf("precondition: the scoped per-test read under its kind is [%s]", got)
		}
		liveSame(t, "the scoped keyed walk", want, keyedPerTestWalk(t, db, byKindAndName, kind, -1))
		liveSame(t, "the scoped keyed walk under one name", perTestUnder(t, db, byKindAndName, "name:shared"), keyedPerTestWalk(t, db, byKindAndName, "name:shared", -1))
	}
	db := workerScoped(t, mm, allow, universe)
	if !db.DeleteFilteredMock(*perTest[0]) {
		t.Fatal("precondition: the worker's open test could not consume its own recording")
	}
	want := perTestUnder(t, db, byKindAndName, kind)
	if got := verifyNames(want); got != "shared, mine-too" {
		t.Fatalf("precondition: after the scoped consume the read under its kind is [%s]", got)
	}
	liveSame(t, "the scoped keyed walk after the worker's test consumed "+perTest[0].Name, want, keyedPerTestWalk(t, db, byKindAndName, kind, -1))
}
