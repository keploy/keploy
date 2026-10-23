package proxy

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// roundStore is a set staged under exact serving with S12's background
// rules, as record's full round serves it: T1's own (own, own2), T2's
// (next, in flight when it began, and next2), T3's (other: a pairing
// candidate), the pool (pool) and a worker-scope recording (w-boot), and the
// learn tracker the agent's proxy routes through the manager.
type roundStore struct {
	mm *MockManager
	tr *learn.Tracker
	ms map[string]*models.Mock
}

func newRoundStore(t *testing.T) *roundStore {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	ms := map[string]*models.Mock{}
	var all []*models.Mock
	for i, name := range []string{"own", "own2", "next", "next2", "other", "pool", "w-boot"} {
		m := httpCall(name, time.Duration(i)*time.Second, "/"+name)
		ms[name] = m
		all = append(all, m)
	}
	// A handshake: protocol-phase, shown whatever the view.
	ms["hs"] = configMock("hs", 8*time.Second)
	all = append(all, ms["hs"])
	mm := stagedManager(t, nil, all)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot"})
	mm.ServeExactWorker([]string{"w-boot"})
	mm.SetPairing(true)
	tr := learn.NewTracker()
	mm.SetServedObserver(tr.ServedMock)
	return &roundStore{mm: mm, tr: tr, ms: ms}
}

func (s *roundStore) begin() { s.mm.LearnBoundary(s.tr, func(t *learn.Tracker) { t.Begin() }) }
func (s *roundStore) end()   { s.mm.LearnBoundary(s.tr, func(t *learn.Tracker) { t.End() }) }

// inTest opens T1 on the tracker and the store, as the replay does.
func (s *roundStore) inTest() {
	s.begin()
	s.mm.ServeExactTest("T1", []*models.Mock{s.ms["own"], s.ms["own2"]})
}

// betweenWithAhead ends T1 as the replay does (the store's pool view first,
// its ahead T2's call in flight, then the tracker), and leaves the round
// between tests.
func (s *roundStore) betweenWithAhead() {
	s.inTest()
	s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"}, s.ms["next"])
	s.end()
}

// poolSpent serves the pool view with no test begun, and a call outside
// every test spends the pool's recording for the staging.
func (s *roundStore) poolSpent() {
	s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	if !s.mm.DeleteFilteredMock(*s.ms["pool"]) {
		panic("precondition: the pool's recording was not spent outside every test")
	}
}

func (s *roundStore) snapshot(t *testing.T) integrations.RoundSnapshot {
	t.Helper()
	snap, ok := s.mm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP, Tracker: s.tr, Pairing: true})
	if !ok {
		t.Fatal("no snapshot under exact serving")
	}
	return snap
}

// roundMock is the staged recording named name.
func roundMock(mm *MockManager, name string) *models.Mock {
	for _, mk := range mm.StagedMocks() {
		if mk.Name == name {
			return mk
		}
	}
	return nil
}

// shown is the view's copy of name in snap's per-test or session read.
func shown(snap integrations.RoundSnapshot, name string) *models.Mock {
	for _, list := range [][]*models.Mock{snap.PerTest, snap.Session} {
		for _, m := range list {
			if m.Name == name {
				return m
			}
		}
	}
	return nil
}

// roundSummary is every part of a snapshot, by name, for comparing two.
func roundSummary(s integrations.RoundSnapshot) string {
	names := func(ms []*models.Mock) string { return strings.Join(exactNames(ms), ",") }
	var takes []string
	for _, tk := range s.Taken {
		takes = append(takes, fmt.Sprintf("%s:%d:%s:%d:%v:%d", tk.Mock.Name, tk.How, tk.Test, tk.Gen, tk.Ahead, tk.Of))
	}
	var owners []string
	for _, m := range append(append([]*models.Mock(nil), s.PerTest...), s.Session...) {
		owners = append(owners, fmt.Sprintf("%s=%d", m.Name, s.Owner(m)))
	}
	sort.Strings(owners)
	var earlier []string
	for _, e := range s.Learn.Earlier {
		earlier = append(earlier, e.Mock)
	}
	return fmt.Sprintf("at=%+v test=%q ahead=%v fired=%v phase=%d boundary=%q open=%v bg=%v\nper-test=%s\nsession=%s\nstartup=%s\nscoped=%s\nconsumed=%s\ntaken=%v\nlearn=%v/%d/%v/%v earlier=%v served=%s before=%s answered=%s\nbg=%s|%s\ncands=%s ondisk=%d\nowners=%v",
		s.At, s.Test, s.Ahead, s.Fired, s.Phase, s.Boundary, s.Open, s.Background,
		names(s.PerTest), names(s.Session), names(s.Startup), names(s.SessionScoped), names(s.Consumed), takes,
		s.Learn.Bound, s.Learn.Number, s.Learn.Open, s.Learn.Begun, earlier, names(s.Learn.Served), names(s.Learn.Before), names(s.Learn.Answered),
		names(s.BackgroundUnused), names(s.BackgroundSpent), names(s.Candidates), len(s.OnDisk), owners)
}

// roundReads are the reads a snapshot makes, in order (RoundSnapshot's
// onRead steps).
var roundReads = []string{"view", "learn", "per-test", "session", "consumed", "background", "candidates"}

// landInside takes a snapshot during which change runs, landing after the
// read named at: change starts there, and the snapshot goes on once change
// is about to take the round lock (onWait) or is done. It returns the
// snapshot, and waits for change to end.
func (s *roundStore) landInside(t *testing.T, at string, change func()) integrations.RoundSnapshot {
	t.Helper()
	r := &s.mm.exact.round
	waiting := make(chan struct{}, 1)
	done := make(chan struct{})
	r.onWait = func() {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}
	started := false
	r.onRead = func(step string) {
		if step != at || started {
			return
		}
		started = true
		go func() {
			defer close(done)
			change()
		}()
		select {
		case <-waiting:
		case <-done:
		}
	}
	snap := s.snapshot(t)
	r.onRead = nil
	if !started {
		t.Fatalf("the snapshot made no read %q", at)
	}
	<-done
	r.onWait = nil
	return snap
}

