package proxy

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A windowed lookup must see every mock the session tree holds while it runs,
// even when an update moves one to another tree ID meanwhile.
//
// The index used to stand down through a flag set before the update and read
// before the lookup took the tree's lock. A lookup that read the flag just
// before an ID-changing update was stored, and resolved its IDs just after it
// landed, looked the moved mock up under its old ID and dropped it. The index
// is now read and changed under the tree's own lock, so a lookup sees the tree
// either before the update or after it, never a mix. Each round races one such
// update against lookups on a fresh manager, because the first one is the only
// update the old flag could miss; the rounds run for a fixed time.
func TestWindowedLookupSeesAMockMovedToAnotherID(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	const n = 3000
	session := make([]*models.Mock, 0, n)
	for i := 0; i < n; i++ {
		session = append(session, newMockForTest(fmt.Sprintf("s%d", i), base.Add(time.Duration(i)*time.Microsecond), models.LifetimeSession))
	}
	start, end := base, base.Add(time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for round := 0; time.Now().Before(deadline); round++ {
		staged := make([]*models.Mock, n)
		for i, mk := range session {
			staged[i] = mk.DeepCopy()
		}
		mm := NewMockManager(nil, nil, zap.NewNop())
		mm.SetMocksWithWindow(nil, staged, start, end)
		target := staged[n/2]
		moved := *target
		moved.TestModeInfo.ID = target.TestModeInfo.ID + 10*n

		var wg sync.WaitGroup
		go_ := make(chan struct{})
		misses := make(chan int, 8)
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-go_
				for k := 0; k < 3; k++ {
					got, err := mm.GetSessionMocksInWindow(start, end)
					if err != nil {
						t.Error(err)
						return
					}
					found := false
					for _, mk := range got {
						if mk.Name == target.Name {
							found = true
							break
						}
					}
					if !found {
						misses <- len(got)
						return
					}
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-go_
			if !mm.UpdateUnFilteredMock(target, &moved) {
				t.Error("UpdateUnFilteredMock failed")
			}
		}()
		close(go_)
		wg.Wait()
		mm.Close()
		select {
		case got := <-misses:
			t.Fatalf("round %d: a windowed lookup returned %d of the %d session mocks, without %q, "+
				"which the tree held throughout under one ID or the other", round, got, n, target.Name)
		default:
		}
	}
}

// sessionIndexed reports whether mm's session tree holds a window index.
func sessionIndexed(mm *MockManager) bool {
	mm.treesMu.RLock()
	tree := mm.unfiltered
	mm.treesMu.RUnlock()
	tree.mu.RLock()
	defer tree.mu.RUnlock()
	return tree.win != nil
}

// The window index stays through the updates matchers make and only those, and
// lookups stay exact through all of them. Whether an update keeps the index is
// decided by what the index files for the mock's ID, not by the mock the tree
// held before, which its caller may have edited in place.
func TestWindowIndexFollowsTheUpdatesItCan(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	stage := func(t *testing.T) (*MockManager, []*models.Mock) {
		t.Helper()
		var session []*models.Mock
		for i := 0; i < 20; i++ {
			session = append(session, newMockForTest(fmt.Sprintf("m%d", i), base.Add(time.Duration(i)*time.Millisecond), models.LifetimeSession))
		}
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.SetMocksWithWindow(nil, session, base, base.Add(time.Second))
		// The first windowed lookup builds the index.
		if _, err := mm.GetSessionMocksInWindow(base, base); err != nil || !sessionIndexed(mm) {
			t.Fatalf("the first lookup left the tree unindexed (%v)", err)
		}
		got, _ := mm.GetSessionMocks()
		return mm, got
	}
	exact := func(t *testing.T, mm *MockManager, step string) {
		t.Helper()
		for _, w := range [][2]time.Duration{{0, 4 * time.Millisecond}, {0, 9 * time.Millisecond}, {5 * time.Millisecond, 9 * time.Millisecond}, {40 * time.Millisecond, 60 * time.Millisecond}} {
			start, end := base.Add(w[0]), base.Add(w[1])
			want := windowOfSessionMocks(t, mm, start, end)
			if got, _ := mm.GetSessionMocksInWindow(start, end); !sameMocks(got, want) {
				t.Fatalf("%s, window %v-%v:\n got  %s\n want %s", step, w[0], w[1], describe(got), describe(want))
			}
		}
	}

	t.Run("a copy re-stamped, as the MySQL and HTTP matchers do", func(t *testing.T) {
		mm, pool := stage(t)
		served := pool[3]
		updated := *served
		updated.TestModeInfo.SortOrder = 1000
		if !mm.UpdateUnFilteredMock(served, &updated) {
			t.Fatal("UpdateUnFilteredMock failed")
		}
		if !sessionIndexed(mm) {
			t.Fatal("the index was dropped by an update it follows")
		}
		exact(t, mm, "after the copy re-stamp")
	})

	t.Run("the pooled mock re-stamped in place and stored back, as a copy of it is passed as old", func(t *testing.T) {
		mm, pool := stage(t)
		served := pool[3]
		old := served.DeepCopy()
		served.TestModeInfo.SortOrder = 1000 // in place, request time unchanged
		if !mm.UpdateUnFilteredMock(old, served) {
			t.Fatal("UpdateUnFilteredMock failed")
		}
		if !sessionIndexed(mm) {
			t.Fatal("the index was dropped by an update that left the mock's ID and request time")
		}
		exact(t, mm, "after the in-place re-stamp")
	})

	t.Run("a copy passed as both old and new", func(t *testing.T) {
		mm, pool := stage(t)
		snapshot := *pool[3]
		if !mm.UpdateUnFilteredMock(&snapshot, &snapshot) {
			t.Fatal("UpdateUnFilteredMock failed")
		}
		if !sessionIndexed(mm) {
			t.Fatal("the index was dropped by an update that left the mock's ID and request time")
		}
		exact(t, mm, "after storing the snapshot")
	})

	t.Run("a pooled mock re-stamped in place twice, the second time found by its ID", func(t *testing.T) {
		mm, pool := stage(t)
		served := pool[3]
		// A first in-place re-stamp whose update lost the race leaves the tree
		// key behind the mock's own sort order, so the next update finds it
		// through the ID index rather than the key.
		served.TestModeInfo.SortOrder = 1000
		old := served.DeepCopy()
		served.TestModeInfo.SortOrder = 1001
		if !mm.UpdateUnFilteredMock(old, served) {
			t.Fatal("UpdateUnFilteredMock failed")
		}
		if !sessionIndexed(mm) {
			t.Fatal("the index was dropped by an update that left the mock's ID and request time")
		}
		exact(t, mm, "after the re-stamp found by ID")
	})

	t.Run("re-stamps of a mock that shares its request time and of an undated one", func(t *testing.T) {
		// Three request times shared by 40 mocks, staged out of time order so
		// the index sorts them, and one undated mock.
		var session []*models.Mock
		for i := 0; i < 40; i++ {
			at := base.Add(time.Duration(2-i%3) * time.Millisecond)
			session = append(session, newMockForTest(fmt.Sprintf("t%02d", i), at, models.LifetimeSession))
		}
		session = append(session, newMockForTest("undated", time.Time{}, models.LifetimeSession))
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.SetMocksWithWindow(nil, session, base, base.Add(time.Second))
		_, _ = mm.GetSessionMocksInWindow(base, base)
		pool, _ := mm.GetSessionMocks()
		for i, mk := range pool {
			updated := *mk
			updated.TestModeInfo.SortOrder = 1000 + int64(i)
			if !mm.UpdateUnFilteredMock(mk, &updated) {
				t.Fatal("UpdateUnFilteredMock failed")
			}
			if !sessionIndexed(mm) {
				t.Fatalf("the index was dropped by a re-stamp of %s", mk.Name)
			}
		}
		exact(t, mm, "after the re-stamps")
	})

	t.Run("a mock moved onto the request time another entry holds", func(t *testing.T) {
		mm, pool := stage(t)
		moved := pool[3]
		moved.Spec.ReqTimestampMock = pool[10].Spec.ReqTimestampMock
		if !mm.UpdateUnFilteredMock(moved, moved) {
			t.Fatal("UpdateUnFilteredMock failed")
		}
		start := pool[10].Spec.ReqTimestampMock
		want := windowOfSessionMocks(t, mm, start, start)
		if got, _ := mm.GetSessionMocksInWindow(start, start); !sameMocks(got, want) || len(got) != 2 {
			t.Fatalf("window at the shared time:\n got  %s\n want %s (both mocks)", describe(got), describe(want))
		}
		exact(t, mm, "after the move onto a held time")
	})

	// An update that stores a mock under an ID another live entry holds: the
	// tree's ID index resolves that ID to one of them, so the index rebuilt
	// from the tree must not resolve through it.
	t.Run("a mock moved onto an ID another entry holds", func(t *testing.T) {
		mm, pool := stage(t)
		moved := *pool[3]
		moved.TestModeInfo.ID = pool[7].TestModeInfo.ID
		moved.TestModeInfo.SortOrder = 1000
		if !mm.UpdateUnFilteredMock(pool[3], &moved) {
			t.Fatal("UpdateUnFilteredMock failed")
		}
		exact(t, mm, "after the move onto a held ID")
	})

	t.Run("a mock moved onto the ID and request time another entry holds", func(t *testing.T) {
		mm, pool := stage(t)
		moved := *pool[3]
		moved.TestModeInfo.ID = pool[7].TestModeInfo.ID
		moved.TestModeInfo.SortOrder = 1000
		moved.Spec.ReqTimestampMock = pool[7].Spec.ReqTimestampMock
		if !mm.UpdateUnFilteredMock(pool[3], &moved) {
			t.Fatal("UpdateUnFilteredMock failed")
		}
		exact(t, mm, "after the move onto a held ID and time")
	})

	// Request times beyond what Unix nanoseconds hold are filed apart; a
	// re-stamp keeps them, and a move onto one, or onto no request time, drops
	// the index.
	t.Run("a mock recorded beyond nanosecond range re-stamped, then moved", func(t *testing.T) {
		var session []*models.Mock
		for i := 0; i < 10; i++ {
			session = append(session, newMockForTest(fmt.Sprintf("m%d", i), base.Add(time.Duration(i)*time.Millisecond), models.LifetimeSession))
		}
		far := time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC)
		session = append(session, newMockForTest("far", far, models.LifetimeSession))
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.SetMocksWithWindow(nil, session, base, base.Add(time.Second))
		_, _ = mm.GetSessionMocksInWindow(base, base)
		pool, _ := mm.GetSessionMocks()
		var farMock *models.Mock
		for _, mk := range pool {
			if mk.Name == "far" {
				farMock = mk
			}
		}
		updated := *farMock
		updated.TestModeInfo.SortOrder = 1000
		if !mm.UpdateUnFilteredMock(farMock, &updated) || !sessionIndexed(mm) {
			t.Fatal("the index was dropped by a re-stamp of a mock recorded beyond nanosecond range")
		}
		for _, to := range []time.Time{far, {}} {
			mm.SetMocksWithWindow(nil, session, base, base.Add(time.Second))
			_, _ = mm.GetSessionMocksInWindow(base, base)
			pool, _ := mm.GetSessionMocks()
			moved := pool[2]
			moved.Spec.ReqTimestampMock = to
			if !mm.UpdateUnFilteredMock(moved, moved) {
				t.Fatal("UpdateUnFilteredMock failed")
			}
			if sessionIndexed(mm) {
				t.Fatalf("the index outlived a move to %v", to)
			}
			for _, w := range [][2]time.Time{{far, far}, {base, base.Add(time.Second)}} {
				want := windowOfSessionMocks(t, mm, w[0], w[1])
				if got, _ := mm.GetSessionMocksInWindow(w[0], w[1]); !sameMocks(got, want) {
					t.Fatalf("after a move to %v, window %v-%v:\n got  %s\n want %s", to, w[0], w[1], describe(got), describe(want))
				}
			}
		}
	})

	// The request time edited in place, then the mock stored back as both old
	// and new: through the tree key (its sort order unchanged) and through the
	// ID index (its sort order bumped in place too, so the old key no longer
	// finds it).
	for _, bump := range []bool{false, true} {
		t.Run(fmt.Sprintf("the request time edited in place, sort order bumped=%v", bump), func(t *testing.T) {
			mm, pool := stage(t)
			moved := pool[3]
			moved.Spec.ReqTimestampMock = base.Add(50 * time.Millisecond)
			if bump {
				moved.TestModeInfo.SortOrder = 1000
			}
			if !mm.UpdateUnFilteredMock(moved, moved) {
				t.Fatal("UpdateUnFilteredMock failed")
			}
			if sessionIndexed(mm) {
				t.Fatal("the index outlived an update that moved a mock's request time")
			}
			exact(t, mm, "after the move")
		})
	}
}

