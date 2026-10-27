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
func TestExactKeyedWalkReadsTheOpenTestsMocksThroughItsIndex(t *testing.T) {
	base := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)
	var computed atomic.Int64
	ix := &integrations.MockIndex{Keys: func(m *models.Mock) []string {
		computed.Add(1)
		return m.Noise
	}}
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
			mk := newMockForTest(fmt.Sprintf("o%d", i), base.Add(time.Duration(100+i)*time.Millisecond), models.LifetimePerTest)
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

		walk := func(key string) []*models.Mock {
			var got []*models.Mock
			if err := mm.RangeSessionMocksWithKey(ix, key, func(mk *models.Mock) bool {
				got = append(got, mk)
				return true
			}); err != nil {
				t.Fatal(err)
			}
			return got
		}
		// The reference: every unanswered owned mock's keys computed, then
		// the staged mocks the view shows, through the trees' index.
		reference := func(key string) []*models.Mock {
			var want []*models.Mock
			for _, mk := range v.unanswered("") {
				if slices.Contains(mk.Noise, key) {
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
		for _, key := range append(keys, "none") {
			got, want := walk(key), reference(key)
			if len(got) != len(want) {
				t.Fatalf("seed %d, key %q: got %d mocks, want %d", seed, key, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("seed %d, key %q: mock %d is %s, want %s", seed, key, i, got[i].Name, want[i].Name)
				}
			}
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