// A change of the round that lands between any two of a snapshot's reads is
// in none of them: the snapshot is the one read before the change, part for
// part, and the next shows all of it. Each mover the round has: a consume
// (the observer's account of it in the same step), a spend by kind, a
// pairing's claim, a view swap (pool to pool with the same names and with
// other names, pool to a test's, a test's to pool), and the learn tracker's
// begin and end.
func TestARoundChangeLandsInNoSnapshotItRacesWith(t *testing.T) {
	type mover struct {
		name  string
		start func(s *roundStore)
		// change is the move; check what the snapshot after it must show.
		change func(t *testing.T, s *roundStore, before integrations.RoundSnapshot)
		check  func(t *testing.T, s *roundStore, before, after integrations.RoundSnapshot)
	}
	sameGen := func(t *testing.T, before, after integrations.RoundSnapshot) {
		t.Helper()
		if after.At.Gen != before.At.Gen || after.At.Taken != before.At.Taken+1 {
			t.Fatalf("a take moved the round to %+v from %+v; want the same generation and one take more", after.At, before.At)
		}
	}
	movedGen := func(t *testing.T, before, after integrations.RoundSnapshot) {
		t.Helper()
		if after.At.Gen == before.At.Gen {
			t.Fatalf("the round's generation did not move: %+v", after.At)
		}
	}
	lastTake := func(t *testing.T, after integrations.RoundSnapshot, name string, how integrations.TakeHow) {
		t.Helper()
		if len(after.Taken) == 0 {
			t.Fatal("no take logged")
		}
		tk := after.Taken[len(after.Taken)-1]
		if tk.Mock.Name != name || tk.How != how {
			t.Fatalf("the last take is %s/%d; want %s/%d", tk.Mock.Name, tk.How, name, how)
		}
	}
	movers := []mover{
		{"a consume", (*roundStore).inTest,
			func(t *testing.T, s *roundStore, before integrations.RoundSnapshot) {
				if !s.mm.DeleteFilteredMock(*shown(before, "own")) {
					t.Error("the consume was refused")
				}
			},
			func(t *testing.T, s *roundStore, before, after integrations.RoundSnapshot) {
				sameGen(t, before, after)
				lastTake(t, after, "own", integrations.TakeConsumed)
				if shown(after, "own") != nil || strings.Join(exactNames(after.Consumed), ",") != "own" || !hasName(after.Learn.Served, "own") {
					t.Fatalf("after the consume: own shown %v, consumed %v, the tracker's served %v", shown(after, "own") != nil, exactNames(after.Consumed), exactNames(after.Learn.Served))
				}
			}},
		{"a spend by kind", (*roundStore).inTest,
			func(t *testing.T, s *roundStore, before integrations.RoundSnapshot) {
				if !s.mm.SpendByKind(s.ms["pool"]) {
					t.Error("the spend was refused")
				}
			},
			func(t *testing.T, s *roundStore, before, after integrations.RoundSnapshot) {
				sameGen(t, before, after)
				lastTake(t, after, "pool", integrations.TakeSpent)
				if !hasName(after.BackgroundSpent, "pool") || hasName(after.BackgroundUnused, "pool") {
					t.Fatalf("after the spend the background is %v unused, %v spent", exactNames(after.BackgroundUnused), exactNames(after.BackgroundSpent))
				}
			}},
		{"a claim", (*roundStore).inTest,
			func(t *testing.T, s *roundStore, before integrations.RoundSnapshot) {
				if !s.mm.ClaimPairing(s.ms["other"]) {
					t.Error("the claim was refused")
				}
			},
			func(t *testing.T, s *roundStore, before, after integrations.RoundSnapshot) {
				sameGen(t, before, after)
				lastTake(t, after, "other", integrations.TakeClaimed)
				if !hasName(before.Candidates, "other") || hasName(after.Candidates, "other") {
					t.Fatalf("other is a candidate before %v, after %v", exactNames(before.Candidates), exactNames(after.Candidates))
				}
			}},
		{"a pool view after a pool view, the same names", (*roundStore).betweenWithAhead,
			func(t *testing.T, s *roundStore, before integrations.RoundSnapshot) {
				s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"}, s.ms["next"])
			},
			func(t *testing.T, s *roundStore, before, after integrations.RoundSnapshot) {
				movedGen(t, before, after)
				if strings.Join(exactNames(after.PerTest), ",") != strings.Join(exactNames(before.PerTest), ",") {
					t.Fatalf("the views show %v and %v; the row wants the same names", exactNames(before.PerTest), exactNames(after.PerTest))
				}
			}},
		{"a pool view after a pool view, other names", (*roundStore).betweenWithAhead,
			func(t *testing.T, s *roundStore, before integrations.RoundSnapshot) {
				s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"}, s.ms["next2"])
			},
			func(t *testing.T, s *roundStore, before, after integrations.RoundSnapshot) {
				movedGen(t, before, after)
				if !hasName(after.PerTest, "next2") || hasName(after.PerTest, "next") {
					t.Fatalf("the next pool view shows %v", exactNames(after.PerTest))
				}
			}},
		{"a test's view after the pool's", func(s *roundStore) { s.betweenWithAhead(); s.begin() },
			func(t *testing.T, s *roundStore, before integrations.RoundSnapshot) {
				s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"], s.ms["next2"]})
			},
			func(t *testing.T, s *roundStore, before, after integrations.RoundSnapshot) {
				movedGen(t, before, after)
				if before.Phase != integrations.RoundBoundary || after.Phase != integrations.RoundInTest || after.Test != "T2" {
					t.Fatalf("the phase went %d to %d (test %q); want a boundary, then in T2", before.Phase, after.Phase, after.Test)
				}
			}},
		{"the pool's view after a test's", (*roundStore).inTest,
			func(t *testing.T, s *roundStore, before integrations.RoundSnapshot) {
				s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"}, s.ms["next"])
			},
			func(t *testing.T, s *roundStore, before, after integrations.RoundSnapshot) {
				movedGen(t, before, after)
				if after.Phase != integrations.RoundBoundary || after.Open || !strings.Contains(after.Boundary, "ended T1") {
					t.Fatalf("the store ended T1 before the tracker: phase %d open %v (%s)", after.Phase, after.Open, after.Boundary)
				}
			}},
		{"the learn tracker's begin", (*roundStore).betweenWithAhead,
			func(t *testing.T, s *roundStore, before integrations.RoundSnapshot) { s.begin() },
			func(t *testing.T, s *roundStore, before, after integrations.RoundSnapshot) {
				movedGen(t, before, after)
				if after.Learn.Number == before.Learn.Number || !after.Learn.Open || after.Phase != integrations.RoundBoundary {
					t.Fatalf("after the begin the tracker reads %d open %v, phase %d", after.Learn.Number, after.Learn.Open, after.Phase)
				}
			}},
		{"the learn tracker's end", (*roundStore).inTest,
			func(t *testing.T, s *roundStore, before integrations.RoundSnapshot) { s.end() },
			func(t *testing.T, s *roundStore, before, after integrations.RoundSnapshot) {
				movedGen(t, before, after)
				if after.Learn.Open || after.Phase != integrations.RoundBoundary || after.Open {
					t.Fatalf("after the end the tracker reads open %v, phase %d", after.Learn.Open, after.Phase)
				}
			}},
		{"a test process claiming its key again", (*roundStore).poolSpent,
			func(t *testing.T, s *roundStore, before integrations.RoundSnapshot) {
				s.mm.RestartExactKey(models.PoolKeyNone, []string{"pool"})
			},
			func(t *testing.T, s *roundStore, before, after integrations.RoundSnapshot) {
				movedGen(t, before, after)
				lastTake(t, after, "pool", integrations.TakeRestored)
				if shown(after, "pool") == nil || after.AnsweredInRound("pool") {
					t.Fatalf("after the restart the pool is shown %v, answered in the round %v", shown(after, "pool") != nil, after.AnsweredInRound("pool"))
				}
			}},
	}
	for _, mv := range movers {
		for _, at := range roundReads {
			t.Run(mv.name+" after the "+at+" read", func(t *testing.T) {
				s := newRoundStore(t)
				mv.start(s)
				before := s.snapshot(t)
				during := s.landInside(t, at, func() { mv.change(t, s, before) })
				if got, want := roundSummary(during), roundSummary(before); got != want {
					t.Fatalf("the snapshot the change raced with is not the one before it:\n%s\nwant\n%s", got, want)
				}
				after := s.snapshot(t)
				mv.check(t, s, before, after)
			})
		}
	}
}

