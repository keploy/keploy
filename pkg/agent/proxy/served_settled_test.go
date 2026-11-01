package proxy

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
			mm.SetServedObserver(func(mk *models.Mock) {
				told++
				if mk != copied {
					t.Error("the observer was told of old, not the copy that carries what was learned")
				}
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

// The Warn a consume of a recording no staging holds logs, read from logs.
func unstagedWarns(logs *observer.ObservedLogs) []observer.LoggedEntry {
	return logs.FilterMessageSnippet("the test set's staging does not hold").All()
}

// A test's own mock is not in the staged trees: its view hands it out from
// the test's mocks. A retry cycle of record's full round consumes it after
// the set's learn step ended, through MarkMockAsUsed for most parsers. The
// view serves it, so nothing reads it against the staging: no Warn says the
// staging does not hold it and that its test has no verdict.
func TestATestsOwnMockMarkedAfterTheLearnStepEndedWarnsNothing(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	core, logs := observer.New(zapcore.WarnLevel)
	pool := exactMock("pool", time.Second, models.LifetimeSession)
	mm := stagedManagerLogged(t, zap.New(core), nil, []*models.Mock{pool})
	mm.ServeExactPool("k", []string{"pool"})
	mm.SetLearnEnded(true)
	var owned []*models.Mock
	for i := 0; i < 5; i++ {
		owned = append(owned, exactMock("own-"+strconv.Itoa(i), time.Duration(10+i)*time.Second, models.LifetimePerTest))
	}
	mm.ServeExactTest("T1", owned)
	for _, m := range owned {
		if !mm.MarkMockAsUsed(*m) {
			t.Fatalf("precondition: the view did not serve its own %s", m.Name)
		}
	}
	if said := unstagedWarns(logs); len(said) != 0 {
		t.Fatalf("%d Warn(s) for %d mocks the view served, the first of %v", len(said), len(owned), said[0].ContextMap())
	}
}

// A consume a view passes of a recording the staging does not hold, after the
// learn step ended, is still read as learned then (fail closed) and said in
// a Warn, once in the staging, from the tier, holding the round lock. One the
// tier refuses as an earlier set's says nothing: it consumed nothing.
func TestAMarkTheViewPassesOfARecordingNoStagingHoldsIsSaidOnce(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	core, logs := observer.New(zapcore.WarnLevel)
	pool := exactMock("pool", time.Second, models.LifetimeSession)
	mm := stagedManagerLogged(t, zap.New(core), nil, []*models.Mock{pool})
	mm.ServeExactPool("k", []string{"pool"})
	mm.SetLearnEnded(true)
	ghost := configMock("ghost", 2*time.Second)
	ghost.Spec.ReqBodyNoise = map[string][]string{"body.at": {}}
	told := toldNames(mm)
	earlier := configMock("earlier-set", 3*time.Second)
	earlier.TestModeInfo.StagedSet = mm.setGen.Load() + 1
	if mm.MarkMockAsUsed(*earlier) {
		t.Fatal("precondition: the tier took a consume of an earlier set's recording")
	}
	for i := 0; i < 2; i++ {
		if !mm.MarkMockAsUsed(*ghost) {
			t.Fatal("precondition: the view did not pass the consume, or its tier refused it")
		}
		st := mm.GetConsumedMocks()
		if len(st) != 1 || !st[0].LearnedAfterEnd {
			t.Fatalf("consume %d: %+v; want it to say it learned after the end", i+1, st)
		}
		// Said by the first consume, which takes its test's verdict away,
		// and by no consume after it.
		if said := unstagedWarns(logs); len(said) != 1 || said[0].ContextMap()["mock"] != "ghost" {
			t.Fatalf("after consume %d, the Warns %v; want one, naming ghost", i+1, said)
		}
	}
	wantTold(t, "both consumes", told, "ghost", "ghost")
}

// A mark's tier reads the staging its door prepared before the round lock. A
// consume that lands where that is not this staging's, the learn step having
// ended or a staging landed since the door looked, cannot say what it learned:
// read as learning nothing, it would hide learning from the verdict; read
// against no base, it would take a test's verdict away for nothing. It is
// refused, with nothing told and nothing flagged, as a consume outside every
// test is while a staging lands, and the parser's next read goes through,
// against the staging of that moment.
func TestAMarkTheStagingMovedUnderIsRefusedAndReadAgain(t *testing.T) {
	moves := map[string]struct {
		before func(mm *MockManager)
		move   func(mm *MockManager, m *models.Mock)
	}{
		"the learn step ended": {
			before: func(*MockManager) {},
			move:   func(mm *MockManager, _ *models.Mock) { mm.SetLearnEnded(true) },
		},
		"a staging landed": {
			before: func(mm *MockManager) { mm.SetLearnEnded(true) },
			move: func(mm *MockManager, m *models.Mock) {
				mm.SetMocksWithWindow(nil, []*models.Mock{m}, models.BaseTime, time.Now())
			},
		},
	}
	for name, tc := range moves {
		t.Run(name, func(t *testing.T) {
			provenswitch.Pin(true)
			t.Cleanup(provenswitch.Unpin)
			core, logs := observer.New(zapcore.WarnLevel)
			m := exactMock("session", 0, models.LifetimeSession)
			mm := stagedManagerLogged(t, zap.New(core), nil, []*models.Mock{m})
			mm.ServeExactPool("k", []string{"session"})
			tc.before(mm)
			told := toldNames(mm)
			// A copy built by name, of no staging, that learned a noise path
			// the recording lacks.
			c := *m
			c.TestModeInfo.StagedSet = 0
			c.Spec.ReqBodyNoise = map[string][]string{"body.at": {}}
			var once sync.Once
			hook := func(string) { once.Do(func() { tc.move(mm, m) }) }
			mm.exact.round.onConsume.Store(&hook)
			if mm.MarkMockAsUsed(c) {
				t.Fatal("the mark went through on a staging its door did not read")
			}
			wantTold(t, "the refused mark", told)
			if st := mm.GetConsumedMocks(); len(st) != 0 {
				t.Fatalf("the refused mark was flagged: %+v", st)
			}
			if n := mm.SessionMockHitCounts()["session"]; n != 0 {
				t.Fatalf("the refused mark was counted as %d match(es), want 0", n)
			}
			if !mm.MarkMockAsUsed(c) {
				t.Fatal("the mark read again did not go through")
			}
			wantTold(t, "the mark read again", told, "session")
			if n := mm.SessionMockHitCounts()["session"]; n != 1 {
				t.Fatalf("the mark read again was counted as %d match(es), want 1", n)
			}
			st := mm.GetConsumedMocks()
			if len(st) != 1 || !st[0].LearnedAfterEnd {
				t.Fatalf("the mark read again: %+v; want it to say it learned after the end", st)
			}
			if said := unstagedWarns(logs); len(said) != 0 {
				t.Fatalf("the staging holds the recording, and a Warn says it does not: %v", said[0].ContextMap())
			}
		})
	}
}

// A parser's goroutine recovers a panic and loses that connection. A tier runs
// holding the round lock, so a panic in one must let it go: a snapshot and
// another door still go through. Held past the recover, every consume,
// snapshot and boundary after it waits for good, with no error. The door's
// nil copy stands for any parser bug; the panic stays the parser's.
func TestAPanicInATierLeavesTheStoreServing(t *testing.T) {
	m := exactMock("session", 0, models.LifetimeSession)
	other := exactMock("other", time.Second, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{m, other})
	mm.ServeExactPool("k", []string{"session", "other"})
	told := toldNames(mm)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("precondition: the tier did not panic")
			}
		}()
		mm.UpdateUnFilteredMock(m, nil)
	}()
	done := make(chan bool, 1)
	go func() {
		mm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP})
		done <- mm.MarkMockAsUsed(*other)
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the door after the panic did not go through")
		}
	case <-time.After(time.Minute):
		mm.exact.round.mu.Unlock() // lets the goroutine and the cleanup go
		<-done
		t.Fatal("the store is wedged: the round lock outlived the panic")
	}
	wantTold(t, "the panic told nothing, the door after it one serve", told, "other")
}

