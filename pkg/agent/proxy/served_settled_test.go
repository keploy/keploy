package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// toldNames makes every serve told to mm's served observer land in the list
// it returns.
func toldNames(mm *MockManager) *[]string {
	told := &[]string{}
	mm.SetServedObserver(func(m *models.Mock) { *told = append(*told, m.Name) })
	return told
}

func wantTold(t *testing.T, what string, told *[]string, want ...string) {
	t.Helper()
	if len(*told) != len(want) {
		t.Fatalf("%s: the served observer was told %v, want %v", what, *told, want)
	}
	for i := range want {
		if (*told)[i] != want[i] {
			t.Fatalf("%s: the served observer was told %v, want %v", what, *told, want)
		}
	}
}

// doorMock is a recording the consume door named goes through for, once
// stagedForDoor staged it.
func doorMock(door, name string, at time.Duration, kind models.Kind) *models.Mock {
	lifetime := models.LifetimeSession
	if door == "DeleteFilteredMock" {
		lifetime = models.LifetimePerTest
	}
	m := exactMock(name, at, lifetime)
	m.Kind = kind
	return m
}

// stagedForDoor is a manager holding ms in the tier the consume door named
// takes them from, so that a consume through it goes through. A staging at
// the base time files the per-test list in the startup tier alone; a test's,
// in the per-test tier.
func stagedForDoor(t *testing.T, door string, ms ...*models.Mock) *MockManager {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	switch door {
	case "DeleteFilteredMock":
		mm.SetMocksWithWindow(ms, nil, exactT0.Add(-time.Hour), exactT0.Add(time.Hour))
	case "DeleteStartupMock":
		mm.SetMocksWithWindow(ms, nil, models.BaseTime, time.Now())
	default:
		mm.SetMocksWithWindow(nil, ms, models.BaseTime, time.Now())
	}
	return mm
}

// updated is the copy a parser hands the update door for m.
func updated(m *models.Mock) *models.Mock {
	c := m.DeepCopy()
	c.TestModeInfo.IsFiltered = false
	c.TestModeInfo.SortOrder = m.TestModeInfo.SortOrder + 1000
	return c
}

// A recording the store offers and no tier it updates holds (a session
// recording staged in the per-test list at the set's first staging: the
// startup tier alone) cannot be consumed through the update door, so its
// consume is refused every time a parser asks. Nothing served it: the
// observer hears nothing, however many times the parser asks, with exact
// serving off and with a view that shows it. Before, it was told of every
// ask, 16 times for one HTTP/2 request that missed.
func TestAConsumeNoTierHoldsTellsTheObserverNothing(t *testing.T) {
	for _, exact := range []bool{false, true} {
		name := "exact serving off"
		if exact {
			name = "a view shows it"
		}
		t.Run(name, func(t *testing.T) {
			stuck := exactMock("boot-token", 0, models.LifetimeSession)
			mm := stagedManager(t, []*models.Mock{stuck}, nil)
			if exact {
				mm.ServeExactPool("k", []string{"boot-token"})
			}
			told := toldNames(mm)
			for i := 0; i < 16; i++ {
				if mm.UpdateUnFilteredMock(stuck, updated(stuck)) {
					t.Fatal("precondition: the update door consumed a recording only the startup tier holds")
				}
			}
			wantTold(t, "16 refused consumes", told)
		})
	}
}

// A request that read a per-test recording another request then took loses
// the delete door and falls through to the update door, which refuses it too.
// The recording was served once, to the winner: the loser, which nothing
// served, is told of through neither door.
func TestARequestLosingAPerTestRecordingIsNotToldAsServed(t *testing.T) {
	for _, exact := range []bool{false, true} {
		name := "exact serving off"
		if exact {
			name = "the open test owns it"
		}
		t.Run(name, func(t *testing.T) {
			m := exactMock("own", 0, models.LifetimePerTest)
			mm := stagedManager(t, []*models.Mock{m}, nil)
			if exact {
				mm.ServeExactTest("T1", []*models.Mock{m})
			}
			told := toldNames(mm)
			if !mm.DeleteFilteredMock(*m) {
				t.Fatal("precondition: the winner's consume was refused")
			}
			wantTold(t, "the winner's serve", told, "own")
			if mm.DeleteFilteredMock(*m) {
				t.Fatal("the loser consumed a recording the winner took")
			}
			if mm.UpdateUnFilteredMock(m, updated(m)) {
				t.Fatal("the loser's fall-through consumed a recording the winner took")
			}
			wantTold(t, "the loser's two refused consumes", told, "own")
		})
	}
}