func hasName(ms []*models.Mock, name string) bool {
	for _, m := range ms {
		if m.Name == name {
			return true
		}
	}
	return false
}

// A take checked against a snapshot (WithinRound, ClaimPairingIn,
// SpendByKindIn) takes while nothing it was decided on moved, another
// call's take included, and is refused, saying what moved, once the
// staging, the view or the learn tracker's test did.
func TestATakeCheckedAgainstASnapshotIsRefusedOnceTheRoundMoved(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(s *roundStore)
		want string // "" for no move
	}{
		{"nothing moved", func(*roundStore) {}, ""},
		{"another call took a recording", func(s *roundStore) { s.mm.ClaimPairing(s.ms["next"]) }, ""},
		{"a view was installed", func(s *roundStore) {
			s.mm.ServeExactTest("T1", []*models.Mock{s.ms["own"], s.ms["own2"]})
		}, "a view was installed"},
		{"the learn tracker ended its test", func(s *roundStore) { s.end() }, "the learn tracker ended its test"},
		{"the learn tracker began a test", func(s *roundStore) { s.end(); s.begin() }, "the learn tracker began a test"},
		{"the set was staged again", func(s *roundStore) {
			s.mm.SetMocksWithWindow(nil, []*models.Mock{s.ms["own"], s.ms["other"], s.ms["pool"]}, models.BaseTime, time.Now())
		}, "the staging moved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRoundStore(t)
			s.inTest()
			snap := s.snapshot(t)
			tc.move(s)
			scope := s.mm.WithinRound(snap.At)
			consumed := scope.DeleteFilteredMock(*shown(snap, "own"))
			claimed, claimMoved, claimWhy := s.mm.ClaimPairingIn(s.ms["other"], snap.At)
			spent, spendMoved, spendWhy := s.mm.SpendByKindIn(s.ms["pool"], snap.At)
			if tc.want == "" {
				if !consumed || !claimed || !spent || scope.Moved() != "" || claimMoved || spendMoved {
					t.Fatalf("nothing the snapshot read moved, yet: consumed %v (%s), claimed %v (%s), spent %v (%s)", consumed, scope.Moved(), claimed, claimWhy, spent, spendWhy)
				}
				return
			}
			if consumed || claimed || spent {
				t.Fatalf("a take landed after %s: consumed %v, claimed %v, spent %v", tc.name, consumed, claimed, spent)
			}
			if _, _, served, before := s.tr.Test(); hasName(served, "own") || hasName(before, "own") {
				t.Errorf("a consume refused for a move told the served observer of own: served %v, before %v", exactNames(served), exactNames(before))
			}
			for what, why := range map[string]string{"consume": scope.Moved(), "claim": claimWhy, "spend": spendWhy} {
				if !strings.Contains(why, tc.want) {
					t.Errorf("the %s was refused for %q; want it to say %q", what, why, tc.want)
				}
			}
			if !claimMoved || !spendMoved {
				t.Errorf("claim moved %v, spend moved %v; want both", claimMoved, spendMoved)
			}
		})
	}
}