// The served observer runs holding the round lock too, and it is the learn
// tracker's code. One that panics (recovered with the parser's connection)
// must let the lock go as a tier's panic does: a snapshot and the next mark
// go through, and the observer is told of that one. At main the lock outlived
// the observer's panic and wedged the store.
func TestAPanickingServedObserverLeavesTheStoreServing(t *testing.T) {
	m := exactMock("session", 0, models.LifetimeSession)
	other := exactMock("other", time.Second, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{m, other})
	mm.ServeExactPool("k", []string{"session", "other"})
	var panicked atomic.Bool
	var told atomic.Int64
	mm.SetServedObserver(func(*models.Mock) {
		if panicked.CompareAndSwap(false, true) {
			panic("observer bug")
		}
		told.Add(1)
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("precondition: the observer did not panic")
			}
		}()
		mm.MarkMockAsUsed(*m)
	}()
	done := make(chan bool, 1)
	go func() {
		mm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP})
		done <- mm.MarkMockAsUsed(*other)
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the mark after the panic did not go through")
		}
	case <-time.After(time.Minute):
		mm.exact.round.mu.Unlock() // lets the goroutine and the cleanup go
		<-done
		t.Fatal("the store is wedged: the round lock outlived the observer's panic")
	}
	if n := told.Load(); n != 1 {
		t.Fatalf("told %d serve(s) after the panic, want 1", n)
	}
}