// Two requests that read the same reusable recording both go through its
// update door: the second finds its copy under the newer one the first filed
// (TreeDb.updateReplacing's ID index), so it is served too, and told.
func TestEveryRequestAReusableRecordingServesIsTold(t *testing.T) {
	for _, exact := range []bool{false, true} {
		name := "exact serving off"
		if exact {
			name = "a view shows it"
		}
		t.Run(name, func(t *testing.T) {
			m := exactMock("session", 0, models.LifetimeSession)
			mm := stagedManager(t, nil, []*models.Mock{m})
			if exact {
				mm.ServeExactPool("k", []string{"session"})
			}
			told := toldNames(mm)
			for i := 0; i < 2; i++ {
				if !mm.UpdateUnFilteredMock(m, updated(m)) {
					t.Fatalf("request %d: the update of a reusable recording was refused", i)
				}
			}
			wantTold(t, "two serves", told, "session", "session")
		})
	}
}

// Every by-value door refuses a mock of the set before this one (stalePooled)
// and a mock its tier does not hold: neither is a serve. With exact serving
// off no view settles those, and every door is its tier's alone.
func TestEveryDoorTellsNothingOfAConsumeItsTierRefuses(t *testing.T) {
	doors := map[string]func(mm *MockManager, m *models.Mock) bool{
		"DeleteFilteredMock":   func(mm *MockManager, m *models.Mock) bool { return mm.DeleteFilteredMock(*m) },
		"DeleteUnFilteredMock": func(mm *MockManager, m *models.Mock) bool { return mm.DeleteUnFilteredMock(*m) },
		"DeleteStartupMock":    func(mm *MockManager, m *models.Mock) bool { return mm.DeleteStartupMock(*m) },
		"UpdateUnFilteredMock": func(mm *MockManager, m *models.Mock) bool { return mm.UpdateUnFilteredMock(m, updated(m)) },
	}
	for name, door := range doors {
		t.Run(name, func(t *testing.T) {
			mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
			defer mm.Close()
			told := toldNames(mm)
			if door(mm, &models.Mock{Name: "not-held", Kind: models.HTTP}) {
				t.Fatal("a door consumed a mock the manager does not hold")
			}
			wantTold(t, "a mock no tier holds", told)
		})
	}
	stale := func(t *testing.T, exact bool) {
		m := exactMock("old-pool", 0, models.LifetimeSession)
		mm := stagedManager(t, nil, []*models.Mock{m})
		if exact {
			mm.ServeExactPool("k", []string{"old-pool"})
		}
		held := *m
		held.TestModeInfo.StagedSet = mm.setGen.Load() + 1 // not this set's
		told := toldNames(mm)
		if mm.MarkMockAsUsed(held) {
			t.Fatal("MarkMockAsUsed consumed a mock of another set's pool")
		}
		if mm.UpdateUnFilteredMock(&held, updated(&held)) {
			t.Fatal("UpdateUnFilteredMock consumed a mock of another set's pool")
		}
		wantTold(t, "another set's mock", told)
	}
	t.Run("another set's pool, exact serving off", func(t *testing.T) { stale(t, false) })
	t.Run("another set's pool, a view shows it", func(t *testing.T) { stale(t, true) })
}