// A take checked against a snapshot that finds its view closing (an install
// under way: the view closed, its replacement not in force yet) waits for the
// install, and is then refused for it: the round moved. Refused at once, its
// caller read the round again while the closing view still stood, took, was
// refused and read again, until the install landed. A consume refused tells
// the served observer nothing.
func TestATakeCheckedAgainstAClosingViewWaitsForTheInstallAndIsRefusedForIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		take func(s *roundStore, snap integrations.RoundSnapshot) (bool, string)
	}{
		{"a consume", func(s *roundStore, snap integrations.RoundSnapshot) (bool, string) {
			scope := s.mm.WithinRound(snap.At)
			return scope.DeleteFilteredMock(*shown(snap, "next")), scope.Moved()
		}},
		{"a claim", func(s *roundStore, snap integrations.RoundSnapshot) (bool, string) {
			claimed, _, why := s.mm.ClaimPairingIn(s.ms["other"], snap.At)
			return claimed, why
		}},
		{"a spend", func(s *roundStore, snap integrations.RoundSnapshot) (bool, string) {
			spent, _, why := s.mm.SpendByKindIn(s.ms["pool"], snap.At)
			return spent, why
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRoundStore(t)
			s.betweenWithAhead()
			s.begin()
			snap := s.snapshot(t)
			var took, waited bool
			var why string
			// The take runs beside the install; the install goes on once the
			// take waits for the view that replaces the closed one, or
			// returns.
			done, waits := make(chan struct{}), make(chan struct{}, 1)
			s.mm.exact.onClosedWait = func() {
				select {
				case waits <- struct{}{}:
				default:
				}
			}
			s.mm.exact.beforeInstall = func() {
				s.mm.exact.beforeInstall = nil
				go func() {
					defer close(done)
					took, why = tc.take(s, snap)
				}()
				select {
				case <-done:
				case <-waits:
					waited = true
				}
			}
			s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"], s.ms["next2"]})
			<-done
			s.mm.exact.onClosedWait = nil
			if !waited || took || !strings.Contains(why, "a view was installed") {
				t.Fatalf("%s of the closing view: waited for the install %v, took %v, why %q; want it to wait, then be refused for the install", tc.name, waited, took, why)
			}
			if _, _, served, before := s.tr.Test(); hasName(served, "next") || hasName(before, "next") {
				t.Errorf("a consume refused told the served observer of next: served %v, before %v", exactNames(served), exactNames(before))
			}
		})
	}
}

// A take checked against a round where none was read (exact serving off) is
// refused, through every door, and tells the served observer nothing.
func TestATakeCheckedAgainstNoRoundIsRefused(t *testing.T) {
	s := newRoundStore(t)
	if _, ok := s.mm.RoundSnapshot(integrations.RoundQuery{Tracker: s.tr}); ok {
		t.Fatal("precondition: a snapshot with exact serving off")
	}
	mk := *s.ms["own"]
	for door, take := range map[string]func(scope integrations.RoundScope) bool{
		"DeleteFilteredMock":   func(scope integrations.RoundScope) bool { return scope.DeleteFilteredMock(mk) },
		"DeleteUnFilteredMock": func(scope integrations.RoundScope) bool { return scope.DeleteUnFilteredMock(mk) },
		"DeleteStartupMock":    func(scope integrations.RoundScope) bool { return scope.DeleteStartupMock(mk) },
		"MarkMockAsUsed":       func(scope integrations.RoundScope) bool { return scope.MarkMockAsUsed(mk) },
		"UpdateUnFilteredMock": func(scope integrations.RoundScope) bool { return scope.UpdateUnFilteredMock(&mk, &mk) },
	} {
		scope := s.mm.WithinRound(integrations.RoundAt{})
		if take(scope) || !strings.Contains(scope.Moved(), "exact serving is off") {
			t.Errorf("%s with no round read: took, or refused for %q", door, scope.Moved())
		}
	}
	if claimed, moved, why := s.mm.ClaimPairingIn(s.ms["other"], integrations.RoundAt{}); claimed || !moved {
		t.Errorf("a claim with no round read: claimed %v moved %v (%s)", claimed, moved, why)
	}
	if spent, _, why := s.mm.SpendByKindIn(s.ms["pool"], integrations.RoundAt{}); spent || !strings.Contains(why, "exact serving is off") {
		t.Errorf("a spend with no round read: spent %v (%s)", spent, why)
	}
	if _, _, served, before := s.tr.Test(); len(served)+len(before) > 0 {
		t.Errorf("a take refused told the served observer of %v %v", exactNames(served), exactNames(before))
	}
}

// No staging lands while a snapshot reads: a staging takes swapMu to write,
// which every snapshot holds to read, so the trees, the window and the
// staging's epoch a snapshot reads are of one staging.
func TestNoStagingLandsWhileASnapshotReads(t *testing.T) {
	s := newRoundStore(t)
	s.inTest()
	steps := 0
	s.mm.exact.round.onRead = func(step string) {
		steps++
		if s.mm.swapMu.TryLock() {
			s.mm.swapMu.Unlock()
			t.Errorf("a staging could land after the snapshot's %s read", step)
		}
	}
	s.snapshot(t)
	s.mm.exact.round.onRead = nil
	if steps != len(roundReads) {
		t.Fatalf("the snapshot made %d reads; want %d", steps, len(roundReads))
	}
}