// A mark the view passes while the learn step runs (record's first cycle:
// every parser that consumes a reusable recording through MarkMockAsUsed)
// goes through, told once, and says it learned nothing after the end,
// whatever its copy learned: nothing has ended.
func TestAMarkWhileTheLearnStepRunsGoesThrough(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	m := exactMock("session", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{m})
	mm.ServeExactPool("k", []string{"session"})
	told := toldNames(mm)
	c := *m
	c.TestModeInfo.StagedSet = 0
	c.Spec.ReqBodyNoise = map[string][]string{"body.at": {}}
	if !mm.MarkMockAsUsed(c) {
		t.Fatal("a mark the view passes was refused while the learn step runs")
	}
	wantTold(t, "the mark", told, "session")
	if st := mm.GetConsumedMocks(); len(st) != 1 || st[0].LearnedAfterEnd {
		t.Fatalf("the mark: %+v; want one, that learned nothing after the end", st)
	}
}

// Once the staging's outside answers are built, a mark after the learn step
// ended takes no lock before its round step, neither swapMu nor the outside
// answers' own: so it waits behind no staging until that staging starts its
// epoch (one waiting for swapMu, or holding it before its epoch). In a retry
// cycle a parser marks every reusable recording it answers from, one consume
// at a time. A door after a staging started its epoch waits for it to land,
// as there is nothing to read before it does; no staging here gets that far.
func TestAMarkAfterTheLearnStepEndedTakesNoLockBeforeAStagingStartsItsEpoch(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	m := exactMock("session", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{m})
	mm.ServeExactPool("k", []string{"session"})
	mm.SetLearnEnded(true)
	told := toldNames(mm)
	if !mm.MarkMockAsUsed(*m) {
		t.Fatal("precondition: the first mark did not go through")
	}
	mm.swapMu.Lock()
	mm.exact.outside.mu.Lock()
	unlock := func() { mm.exact.outside.mu.Unlock(); mm.swapMu.Unlock() }
	done := make(chan bool, 1)
	go func() { done <- mm.MarkMockAsUsed(*m) }()
	select {
	case ok := <-done:
		unlock()
		if !ok {
			t.Fatal("the mark, its staging's outside answers built, was refused")
		}
	case <-time.After(time.Minute):
		unlock()
		<-done
		t.Fatal("the mark waited for the staging lock or the outside answers' lock with its staging's outside answers built")
	}
	wantTold(t, "both marks", told, "session", "session")
}