// An insert into the session tree drops the window index: the tree then holds
// an entry the index does not file. The next lookup builds it again and stays
// exact.
func TestWindowIndexIsDroppedByAnInsert(t *testing.T) {
	base := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
	var session []*models.Mock
	for i := 0; i < 5; i++ {
		session = append(session, newMockForTest(fmt.Sprintf("m%d", i), base.Add(time.Duration(i)*time.Millisecond), models.LifetimeSession))
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, session, base, base.Add(time.Second))
	if _, err := mm.GetSessionMocksInWindow(base, base); err != nil || !sessionIndexed(mm) {
		t.Fatalf("the first lookup left the session tree without a window index (%v)", err)
	}
	late := newMockForTest("late", base.Add(2*time.Millisecond), models.LifetimeSession)
	late.TestModeInfo = models.TestModeInfo{ID: 100, SortOrder: 100, Lifetime: models.LifetimeSession}
	mm.treesMu.RLock()
	tree := mm.unfiltered
	mm.treesMu.RUnlock()
	tree.insert(late.TestModeInfo, late)
	if sessionIndexed(mm) {
		t.Fatal("an insert left the window index in place")
	}
	at := base.Add(2 * time.Millisecond)
	if got, _ := mm.GetSessionMocksInWindow(at, at); !containsMock(got, late) || !sameMocks(got, windowOfSessionMocks(t, mm, at, at)) {
		t.Fatalf("window at 2ms after the insert: %s", describe(got))
	}
	if !sessionIndexed(mm) {
		t.Fatal("the lookup after the insert did not build the index again")
	}
}