// A test process that claims its pool slice's key again (a retried package)
// is a new scope: what calls outside every test spent of its slice answers
// again, in one step of the round. The take log gives each spend back
// (TakeRestored, naming its spend), the generation moves, and a take
// decided before it is refused; the recording answers once more, and is
// spent again. Given back outside the round lock and with no entry, a
// snapshot read the recording shown and spent both, and a take decided on
// the spent round landed in the restarted one.
func TestARestartedKeyGivesItsSpendsBackInOneStepOfTheRound(t *testing.T) {
	s := newRoundStore(t)
	s.poolSpent()
	spent := s.snapshot(t)
	at := len(spent.Taken) - 1
	if shown(spent, "pool") != nil || !spent.AnsweredInRound("pool") || spent.Taken[at].How != integrations.TakeSpent {
		t.Fatalf("precondition: the pool's recording is not spent: %s", roundSummary(spent))
	}
	s.mm.RestartExactKey(models.PoolKeyNone, []string{"pool"})
	after := s.snapshot(t)
	last := after.Taken[len(after.Taken)-1]
	if last.How != integrations.TakeRestored || last.Mock.Name != "pool" || last.Of != at || last.Gen != after.At.Gen || after.At.Gen == spent.At.Gen {
		t.Fatalf("the restart wrote %+v at %+v; want the spend at %d given back, the round moved", last, after.At, at)
	}
	if shown(after, "pool") == nil || after.AnsweredInRound("pool") || !hasName(after.BackgroundUnused, "pool") {
		t.Fatalf("after the restart the pool is shown %v, unused %v, answered in the round %v", shown(after, "pool") != nil, hasName(after.BackgroundUnused, "pool"), after.AnsweredInRound("pool"))
	}
	scope := s.mm.WithinRound(spent.At)
	if scope.DeleteFilteredMock(*shown(after, "pool")) || !strings.Contains(scope.Moved(), "claimed its key again") {
		t.Fatalf("a take decided before the restart: refused for %q", scope.Moved())
	}
	if !s.mm.DeleteFilteredMock(*shown(after, "pool")) {
		t.Fatal("the restarted pool's recording does not answer again")
	}
	again := s.snapshot(t)
	if shown(again, "pool") != nil || !again.AnsweredInRound("pool") || again.Taken[len(again.Taken)-1].How != integrations.TakeSpent {
		t.Fatalf("spent again: %s", roundSummary(again))
	}
	// A key restarted with nothing spent gives nothing back.
	s.mm.RestartExactKey(models.PoolKeyNone, []string{"w-boot"})
	if n := len(s.snapshot(t).Taken); n != len(again.Taken) {
		t.Fatalf("a restart of an unspent recording wrote %d entries", n-len(again.Taken))
	}
	// Nor does it give back a spend that holds for the view alone (a
	// protocol-phase recording a delete door evicted in a test): the view
	// still hides it.
	v := newRoundStore(t)
	v.inTest()
	if !v.mm.DeleteFilteredMock(*shown(v.snapshot(t), "hs")) {
		t.Fatal("precondition: the handshake was not evicted")
	}
	evicted := v.snapshot(t)
	v.mm.RestartExactKey(models.PoolKeyNone, []string{"hs"})
	if after := v.snapshot(t); len(after.Taken) != len(evicted.Taken) || shown(after, "hs") != nil || !after.AnsweredInRound("hs") {
		t.Fatalf("a restart gave back the view's eviction: %s", roundSummary(after))
	}
}

// A test's view is bound to the learn tracker's open test only when no other
// view is bound to it. After a test whose end never reached the tracker (its
// misses' read did not complete), the next test's view, installed before the
// tracker begins it (the scope flow), is not the stale test's: until the
// tracker begins it, the round is at a boundary, and from then on in the
// test. Bound to the stale test at its install, every snapshot of the next
// test read a boundary, and the learn step learned the pool's drift inside
// it.
func TestATestViewIsBoundOnlyToATestNoOtherViewIsBoundTo(t *testing.T) {
	phase := func(t *testing.T, s *roundStore, when string, want integrations.RoundPhase, test, why string) {
		t.Helper()
		snap := s.snapshot(t)
		if snap.Phase != want || snap.Test != test || !strings.Contains(snap.Boundary, why) || snap.Open != (want == integrations.RoundInTest) {
			t.Fatalf("%s: phase %d (%q), test %q, open %v; want %d (%q) in %q", when, snap.Phase, snap.Boundary, snap.Test, snap.Open, want, why, test)
		}
	}
	t.Run("the scope flow: a test's view, then the tracker's begin", func(t *testing.T) {
		s := newRoundStore(t)
		s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		s.mm.ServeExactTest("T1", []*models.Mock{s.ms["own"]})
		phase(t, s, "T1's view before the begin", integrations.RoundBoundary, "T1", "has no test open")
		s.begin()
		phase(t, s, "T1 begun", integrations.RoundInTest, "T1", "")
		// T1 ends on the store; its end never reaches the tracker.
		s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		phase(t, s, "T1 ended on the store alone", integrations.RoundBoundary, "", "ended T1")
		s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"]})
		phase(t, s, "T2's view, the tracker still in T1", integrations.RoundBoundary, "T2", "open test is T1's")
		s.begin()
		phase(t, s, "T2 begun", integrations.RoundInTest, "T2", "")
		// T2's view stays as the tracker ends T2 and begins the next test:
		// a view left over from the test the tracker ended.
		s.end()
		s.begin()
		phase(t, s, "the tracker began the next test, T2's view left over", integrations.RoundBoundary, "T2", "another test than T2")
	})
	t.Run("the replay flow: the tracker's begin, then a test's view", func(t *testing.T) {
		s := newRoundStore(t)
		s.inTest()
		s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		// T1's end never reaches the tracker.
		s.begin()
		phase(t, s, "the tracker began T2 first", integrations.RoundBoundary, "", "has not opened")
		s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"]})
		phase(t, s, "T2 opened", integrations.RoundInTest, "T2", "")
	})
}

