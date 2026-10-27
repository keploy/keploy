package proxy

import (
	"fmt"
	"math/rand"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Under exact serving, RangeSessionMocksWithKey gives the open test's
// unanswered mocks filed under the key first, in recorded order, then the
// staged mocks the view shows: what it gave when it computed every owned
// mock's keys on each lookup. The owned ones now come from the view's own
// key index, built at its first lookup, so later lookups compute no keys.
// Some of the open test's mocks were recorded with a session lifetime: the
// view serves them as its own too. An index with OwnedKeys files the open
// test's mocks by OwnedKeys alone, and the staged ones by Keys.
func TestExactKeyedWalkReadsTheOpenTestsMocksThroughItsIndex(t *testing.T) {
	base := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)
	var computed, ownedKeys, ownedByKeys atomic.Int64
	ix := &integrations.MockIndex{Keys: func(m *models.Mock) []string {
		computed.Add(1)
		return m.Noise
	}}
	// byOwned files the open test's mocks under "a" alone.
	byOwned := &integrations.MockIndex{
		Keys: func(m *models.Mock) []string {
			if m.Name[0] == 'o' {
				ownedByKeys.Add(1)
			}
			return m.Noise
		},
		OwnedKeys: func(m *models.Mock) []string {
			ownedKeys.Add(1)
			if slices.Contains(m.Noise, "a") {
				return []string{"a"}
			}
			return nil
		},
	}
	keys := []string{"a", "b", "c"}
	for seed := int64(1); seed <= 10; seed++ {
		r := rand.New(rand.NewSource(seed))
		var staged, owned []*models.Mock
		for i := 0; i < 60; i++ {
			mk := newMockForTest(fmt.Sprintf("s%d", i), base.Add(time.Duration(i)*time.Millisecond), models.LifetimeSession)
			mk.Noise = []string{keys[r.Intn(3)]}
			staged = append(staged, mk)
		}
		for i := 0; i < 400; i++ {
			lifetime := models.LifetimePerTest
			if r.Intn(3) == 0 {
				lifetime = models.LifetimeSession
			}
			mk := newMockForTest(fmt.Sprintf("o%d", i), base.Add(time.Duration(100+i)*time.Millisecond), lifetime)
			for _, k := range keys {
				if r.Intn(3) == 0 {
					mk.Noise = append(mk.Noise, k, k) // a key given twice is filed once
				}
			}
			owned = append(owned, mk)
		}
		mm := NewMockManager(nil, nil, zap.NewNop())
		mm.SetMocksWithWindow(nil, staged, time.Time{}, time.Time{})
		mm.ServeExactTest("T", owned)
		v := mm.exactView()
		if v == nil {
			t.Fatal("no exact view after ServeExactTest")
		}
		// Some of the open test's mocks have answered.
		v.mu.Lock()
		for i := range v.consumed {
			v.consumed[i] = r.Intn(4) == 0
		}
		v.mu.Unlock()

		if !slices.Contains(v.session, true) || !slices.Contains(v.session, false) {
			t.Fatalf("seed %d: precondition: the open test owns mocks of both lifetimes", seed)
		}
		walkIn := func(ix *integrations.MockIndex, key string) []*models.Mock {
			var got []*models.Mock
			if err := mm.RangeSessionMocksWithKey(ix, key, func(mk *models.Mock) bool {
				got = append(got, mk)
				return true
			}); err != nil {
				t.Fatal(err)
			}
			return got
		}
		walk := func(key string) []*models.Mock { return walkIn(ix, key) }
		// The reference: every unanswered owned mock's keys computed, then
		// the staged mocks the view shows, through the trees' index.
		referenceOf := func(ownedKeys func(*models.Mock) []string, key string) []*models.Mock {
			var want []*models.Mock
			for _, mk := range v.unanswered("") {
				if slices.Contains(ownedKeys(mk), key) {
					want = append(want, mk)
				}
			}
			mm.treesMu.RLock()
			session := mm.unfiltered
			mm.treesMu.RUnlock()
			session.rangeKeyed(ix, key, func(mk *models.Mock) bool {
				if v.shows(mk) {
					want = append(want, mk)
				}
				return true
			})
			return want
		}
		reference := func(key string) []*models.Mock {
			return referenceOf(func(mk *models.Mock) []string { return mk.Noise }, key)
		}
		same := func(name, key string, got, want []*models.Mock) {
			t.Helper()
			if len(got) != len(want) {
				t.Fatalf("seed %d, %s, key %q: got %d mocks, want %d", seed, name, key, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("seed %d, %s, key %q: mock %d is %s, want %s", seed, name, key, i, got[i].Name, want[i].Name)
				}
			}
		}
		for _, key := range append(keys, "none") {
			same("Keys", key, walk(key), reference(key))
		}
		ownedKeys.Store(0)
		for _, key := range append(keys, "none") {
			want := referenceOf(func(mk *models.Mock) []string {
				if slices.Contains(mk.Noise, "a") {
					return []string{"a"}
				}
				return nil
			}, key)
			same("OwnedKeys", key, walkIn(byOwned, key), want)
		}
		if n := ownedByKeys.Load(); n != 0 {
			t.Fatalf("seed %d: an index with OwnedKeys computed the Keys of %d of the open test's mocks, want 0", seed, n)
		}
		if n := ownedKeys.Load(); n != int64(len(owned)) {
			t.Fatalf("seed %d: four lookups computed OwnedKeys %d times, want %d (each owned mock once)", seed, n, len(owned))
		}
		// Once the view's index and the trees' are built, a lookup computes
		// no mock's keys, whatever the test owns.
		before := computed.Load()
		for i := 0; i < 20; i++ {
			walk(keys[i%3])
		}
		if n := computed.Load() - before; n != 0 {
			t.Fatalf("seed %d: 20 lookups computed %d mocks' keys, want 0 (the open test owns %d)", seed, n, len(owned))
		}
		// Stopping early stops the walk.
		calls := 0
		_ = mm.RangeSessionMocksWithKey(ix, "a", func(*models.Mock) bool { calls++; return calls < 2 })
		if len(reference("a")) >= 2 && calls != 2 {
			t.Fatalf("seed %d: the walk went on after fn returned false (%d calls)", seed, calls)
		}
		mm.Close()
	}
}