// A refused consume said in its request leaves the request with no serve:
// the learn tracker holds nothing of it, in its running test or in F's order.
func TestARefusedConsumeSaidInItsRequestFilesNothing(t *testing.T) {
	stuck := exactMock("boot-token", 0, models.LifetimeSession)
	mm := stagedManager(t, []*models.Mock{stuck}, nil)
	mm.ServeExactPool("k", []string{"boot-token"})
	tr := learn.NewTracker()
	mm.SetServedObserver(tr.ServedMock)
	mm.SetServedInObserver(tr.ServedMockIn)
	tr.Begin()
	db := mm.ServingFor(tr.Lift())
	for i := 0; i < 16; i++ {
		if db.UpdateUnFilteredMock(stuck, updated(stuck)) {
			t.Fatal("precondition: the update door consumed a recording only the startup tier holds")
		}
	}
	p := tr.Placed()
	if len(p.Served) != 0 {
		t.Fatalf("the tracker holds %v as served in the running test; nothing served them", exactNames(p.Served))
	}
	if f, l := p.Order.Of(stuck); f != 0 || l != 0 {
		t.Fatalf("a consume no tier took is at %d,%d in F's order; want no serve of it", f, l)
	}
}

// heldIn reports whether tree holds a value that is exactly m.
func heldIn(mm *MockManager, tree *TreeDb, m *models.Mock) bool {
	for _, v := range mm.treeValues(tree, "") {
		if v == m {
			return true
		}
	}
	return false
}

// The observer is told once the consume went through, its tier's take and
// what the replay drains (ServedCounts) both done, so a tell never comes of a
// consume that then fails. Under exact serving it is told holding the round
// lock, the take in the same step, so a snapshot reads both or neither.
func TestTheObserverIsToldAfterTheConsumeWentThrough(t *testing.T) {
	roundHeld := func(mm *MockManager) bool {
		if mm.exact.round.mu.TryLock() {
			mm.exact.round.mu.Unlock()
			return false
		}
		return true
	}
	for _, exact := range []bool{false, true} {
		name := "exact serving off"
		if exact {
			name = "a view passes it"
		}
		t.Run(name+"/update", func(t *testing.T) {
			m := exactMock("session", 0, models.LifetimeSession)
			mm := stagedManager(t, nil, []*models.Mock{m})
			if exact {
				mm.ServeExactPool("k", []string{"session"})
			}
			copied := updated(m)
			told := 0
			mm.SetServedObserver(func(*models.Mock) {
				told++
				if !heldIn(mm, mm.unfiltered, copied) {
					t.Error("the observer was told before the tier held the updated copy")
				}
				if mm.ServedCounts()["session"] != 1 {
					t.Error("the observer was told before the consume was flagged")
				}
				if got := roundHeld(mm); got != exact {
					t.Errorf("the round lock held at the tell: %v, want %v", got, exact)
				}
			})
			if !mm.UpdateUnFilteredMock(m, copied) || told != 1 {
				t.Fatalf("the update was not served and told once (told %d)", told)
			}
		})
		t.Run(name+"/mark", func(t *testing.T) {
			m := exactMock("session", 0, models.LifetimeSession)
			mm := stagedManager(t, nil, []*models.Mock{m})
			if exact {
				mm.ServeExactPool("k", []string{"session"})
			}
			told := 0
			mm.SetServedObserver(func(*models.Mock) {
				told++
				if mm.ServedCounts()["session"] != 1 {
					t.Error("the observer was told before the consume was flagged")
				}
				if got := roundHeld(mm); got != exact {
					t.Errorf("the round lock held at the tell: %v, want %v", got, exact)
				}
			})
			if !mm.MarkMockAsUsed(*m) || told != 1 {
				t.Fatalf("the mark was not served and told once (told %d)", told)
			}
		})
	}
	t.Run("delete doors, exact serving off", func(t *testing.T) {
		doors := map[string]struct {
			tree func(mm *MockManager) *TreeDb
			door func(mm *MockManager, m models.Mock) bool
		}{
			"DeleteFilteredMock": {
				func(mm *MockManager) *TreeDb { return mm.filtered },
				func(mm *MockManager, m models.Mock) bool { return mm.DeleteFilteredMock(m) },
			},
			"DeleteUnFilteredMock": {
				func(mm *MockManager) *TreeDb { return mm.unfiltered },
				func(mm *MockManager, m models.Mock) bool { return mm.DeleteUnFilteredMock(m) },
			},
			"DeleteStartupMock": {
				func(mm *MockManager) *TreeDb { return mm.startup },
				func(mm *MockManager, m models.Mock) bool { return mm.DeleteStartupMock(m) },
			},
		}
		for name, d := range doors {
			t.Run(name, func(t *testing.T) {
				m := doorMock(name, "taken", 0, models.HTTP)
				mm := stagedForDoor(t, name, m)
				tree := d.tree(mm)
				if len(mm.treeValues(tree, "")) != 1 {
					t.Fatal("precondition: the door's tier does not hold the recording")
				}
				told := 0
				mm.SetServedObserver(func(*models.Mock) {
					told++
					if n := len(mm.treeValues(tree, "")); n != 0 {
						t.Errorf("the observer was told while the tier still held %d recording(s)", n)
					}
				})
				if !d.door(mm, *m) || told != 1 {
					t.Fatalf("the delete was not served and told once (told %d)", told)
				}
			})
		}
	})
}