// A staging lets the past staging's outside answers go before it starts its
// epoch, and they are built only under swapMu: so at every instant they are
// none or the current staging's, and a read that takes no lock never sees a
// past staging's answers under the new epoch.
func TestAStagingLetsThePastOutsideAnswersGoBeforeItStartsItsEpoch(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	m := exactMock("session", 0, models.LifetimeSession)
	mm := stagedManager(t, nil, []*models.Mock{m})
	mm.SetLearnEnded(true)
	mm.prepareOutside()
	if mm.outsideNow() == nil {
		t.Fatal("precondition: the outside answers were not built")
	}
	seen := false
	mm.exact.outside.onLetGo = func() {
		mm.exact.outside.onLetGo = nil
		seen = true
		if st := mm.exact.outside.cur.Load(); st != nil && st.epoch != mm.stagingEpoch.Load() {
			t.Errorf("as the staging started its epoch, the outside answers held staging %d's under epoch %d", st.epoch, mm.stagingEpoch.Load())
		}
	}
	mm.SetMocksWithWindow(nil, []*models.Mock{m}, models.BaseTime, time.Now())
	if !seen {
		t.Fatal("the staging started no epoch")
	}
	if st := mm.exact.outside.cur.Load(); st != nil && st.epoch != mm.stagingEpoch.Load() {
		t.Fatalf("after the staging, the outside answers held staging %d's under epoch %d", st.epoch, mm.stagingEpoch.Load())
	}
}

// A named read (a delete door's spend: noteExactSpent) builds the staging's
// outside answers, then reads them. A staging that lands between the two
// leaves no base to read the copy against: every noise path it carries is
// read as learned after the end (fail closed: its tests get no verdict), and
// a Warn naming the recording says a staging began under the read. It does
// not say the staging lacks the recording: the new staging holds it.
func TestANamedReadAStagingLandedUnderIsReadAsLearnedAndSaid(t *testing.T) {
	provenswitch.Pin(true)
	t.Cleanup(provenswitch.Unpin)
	core, logs := observer.New(zapcore.WarnLevel)
	m := exactMock("session", 0, models.LifetimeSession)
	m.Spec.ReqBodyNoise = map[string][]string{"body.at": {}}
	mm := stagedManagerLogged(t, zap.New(core), nil, []*models.Mock{m})
	mm.SetLearnEnded(true)
	c := *m
	if mm.learnedAfterEndNamed(&c) {
		t.Fatal("precondition: a copy carrying only the recording's noise was read as learned beyond it")
	}
	mm.exact.outside.onPrepared = func() {
		mm.exact.outside.onPrepared = nil
		mm.SetMocksWithWindow(nil, []*models.Mock{m}, models.BaseTime, time.Now())
	}
	if !mm.learnedAfterEndNamed(&c) {
		t.Fatal("read with no staging's base, the copy's noise path was read as learned before the end")
	}
	if said := begunUnderWarns(logs); len(said) != 1 || said[0].ContextMap()["mock"] != "session" {
		t.Fatalf("the Warns that a staging began under the read %v; want one, naming session", said)
	}
	if said := unstagedWarns(logs); len(said) != 0 {
		t.Fatalf("the Warns %v say the staging does not hold session; it does, a staging began under the read", said)
	}
}

// begunUnderWarns is the Warns that a staging began while keploy read a
// consumed recording's staged base (warnStagingBegunUnder).
func begunUnderWarns(logs *observer.ObservedLogs) []observer.LoggedEntry {
	return logs.FilterMessageSnippet("a new staging began while keploy read the staged recording").All()
}

// DeleteFilteredMock's fall to the start-up tier calls this door saying not to
// tell: it tells of the serve itself. A view installed between the two settles
// the inner consume, which must stay silent there too, or one serve is told
// twice.
func TestADoorWhoseCallerTellsTellsNothingUnderAView(t *testing.T) {
	for _, tc := range []struct {
		observe bool
		want    []string
	}{{false, nil}, {true, []string{"boot"}}} {
		m := doorMock("DeleteStartupMock", "boot", 0, models.HTTP)
		mm := stagedForDoor(t, "DeleteStartupMock", m)
		mm.ServeExactPool("k", []string{"boot"})
		told := toldNames(mm)
		if !mm.deleteStartupMock(*m, nil, tc.observe) {
			t.Fatal("precondition: the view did not serve the consume")
		}
		wantTold(t, "observe "+strconv.FormatBool(tc.observe), told, tc.want...)
	}
}