// The take log: a pool view's ahead answer, carried into its test as it
// opens, or voided by a view that carries it into none, each in the same
// step as the view's install; one log per staging.
func TestTheTakeLogCarriesAndVoidsAheadAnswers(t *testing.T) {
	t.Run("carried", func(t *testing.T) {
		s := newRoundStore(t)
		s.betweenWithAhead()
		between := s.snapshot(t)
		if !s.mm.DeleteFilteredMock(*shown(between, "next")) {
			t.Fatal("the ahead answer was refused")
		}
		s.begin()
		s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"], s.ms["next2"]})
		snap := s.snapshot(t)
		n := len(snap.Taken)
		if n < 2 {
			t.Fatalf("taken %v", snap.Taken)
		}
		ahead, carried := snap.Taken[n-2], snap.Taken[n-1]
		if ahead.How != integrations.TakeConsumed || !ahead.Ahead || carried.How != integrations.TakeCarried || carried.Of != n-2 || carried.Test != "T2" || carried.Gen != snap.At.Gen {
			t.Fatalf("the log ends %+v, %+v", ahead, carried)
		}
		if got := exactNames(snap.TakenInView()); strings.Join(got, ",") != "next" || snap.Owner(s.ms["next"]) != integrations.OwnerInFlight || !snap.InFlightIntoOpenTest(s.ms["next"]) {
			t.Fatalf("T2's view answered %v; next is %d", got, snap.Owner(s.ms["next"]))
		}
	})
	t.Run("voided", func(t *testing.T) {
		s := newRoundStore(t)
		s.betweenWithAhead()
		between := s.snapshot(t)
		if !s.mm.DeleteFilteredMock(*shown(between, "next")) {
			t.Fatal("the ahead answer was refused")
		}
		// The test next was answered for does not open next.
		s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		snap := s.snapshot(t)
		last := snap.Taken[len(snap.Taken)-1]
		if last.How != integrations.TakeVoided || last.Mock.Name != "next" || strings.Join(exactNames(snap.Voided()), ",") != "next" || !snap.AnsweredInRound("next") {
			t.Fatalf("the log ends %+v; voided %v", last, exactNames(snap.Voided()))
		}
	})
	t.Run("kept by the next pool view", func(t *testing.T) {
		s := newRoundStore(t)
		s.betweenWithAhead()
		between := s.snapshot(t)
		if !s.mm.DeleteFilteredMock(*shown(between, "next")) {
			t.Fatal("the ahead answer was refused")
		}
		s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"}, s.ms["next"])
		if snap := s.snapshot(t); len(snap.Voided()) != 0 || strings.Join(exactNames(snap.Consumed), ",") != "next" {
			t.Fatalf("a pool view naming next again voided %v, holds %v answered", exactNames(snap.Voided()), exactNames(snap.Consumed))
		}
	})
	t.Run("one log per staging", func(t *testing.T) {
		s := newRoundStore(t)
		s.inTest()
		if !s.mm.ClaimPairing(s.ms["other"]) {
			t.Fatal("the claim was refused")
		}
		old := s.snapshot(t)
		if got := s.mm.TakenLog(old.At.Epoch, 0, old.At.Taken); len(got) != 1 || got[0].How != integrations.TakeClaimed {
			t.Fatalf("the staging's log is %+v", got)
		}
		s.mm.SetMocksWithWindow(nil, []*models.Mock{s.ms["own"], s.ms["other"]}, models.BaseTime, time.Now())
		s.mm.ServeExactPool(models.PoolKeyNone, nil)
		if got := s.mm.TakenLog(old.At.Epoch, 0, old.At.Taken); got != nil {
			t.Fatalf("a staging the store left still has a log: %+v", got)
		}
		if snap := s.snapshot(t); snap.At.Taken != 0 || snap.At.Epoch == old.At.Epoch || !hasName(snap.Candidates, "other") {
			t.Fatalf("the next staging starts at %+v, candidates %v", snap.At, exactNames(snap.Candidates))
		}
	})
}

// Where nothing moves, a snapshot's reads are the accessors': the view's
// per-test, session, start-up and session-scoped reads, its consumed, its
// background and its candidates, in each state the round passes through.
func TestASnapshotReadsWhatTheAccessorsRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   func(t *testing.T, s *roundStore)
	}{
		{"at the start-up", func(*testing.T, *roundStore) {}},
		{"in a test", func(_ *testing.T, s *roundStore) { s.inTest() }},
		{"in a test, one of its own answered", func(t *testing.T, s *roundStore) {
			s.inTest()
			if !s.mm.DeleteFilteredMock(*shown(s.snapshot(t), "own")) {
				t.Fatal("the consume was refused")
			}
		}},
		{"between tests, an ahead", func(_ *testing.T, s *roundStore) { s.betweenWithAhead() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRoundStore(t)
			s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
			tc.at(t, s)
			snap := s.snapshot(t)
			perTest, _ := s.mm.GetPerTestMocksInWindow()
			filtered, _ := s.mm.GetFilteredMocks()
			session, _ := s.mm.GetSessionMocks()
			startup, _ := s.mm.GetStartupMocks()
			scoped, _ := s.mm.GetSessionScopedMocks()
			unused, spent := s.mm.KindBackground(models.HTTP)
			for _, c := range []struct {
				what      string
				got, want []*models.Mock
			}{
				{"per-test", snap.PerTest, perTest},
				{"filtered", snap.PerTest, filtered},
				{"session", snap.Session, session},
				{"start-up", snap.Startup, startup},
				{"session-scoped", snap.SessionScoped, scoped},
				{"background unused", snap.BackgroundUnused, unused},
				{"background spent", snap.BackgroundSpent, spent},
				{"candidates", snap.Candidates, s.mm.PairingCandidates(models.HTTP)},
			} {
				if !reflect.DeepEqual(exactNames(c.got), exactNames(c.want)) {
					t.Errorf("%s: the snapshot reads %v, the accessor %v", c.what, exactNames(c.got), exactNames(c.want))
				}
			}
			if snap.Test != "" && !reflect.DeepEqual(exactNames(snap.Consumed), exactNames(s.mm.ConsumedTestMocks())) {
				t.Errorf("consumed: %v, the accessor %v", exactNames(snap.Consumed), exactNames(s.mm.ConsumedTestMocks()))
			}
			w := s.mm.WindowSnapshot()
			if snap.Fired != w.FirstTestFired || (snap.Test != "") != w.Active {
				t.Errorf("the snapshot's window %v/%v, the accessor's %+v", snap.Test, snap.Fired, w)
			}
			for _, m := range s.ms {
				if snap.Test != "" && snap.Open && (snap.OwnedByOpenTest(m) != s.mm.OwnedByOpenTest(m) || snap.InFlightIntoOpenTest(m) != s.mm.InFlightIntoOpenTest(m)) {
					t.Errorf("%s: owned %v in flight %v; the accessors %v %v", m.Name, snap.OwnedByOpenTest(m), snap.InFlightIntoOpenTest(m), s.mm.OwnedByOpenTest(m), s.mm.InFlightIntoOpenTest(m))
				}
			}
		})
	}
}