// A delete door's tier reads the staging under swapMu, which nothing holding
// the round lock may take, and a tier a view passes to runs holding that lock
// (exactConsumeAt). So a view settles every delete door's consume itself, for
// every kind of recording it shows, in a test or between tests.
func TestNoViewPassesADeleteDoorsConsumeToItsTier(t *testing.T) {
	own := exactMock("own", 0, models.LifetimePerTest)
	pool := exactMock("pool", time.Second, models.LifetimeSession)
	worker := exactMock("worker", 2*time.Second, models.LifetimeSession)
	hs := configMock("handshake", 3*time.Second)
	hs2 := configMock("handshake-2", 4*time.Second)
	mm := stagedManager(t, []*models.Mock{own}, []*models.Mock{pool, worker, hs, hs2})
	mm.ServeExactWorker([]string{"worker"})
	mm.ServeExactPool("k", []string{"pool"})
	for _, m := range []*models.Mock{pool, hs} {
		if v := mm.exactConsume(m, true); v == exactPass {
			t.Fatalf("a pool view passed the delete door's consume of %s", m.Name)
		}
	}
	mm.ServeExactTest("T1", []*models.Mock{own})
	for _, m := range []*models.Mock{own, pool, worker, hs2} {
		if v := mm.exactConsume(m, true); v == exactPass {
			t.Fatalf("a test's view passed the delete door's consume of %s", m.Name)
		}
	}
}

// A consume a view passes goes through its tier holding the round lock, and
// a staging holds swapMu while it waits for that lock, so a tier that took
// swapMu would hold the store still for good. Here swapMu is taken the moment
// the door is about to take the round lock, with the set's learn step ended,
// when MarkMockAsUsed reads the staging: the door goes through all the same.
// The deadline is the test's guard only: a door that took swapMu never
// returns while the test holds it.
func TestAConsumeAViewPassesNeedsNoStagingLockInItsTier(t *testing.T) {
	doors := map[string]func(mm *MockManager, m *models.Mock) bool{
		"MarkMockAsUsed":       func(mm *MockManager, m *models.Mock) bool { return mm.MarkMockAsUsed(*m) },
		"UpdateUnFilteredMock": func(mm *MockManager, m *models.Mock) bool { return mm.UpdateUnFilteredMock(m, updated(m)) },
	}
	for name, door := range doors {
		t.Run(name, func(t *testing.T) {
			provenswitch.Pin(true)
			t.Cleanup(provenswitch.Unpin)
			m := exactMock("session", 0, models.LifetimeSession)
			mm := stagedManager(t, nil, []*models.Mock{m})
			mm.ServeExactPool("k", []string{"session"})
			mm.exact.learnEnded.Store(true)
			told := toldNames(mm)
			mm.exact.round.onWait = func() { mm.swapMu.Lock() }
			done := make(chan bool, 1)
			go func() { done <- door(mm, m) }()
			select {
			case ok := <-done:
				mm.swapMu.Unlock()
				if !ok {
					t.Fatal("the view passed the consume, and its tier refused it")
				}
			case <-time.After(time.Minute):
				mm.swapMu.Unlock()
				<-done
				t.Fatal("the door did not go through with swapMu held: its tier takes it, holding the round lock")
			}
			wantTold(t, "the one serve", told, "session")
		})
	}
}