// The views' indexes are bounded: lookups in many views, each built once,
// leave at most ownedIndexCap of them, and a view's index is rebuilt with the
// same mocks after it is dropped.
func TestExactOwnedIndexesStayBounded(t *testing.T) {
	base := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)
	ix := &integrations.MockIndex{Keys: func(m *models.Mock) []string { return m.Noise }}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, nil, time.Time{}, time.Time{})
	for i := 0; i < 3*ownedIndexCap; i++ {
		mk := newMockForTest(fmt.Sprintf("o%d", i), base, models.LifetimePerTest)
		mk.Noise = []string{"k"}
		mm.ServeExactTest(fmt.Sprintf("T%d", i), []*models.Mock{mk})
		n := 0
		if err := mm.RangeSessionMocksWithKey(ix, "k", func(got *models.Mock) bool {
			if got.Name != mk.Name {
				t.Fatalf("view %d: got %s, want %s", i, got.Name, mk.Name)
			}
			n++
			return true
		}); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("view %d: got %d mocks, want 1", i, n)
		}
		if i%ownedIndexCap == 0 {
			runtime.GC()
		}
		ownedIndexes.mu.Lock()
		size := len(ownedIndexes.m)
		ownedIndexes.mu.Unlock()
		if size > ownedIndexCap {
			t.Fatalf("view %d: %d indexes held, want at most %d", i, size, ownedIndexCap)
		}
	}
}

// A shared recording another test answered with is in no keyed read of the
// open test, as it is in none of its other reads (exactView.takenByOther),
// and one the open test answered with leaves its keyed reads too.
func TestExactKeyedWalkLeavesOutASharedRecordingAnotherTestTook(t *testing.T) {
	ix := &integrations.MockIndex{Keys: func(m *models.Mock) []string { return m.Noise }}
	c1 := exactMock("c1", 0, models.LifetimePerTest)
	c2 := exactMock("c2", time.Second, models.LifetimePerTest)
	own := exactMock("own", 2*time.Second, models.LifetimePerTest)
	for _, mk := range []*models.Mock{c1, c2, own} {
		mk.Noise = []string{"k"}
	}
	mm := stagedManager(t, []*models.Mock{c1, c2, own}, nil)
	keyed := func() []*models.Mock {
		var got []*models.Mock
		if err := mm.RangeSessionMocksWithKey(ix, "k", func(mk *models.Mock) bool {
			got = append(got, mk)
			return true
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	mm.ServeExactPool("-", nil)
	mm.ServeExactTestConcurrent("A", nil, []*models.Mock{c1, c2})
	equalNames(t, "A's keyed read", keyed(), "c1", "c2")
	pt, _ := mm.GetPerTestMocksInWindow()
	equalNames(t, "A's view", pt, "c1", "c2")
	if !mm.DeleteFilteredMock(*pt[1]) {
		t.Fatal("A's call of c2 was refused")
	}
	equalNames(t, "A's keyed read after its call of c2", keyed(), "c1")
	mm.ServeExactPool("-", nil)
	mm.ServeExactTestConcurrent("B", []*models.Mock{own}, []*models.Mock{c1, c2})
	pt, _ = mm.GetPerTestMocksInWindow()
	equalNames(t, "B's view", pt, "own", "c1")
	equalNames(t, "B's keyed read", keyed(), "own", "c1")
}