// Where the round is, read from the store's view and the learn tracker
// together: a test is open only where both say it is the same one.
func TestTheRoundsPhaseIsTheStoresViewAndTheTrackersTestTogether(t *testing.T) {
	s := newRoundStore(t)
	s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
	steps := []struct {
		name  string
		do    func()
		phase integrations.RoundPhase
		why   string
	}{
		{"the start-up", func() {}, integrations.RoundStartUp, ""},
		{"the tracker began T1, the store has not opened it", s.begin, integrations.RoundBoundary, "has not opened"},
		{"both opened T1", func() { s.mm.ServeExactTest("T1", []*models.Mock{s.ms["own"]}) }, integrations.RoundInTest, ""},
		{"the store ended T1, the tracker has not", func() { s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"}) }, integrations.RoundBoundary, "ended T1"},
		{"both ended T1", s.end, integrations.RoundBetween, ""},
		{"the store opened T2, the tracker has no test open", func() { s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"]}) }, integrations.RoundBoundary, "has no test open"},
		{"the tracker began a test while T2's view stands", s.begin, integrations.RoundInTest, ""},
		{"the tracker began another test than T2", func() { s.end(); s.begin() }, integrations.RoundBoundary, "another test than T2"},
	}
	for _, st := range steps {
		st.do()
		snap := s.snapshot(t)
		if snap.Phase != st.phase || !strings.Contains(snap.Boundary, st.why) || snap.Open != (st.phase == integrations.RoundInTest) {
			t.Fatalf("%s: phase %d (%q) open %v; want %d (%q)", st.name, snap.Phase, snap.Boundary, snap.Open, st.phase, st.why)
		}
	}
	// A tracker whose boundaries never reach the store (a test's own
	// manager) binds no view: a test's view with the tracker open is in the
	// test, as the tracker's Open said alone.
	u := newRoundStore(t)
	u.tr.Begin()
	u.mm.ServeExactTest("T1", []*models.Mock{u.ms["own"]})
	u.tr.End()
	u.tr.Begin()
	if snap := u.snapshot(t); snap.Phase != integrations.RoundInTest {
		t.Fatalf("an unbound view reads phase %d (%s)", snap.Phase, snap.Boundary)
	}
}

// Many consumes, claims and snapshots at once, under -race: every snapshot
// holds each recording once, unconsumed or taken, never in neither.
func TestEverySnapshotHoldsEachRecordingOnce(t *testing.T) {
	s := newRoundStore(t)
	s.inTest()
	var wg sync.WaitGroup
	snap := s.snapshot(t)
	for _, name := range []string{"own", "own2"} {
		m := *shown(snap, name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.mm.DeleteFilteredMock(m)
		}()
	}
	wg.Add(2)
	go func() { defer wg.Done(); s.mm.ClaimPairing(s.ms["other"]) }()
	go func() { defer wg.Done(); s.mm.SpendByKind(s.ms["pool"]) }()
	check := func(snap integrations.RoundSnapshot) {
		taken := map[string]int{}
		for _, tk := range snap.Taken {
			taken[tk.Mock.Name]++
		}
		for _, name := range []string{"own", "own2"} {
			if in := shown(snap, name) != nil; in == (taken[name] == 1) {
				t.Errorf("%s shown %v, taken %d times", name, in, taken[name])
			}
		}
		if hasName(snap.Candidates, "other") == (taken["other"] == 1) {
			t.Errorf("other a candidate %v, taken %d times", hasName(snap.Candidates, "other"), taken["other"])
		}
		if hasName(snap.BackgroundUnused, "pool") == (taken["pool"] == 1) {
			t.Errorf("pool unused %v, taken %d times", hasName(snap.BackgroundUnused, "pool"), taken["pool"])
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	for {
		select {
		case <-done:
			check(s.snapshot(t))
			return
		default:
			check(s.snapshot(t))
		}
	}
}

// A view's install is one step: a snapshot read as the install is about to
// take the round lock is the round before it, part for part, at the
// generation before it; and a take decided on the round before it is
// refused once the new view is in force, before the revisions move.
func TestAViewInstallIsOneStepOfTheRound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(s *roundStore)
		swap  func(s *roundStore)
	}{
		{"a pool view after a pool view, the same names", (*roundStore).betweenWithAhead,
			func(s *roundStore) { s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"}, s.ms["next"]) }},
		{"a pool view after a pool view, other names", (*roundStore).betweenWithAhead,
			func(s *roundStore) { s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"}, s.ms["next2"]) }},
		{"a test's view after the pool's", func(s *roundStore) { s.betweenWithAhead(); s.begin() },
			func(s *roundStore) { s.mm.ServeExactTest("T2", []*models.Mock{s.ms["next"], s.ms["next2"]}) }},
		{"the pool's view after a test's", (*roundStore).inTest,
			func(s *roundStore) { s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"}, s.ms["next"]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRoundStore(t)
			tc.start(s)
			before := s.snapshot(t)
			r := &s.mm.exact.round
			armed := false
			var mid integrations.RoundSnapshot
			var midOK bool
			s.mm.exact.beforeInstall = func() { armed = true }
			r.onWait = func() {
				if armed {
					armed = false
					mid, midOK = s.mm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP, Tracker: s.tr, Pairing: true})
				}
			}
			var claimed, moved bool
			var why string
			s.mm.exact.beforeBump = func(*exactView) {
				claimed, moved, why = s.mm.ClaimPairingIn(s.ms["other"], before.At)
			}
			tc.swap(s)
			s.mm.exact.beforeInstall, s.mm.exact.beforeBump, r.onWait = nil, nil, nil
			if !midOK {
				t.Fatal("no snapshot as the install took the lock")
			}
			if got, want := roundSummary(mid), roundSummary(before); got != want {
				t.Fatalf("a snapshot read as the install took the lock is not the round before it:\n%s\nwant\n%s", got, want)
			}
			if claimed || !moved || !strings.Contains(why, "a view was installed") {
				t.Fatalf("a claim decided before the install, landing after it: claimed %v moved %v (%s)", claimed, moved, why)
			}
		})
	}
}

// A spend for the staging (a pool recording a call between tests consumed,
// a worker-scope one a test consumed) and a protocol-phase eviction in a
// test, until the view changes, are each written to the take log in the
// step that spends them.
func TestTheTakeLogHoldsEverySpend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		at      func(s *roundStore)
		mock    string
		test    string
		forView bool
	}{
		{"a pool recording between tests", func(s *roundStore) { s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"}) }, "pool", "", false},
		{"the worker scope's in a test", (*roundStore).inTest, "w-boot", "T1", false},
		{"a protocol-phase recording evicted in a test", (*roundStore).inTest, "hs", "T1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRoundStore(t)
			tc.at(s)
			before := s.snapshot(t)
			m := shown(before, tc.mock)
			if m == nil {
				t.Fatalf("precondition: %s is not shown", tc.mock)
			}
			if !s.mm.DeleteFilteredMock(*m) {
				t.Fatalf("the spend of %s was refused", tc.mock)
			}
			after := s.snapshot(t)
			if after.At.Taken != before.At.Taken+1 {
				t.Fatalf("%d takes after the spend, %d before", after.At.Taken, before.At.Taken)
			}
			tk := after.Taken[len(after.Taken)-1]
			if tk.Mock.Name != tc.mock || tk.How != integrations.TakeSpent || tk.Test != tc.test || tk.ForView != tc.forView || tk.Gen != after.At.Gen {
				t.Fatalf("the take is %+v", tk)
			}
			// The store's recording, not the door's copy of it.
			if tk.Mock != roundMock(s.mm, tc.mock) {
				t.Fatalf("the take names a copy of %s, not the staged recording", tc.mock)
			}
			if shown(after, tc.mock) != nil {
				t.Fatalf("%s is shown after its spend", tc.mock)
			}
		})
	}
}

// A recording the staging took is no pairing candidate in the views after,
// though the replay's account of the take (what consumedInSet reads) is
// written only once the round lock is let go: read in that instant, a
// test's own consumed recording, another test's in the next view, would be
// in the log and among the candidates both.
func TestARecordingTheStagingTookIsNoCandidateBeforeTheReplaysAccountOfIt(t *testing.T) {
	s := newRoundStore(t)
	s.inTest()
	own := *shown(s.snapshot(t), "own")
	var mid integrations.RoundSnapshot
	var claimed bool
	s.mm.exact.round.onSettled = func() {
		s.mm.exact.round.onSettled = nil
		s.mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
		mid = s.snapshot(t)
		claimed = s.mm.ClaimPairing(s.ms["own"])
	}
	if !s.mm.DeleteFilteredMock(own) {
		t.Fatal("the consume was refused")
	}
	if !mid.AnsweredInRound("own") || hasName(mid.Candidates, "own") {
		t.Fatalf("own answered in the round %v, and is a candidate %v (%v)", mid.AnsweredInRound("own"), hasName(mid.Candidates, "own"), exactNames(mid.Candidates))
	}
	if !hasName(mid.Candidates, "own2") {
		t.Fatalf("T1's unconsumed own2 is no candidate in the pool view: %v", exactNames(mid.Candidates))
	}
	if claimed {
		t.Fatal("a pairing claimed own, which T1 consumed")
	}
}

// The pairing's candidates the agent keeps on disk are read by name under
// the round lock and decoded outside it (LoadCandidates): one that fails to
// decode is left out and named, and a failed read of the names is said too,
// so the miss a call that is one of them makes is legible.
func TestTheRoundsCandidatesOnDiskNameWhatCouldNotBeRead(t *testing.T) {
	s := newRoundStore(t)
	s.inTest()
	disk := httpCall("disk-x", 9*time.Second, "/disk")
	broken, closed := errors.New("truncated"), errors.New("closed")
	s.mm.SetRecordedOfKind(func(kind models.Kind, visit func(string, func() (*models.Mock, error)) bool) error {
		visit("disk-x", func() (*models.Mock, error) { return disk, nil })
		visit("disk-bad", func() (*models.Mock, error) { return nil, broken })
		return closed
	})
	snap := s.snapshot(t)
	if len(snap.OnDisk) != 2 || !errors.Is(snap.OnDiskErr, closed) {
		t.Fatalf("the snapshot holds %d on disk, and says %v of their names", len(snap.OnDisk), snap.OnDiskErr)
	}
	cands, errs := snap.LoadCandidates(models.HTTP)
	if !hasName(cands, "disk-x") || hasName(cands, "disk-bad") {
		t.Fatalf("the candidates decoded are %v", exactNames(cands))
	}
	if len(errs) != 2 || errs[0].Name != "" || !errors.Is(errs[0], closed) || errs[1].Name != "disk-bad" || !errors.Is(errs[1], broken) {
		t.Fatalf("what could not be read: %+v", errs)
	}
}
