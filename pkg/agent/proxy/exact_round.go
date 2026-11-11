package proxy

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// THE ROUND LOCK (integrations.RoundReader).
//
// Exact serving's round is what a learn step decides a call from: the view
// and the staging it is of, the learn tracker's test, what the view's own
// answered, and what the staging consumed, spent and claimed. Each change of
// it is made holding one lock, roundState.mu, and a snapshot reads every
// part of it holding the same lock, so it reads them of one instant:
//
//   - a consume's or a spend's settle, every delete door's,
//     UpdateUnFilteredMock's and MarkMockAsUsed's, with the tier's take of
//     one the view passes and the served observer told of it in the same
//     step (exactConsumeAt), and a spend by kind (spendByKindAt);
//   - a pairing's claim (claimPairingAt);
//   - a view's install, with the claims a retried test gives back, the
//     mocks of the tests begun, and the ahead answers it carries or voids
//     (installExactWith). The close before it (closeAhead) changes nothing
//     a snapshot reads: consumes check it under the view's own lock, and a
//     take checked against a snapshot waits for the install it announces,
//     then is refused for it (movedLocked);
//   - the learn tracker's begin, end and new set (LearnBoundary);
//   - a staging's new epoch (bumpStagingEpoch).
//
// Lock order: swapMu (read) before the round lock; the round lock before a
// view's mu and the leaves (claims, staging names, the tracker, the disk
// store's, and what a consume's tier takes in the settle: mirrorMu, hitMu,
// treesMu, connMu read, a tree's, windowMu read, consumedMu, revMu,
// answeredLog.mu, the kept learning's filedBases.mu and keptLoad.mu).
// Nothing holding it takes swapMu, a variables table's lock or a consume door:
// prepareOutside, which takes swapMu, runs before it. A view install that
// begins variables takes the variables' lock and then this one
// (variables.Table.StartOpening runs the install).
//
// What the replay drains (flagMockAsUsed), the revisions and the window
// signal move after a view's own serve, as before: a parser that keeps an
// index reads again. A consume the view passes goes through its tier inside
// the step, since the tier decides whether it is a serve at all.

// roundState is the round's state that changes holding mu alone.
type roundState struct {
	mu sync.Mutex
	// gen is the round's generation (integrations.RoundAt.Gen), and moved
	// what moved it last; bounds its boundary count
	// (integrations.RoundAt.Bounds), which moves with gen at a view's
	// install and a boundary of the learn tracker alone (boundary).
	gen    uint64
	bounds uint64
	moved  string
	// log is the current staging's take log.
	log roundLog
	// learn is what the learn tracker's boundaries (LearnBoundary) last
	// said; bound the test view bound to the tracker's current test number
	// while that test is open (nil: none since the number last moved, or
	// the tracker ended the test). One view per number: a
	// test's view installed while the tracker's open test is another
	// view's is not that test's (the previous test's end never reached the
	// tracker), and the tracker's next begin binds it.
	learn learnBinding
	bound *exactView
	// stagedSet is the learn tracker's set as the last staging found it
	// (bumpStagingEpoch): the tracker's own (MockManager.SetTrackerSetAt),
	// else learnBinding.set. One the tracker began after it is a set the
	// store has not staged (integrations.LearnRead.SetAhead).
	stagedSet uint64
	// onWait and onRead are test seams, nil outside tests: onWait runs as a
	// change is about to take the lock (lockRound), onRead inside a
	// snapshot after each of its reads, with that read's name. A test
	// drives a change between any two reads with them, never with sleeps.
	onWait func()
	onRead func(step string)
	// onAnswerAgain is a test seam too: it runs as a held call is about to
	// be answered once more (heldScope.AnswerRepeatRead), its staging
	// checked, holding the round lock.
	onAnswerAgain func()
	// onSettled is a test seam too: it runs once a consume has settled and
	// let the round lock go, before what it tells the replay
	// (flagMockAsUsed).
	onSettled func()
	// onConsume is a test seam too (MockManager.OnConsume): it runs as a
	// consume is about to be settled, no lock held.
	onConsume atomic.Pointer[func(name string)]
	// moveCh is closed, and dropped, as the round moves (bump, a staging,
	// the manager's Close): a call waiting for the store to open the test
	// the learn tracker began (AwaitRoundMove) wakes on it. Made by the
	// first waiter; nil while none waits. closed: the manager was closed,
	// so no move is told any more.
	moveCh chan struct{}
	closed bool
	// onAwaitMove is a test seam, nil outside tests: it runs as a call
	// starts to wait in AwaitRoundMove, the round lock not held, so a test
	// moves the round after the call waits through no clock.
	onAwaitMove func()
}

// learnBinding is the learn tracker as its last boundary left it.
type learnBinding struct {
	// routed: a boundary of the tracker reached this manager.
	routed bool
	number uint64
	open   bool
	// set is the test number the tracker's running set began at
	// (learn.Tracker.SetAt).
	set uint64
}

// roundLog is one staging's take log (integrations.Take): appended to,
// never written again, so a snapshot shares it.
type roundLog struct {
	epoch uint64
	takes []integrations.Take
	// ahead is, by name, the entry of each ahead answer no view has
	// carried or voided yet; taken, the names of the recordings consumed or
	// spent in the staging and not given back since. A pairing's claims
	// are the staging's claimed set's (exactClaimed).
	ahead map[string]int
	taken map[string]bool
	// byTest is, by test, the entries of what its views answered in the
	// staging that a view of it installed again gives back (reopenLocked):
	// its own and the shared recordings it consumed, and the ahead answers
	// carried into it (their TakeConsumed entries).
	byTest map[string][]int
}

// lockRound takes the round lock for a change (onWait first).
func (m *MockManager) lockRound() {
	if f := m.exact.round.onWait; f != nil {
		f()
	}
	m.exact.round.mu.Lock()
}

// boundary moves the round's boundary count and its generation, holding its
// lock: a view's install, a boundary of the learn tracker. A call marked
// before it is moved (integrations.CallRound.MovedSince).
func (r *roundState) boundary(why string) {
	r.bounds++
	r.bump(why)
}

// bump moves the round's generation, holding its lock, and wakes every call
// waiting for it to move (AwaitRoundMove).
func (r *roundState) bump(why string) {
	r.gen++
	r.moved = why
	r.wakeLocked()
}

// wakeLocked wakes every call waiting for the round to move
// (AwaitRoundMove), holding the round lock.
func (r *roundState) wakeLocked() {
	if r.moveCh != nil {
		close(r.moveCh)
		r.moveCh = nil
	}
}

// AwaitRoundMove implements integrations.RoundWaiter: it waits until the
// round moves from at, its generation (a view's install, a boundary of the
// learn tracker: a test's begin or end on it, a new set) or its staging, or
// ctx ends, whose error it returns. moved is true once it moved, at once
// where it moved already; false, at once, where this manager cannot tell
// the move a call waits for: no exact serving, a learn tracker whose
// boundaries never reached it (its begins and ends then move no
// generation), or a manager closed (Close wakes its waiters so). It holds no
// lock while it waits, so a boundary, an install or a staging takes the
// round lock as ever. No clock: a waiter wakes on the move alone. A waiter
// scheduled only after the store ended the test it waited for reads the
// round moved on, and its call gets the moved miss: accepted as
// fail-closed, the same class as learn.Tracker.AwaitRequest's.
func (m *MockManager) AwaitRoundMove(ctx context.Context, at integrations.RoundAt) (moved bool, err error) {
	r := &m.exact.round
	r.mu.Lock()
	switch {
	case r.closed || !r.learn.routed || m.exactView() == nil:
		r.mu.Unlock()
		return false, nil
	case r.gen != at.Gen || m.stagingEpoch.Load() != at.Epoch:
		r.mu.Unlock()
		return true, nil
	}
	if r.moveCh == nil {
		r.moveCh = make(chan struct{})
	}
	ch, hook := r.moveCh, r.onAwaitMove
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	select {
	case <-ch:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	return !closed, nil
}

// OnAwaitRoundMove makes the manager call f as a call starts to wait in
// AwaitRoundMove, the round lock not held: the agent's tests order the
// round's moves, and keploy test's settle, after a waiting call through no
// clock. nil calls nothing.
func (m *MockManager) OnAwaitRoundMove(f func()) {
	r := &m.exact.round
	r.mu.Lock()
	r.onAwaitMove = f
	r.mu.Unlock()
}

// closeRound wakes every call waiting for the round to move, for good: the
// manager is closed (Close), and its round moves no more.
func (m *MockManager) closeRound() {
	r := &m.exact.round
	r.mu.Lock()
	r.closed = true
	r.wakeLocked()
	r.mu.Unlock()
}

// logLocked is the current staging's take log, holding the round lock: a
// staging after the one it was of starts it afresh.
func (m *MockManager) logLocked() *roundLog {
	l := &m.exact.round.log
	if e := m.stagingEpoch.Load(); l.epoch != e {
		*l = roundLog{epoch: e}
	}
	return l
}

// takeLocked writes t to the current staging's take log, holding the round
// lock, and returns its index.
func (m *MockManager) takeLocked(t integrations.Take) int {
	l := m.logLocked()
	switch t.How {
	case integrations.TakeCarried, integrations.TakeVoided, integrations.TakeRestored:
	default:
		t.Of = -1
	}
	l.takes = append(l.takes, t)
	i := len(l.takes) - 1
	if t.Mock == nil {
		return i
	}
	byTest := func(test string, at int) {
		if l.byTest == nil {
			l.byTest = map[string][]int{}
		}
		l.byTest[test] = append(l.byTest[test], at)
	}
	switch t.How {
	case integrations.TakeRestored:
		delete(l.taken, t.Mock.Name)
	case integrations.TakeCarried:
		if t.Of >= 0 {
			byTest(t.Test, t.Of)
		}
	case integrations.TakeConsumed, integrations.TakeSpent:
		if l.taken == nil {
			l.taken = map[string]bool{}
		}
		l.taken[t.Mock.Name] = true
		switch {
		case t.Ahead:
			if l.ahead == nil {
				l.ahead = map[string]int{}
			}
			l.ahead[t.Mock.Name] = i
		case t.How == integrations.TakeConsumed && t.Test != "":
			byTest(t.Test, i)
		}
	}
	return i
}

// takenInStagingLocked reports whether name was consumed or spent in the
// current staging (the take log), and not given back since, holding the
// round lock.
func (m *MockManager) takenInStagingLocked(name string) bool {
	return m.logLocked().taken[name]
}

// reopenLocked writes, holding the round lock as v, a test's view, is
// installed, that what an earlier view of the same test answered in the
// staging answers again: a test opened again (a retry) starts its own, the
// shared recordings it answered with (exactClaims.release) and the ahead
// answers carried into it afresh, as v shows them unconsumed. One
// TakeRestored entry per answer of a recording v holds (Of: its
// TakeConsumed entry), each given back once.
func (m *MockManager) reopenLocked(v *exactView) {
	l := m.logLocked()
	given := l.byTest[v.test]
	if len(given) == 0 {
		return
	}
	delete(l.byTest, v.test)
	for _, i := range given {
		mk := l.takes[i].Mock
		if _, held := v.index[mk.Name]; !held {
			continue
		}
		m.takeLocked(integrations.Take{Mock: mk, How: integrations.TakeRestored, Gen: v.round, Test: v.test, Of: i})
	}
}

// settleAheadLocked writes, holding the round lock as into is installed,
// what became of each ahead answer of the pool view into replaces
// (answered): carried into into's test, kept by a pool view that names it
// again, or voided (carriedTo).
func (m *MockManager) settleAheadLocked(r *roundState, answered []aheadAnswer, into *exactView, carriedTo func(name string) (carried, kept bool)) {
	for _, a := range answered {
		name := a.owned.Name
		carried, kept := carriedTo(name)
		if kept {
			continue
		}
		l := m.logLocked()
		of, ok := l.ahead[name]
		if !ok {
			of = -1
		}
		delete(l.ahead, name)
		how := integrations.TakeVoided
		if carried {
			how = integrations.TakeCarried
		}
		m.takeLocked(integrations.Take{Mock: a.owned, How: how, Gen: r.gen, Test: into.test, Of: of})
	}
}

// restoreLocked writes, holding the round lock, that each of names the
// staging spent, and did not give back since, answers again
// (RestartExactKey: a TakeRestored entry of its last spend), and moves the
// round's generation: a take decided on a snapshot before it is refused.
func (m *MockManager) restoreLocked(names []string) {
	r := &m.exact.round
	l := m.logLocked()
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	// The last spend for the staging of each name, not given back since.
	last := map[string]int{}
	for i, t := range l.takes {
		if t.Mock == nil || !want[t.Mock.Name] {
			continue
		}
		switch {
		case t.How == integrations.TakeSpent && !t.ForView:
			last[t.Mock.Name] = i
		case t.How == integrations.TakeRestored:
			if at, ok := last[t.Mock.Name]; ok && at == t.Of {
				delete(last, t.Mock.Name)
			}
		}
	}
	r.bump("a test process claimed its key again: what it spent answers again")
	test := ""
	if v := m.exact.view.Load(); v != nil {
		test = v.test
	}
	for _, n := range names {
		if i, ok := last[n]; ok {
			m.takeLocked(integrations.Take{Mock: l.takes[i].Mock, How: integrations.TakeRestored, Gen: r.gen, Test: test, Of: i})
		}
	}
}

// stagedRecording is the staging's own recording of mk's name (what a
// spend for the staging is checked against), or mk.
func (m *MockManager) stagedRecording(mk *models.Mock) *models.Mock {
	if st := m.exact.outside.cur.Load(); st != nil && st.epoch == m.stagingEpoch.Load() {
		if r := st.staged[mk.Name]; r != nil {
			return r
		}
	}
	return mk
}

// bumpStagingEpoch starts a new staging epoch, holding the round lock: a
// snapshot reads one staging's take log and epoch together. Called holding
// swapMu, which comes before it.
func (m *MockManager) bumpStagingEpoch() {
	m.lockRound()
	// The outside answers of the staging before (prepareOutside) hold every
	// recording it staged: let them go with that staging's trees, not at the
	// next call outside every test, which may come only well into this
	// staging, or never. They go before the epoch moves on, and they are
	// built only under swapMu, which this staging holds, with the epoch read
	// there: so the outside answers are, at every instant, none or the
	// current staging's, and no read that takes no lock (outsideNow) ever
	// sees a past staging's answers under the new epoch.
	m.exact.outside.cur.Store(nil)
	if f := m.exact.outside.onLetGo; f != nil {
		f()
	}
	m.stagingEpoch.Add(1)
	// The set the tracker began last is staged now, whichever it is: the
	// tracker's own (SetTrackerSetAt), as its set may have begun before any
	// of its boundaries reached this manager.
	if f := m.trackerSetAt.Load(); f != nil {
		m.exact.round.stagedSet = (*f)()
	} else {
		m.exact.round.stagedSet = m.exact.round.learn.set
	}
	m.exact.round.wakeLocked()
	m.exact.round.mu.Unlock()
}

// roundGuard is a take checked against a snapshot's round (at): refused, and
// why kept, once the store moved from it. in is the request the take serves,
// as its parser said (integrations.ServingIn), told to the served observer;
// free: a guard of ServingFor that checks no round. staging: a guard of a
// call's mark (HoldToRound) that checks the staging alone (at.Epoch): a view
// replaced within it, at a test's begin or end, is still the call's set, and
// a consume that finds its view closed is settled in the view that replaces
// it, as one with no guard is, but for a consume of one of the call's own
// test's recordings, settled in the view the call was read in (ownView).
// restaged: a staging guard found the staging moved. readIn: the view a
// staging guard's call was read in (its mark's), in which the call's reads,
// walk, worker spend and own test's consumes are made (heldScope.readView,
// heldScope.SequenceView, windowed, ownView); nil when they are made in the
// view in force (heldScope.readView says when). ownIn: the view a guard of
// the round made from a round snapshot read (WithinRoundOf), in which its
// call's consume of one of that view's test's recordings settles (ownView);
// its other consumes settle in the view in force, refused once the round
// moved.
type roundGuard struct {
	at       integrations.RoundAt
	moved    string
	in       *learn.Flight
	free     bool
	staging  bool
	restaged bool
	readIn   *exactView
	ownIn    *exactView
}

// windowed reports whether a consume settled in v is a test window's: made
// by a call read in a test's view. It decides which spend of the worker
// scope the consume reads and makes (exactView.spendsInTests,
// spentOutsideIn): the windows' or the calls' between tests. A staging
// guard's call (HoldToRound) settles in the view in force, which a test's
// begin or end may have installed since its read (one of its own test's
// recordings in the view it was read in, ownView); its spend is made in the
// view its reads are (heldScope.readView), as its walk is
// (heldScope.SequenceView): the view it was read in (readIn) while it reads
// there. So a call read between tests that commits after the next test
// opened never spends a worker recording for the windows, nor one read in a
// test that commits after its end for the calls between tests: a window's
// answers never move with when a call's commit lands. A call that reads the
// view in force (one marked where the learn tracker was ahead of the store,
// decided in the test the store opens next) spends there too, so a worker
// recording it is answered in a window is spent for the windows. Any other
// consume settles in the view it was read in (a guard of the round refuses
// one the round moved under), or tells no read: v's.
func (g *roundGuard) windowed(v *exactView) bool {
	if g != nil && g.readIn != nil {
		return g.readIn.test != ""
	}
	return v.test != ""
}

// ownView is the view a consume of mk by g's call settles in when mk is one
// of the recordings of the test whose view the call was read in (readIn, or
// for a guard of the round, the view its round snapshot read: ownIn): that
// view, whether or not it is still in force, or the round moved since: only
// a restage refuses it (stagingGuardLocked). keploy can decide a call
// after the replay ended its test, or began the next: at zero latency the
// call would have been decided in its own view, so it is still answered by
// its own test's recording, unanswered there, and never by the next test's
// (which the view in force would refuse it, or hand it). One its view's
// test answered already is refused there, as at once. A restage refuses it
// (guardLocked): the staging's recordings are another set's then. nil once
// the agent reset the replay session the view was of (sessionEnded): the
// consume settles in the view in force, which refuses it, as it refuses
// every call of an ended session (consumeOwnedLocked checks it again under
// the round lock, for a reset that lands after this). nil for any other
// consume, which settles in the view in force: a call that reads
// the view in force, one read in a pool view (its ahead recordings are the
// next test's, carried into that test's view as it opens), and a recording
// the call's view does not own (the pool's, the worker scope's, the
// start-up's).
func (g *roundGuard) ownView(mk *models.Mock) *exactView {
	if g == nil || mk == nil {
		return nil
	}
	v := g.readIn
	if v == nil {
		v = g.ownIn
	}
	if v != nil && v.test != "" && v.owns(mk.Name) && !v.sessionEnded() {
		return v
	}
	return nil
}

// checks says g checks its take against a round: a guard, not ServingFor's.
func (g *roundGuard) checks() bool { return g != nil && !g.free }

// flight is the request g's take serves; nil for none said, or no g.
func (g *roundGuard) flight() *learn.Flight {
	if g == nil {
		return nil
	}
	return g.in
}

// refuse keeps why the first move refused a take.
func (g *roundGuard) refuse(why string) {
	if g.moved == "" {
		g.moved = why
	}
}

// movedLocked says, holding the round lock, what moved since at was read
// with v in force: "" when nothing did. wait, when not nil, says v is
// closed, its replacement not installed yet: the round is about to move.
// The take is to wait for it (outside the round lock), then be checked
// again, and refused for the install: a caller that read the round again
// at once would read the closing view, and take, be refused and read
// again, until the install lands.
func (m *MockManager) movedLocked(at integrations.RoundAt, v *exactView) (why string, wait <-chan struct{}) {
	r := &m.exact.round
	switch {
	case m.exactView() == nil:
		return "exact serving ended", nil
	case m.stagingEpoch.Load() != at.Epoch:
		return restagedWhy, nil
	case r.gen != at.Gen:
		return "the round moved since it was read: " + r.moved, nil
	}
	if v != nil {
		v.mu.Lock()
		wait = v.closedWait()
		v.mu.Unlock()
	}
	return "", wait
}

// guardLocked checks a take against g's round, holding the round lock:
// refused, the lock let go and why kept in g, when the round moved; wait,
// the lock let go, when it is about to (movedLocked). ok: the take goes on,
// holding the lock. With no g, every take goes on.
func (m *MockManager) guardLocked(g *roundGuard, v *exactView) (ok bool, wait <-chan struct{}) {
	if !g.checks() {
		return true, nil
	}
	if g.staging {
		return m.stagingGuardLocked(g), nil
	}
	why, wait := m.movedLocked(g.at, v)
	if why == "" && wait == nil {
		return true, nil
	}
	m.exact.round.mu.Unlock()
	if why != "" {
		g.refuse(why)
	}
	return false, wait
}

// stagingGuardLocked checks a take against g's staging alone, holding the
// round lock: refused, the lock let go and why kept in g, once the set was
// staged again. A staging guard checks every take so (guardLocked); any
// guard checks so a consume settled in its call's own view (ownView), which
// a view's install or a learn boundary since never refuses: the view's
// recordings stay its test's until the set is staged again. g checks a
// round (roundGuard.checks).
func (m *MockManager) stagingGuardLocked(g *roundGuard) bool {
	if m.stagingEpoch.Load() == g.at.Epoch {
		return true
	}
	m.exact.round.mu.Unlock()
	g.restaged = true
	g.refuse(restagedWhy)
	return false
}

// LearnBoundary runs step (the learn tracker's Begin, End or NewSet) on t
// holding the round lock, and moves the round's generation: a snapshot reads
// the tracker and the store's view of one instant, and a test's view is
// bound to the tracker's open test (the agent's proxy routes every boundary
// of its tracker here). A test's view installed with the tracker's test open
// is that test's, unless a view is bound to that test already (the previous
// test's end never reached the tracker); one installed with none open, or
// after another view was bound to the open test, is the test the tracker
// begins next. A view of a tracker whose boundaries never reach this
// manager is bound to none (integrations.RoundSnapshot.Phase then reads a
// test's view with the tracker open as in the test).
func (m *MockManager) LearnBoundary(t *learn.Tracker, step func(*learn.Tracker)) {
	if t == nil {
		// No tracker: nothing for a view to be bound to.
		step(t)
		return
	}
	m.lockRound()
	defer m.exact.round.mu.Unlock()
	step(t)
	r := &m.exact.round
	n, open, _ := t.State()
	if !r.learn.routed || n != r.learn.number {
		// A new test number: no view is its yet.
		r.bound = nil
	}
	r.learn = learnBinding{routed: true, number: n, open: open, set: t.SetAt()}
	if !open {
		// The bound view is read only while its test is open
		// (endedLocked), and a number the tracker ended never opens
		// again (each begin moves it): let the ended test's view, and
		// the recordings it holds, go now, not at the next begin.
		r.bound = nil
		r.boundary("the learn tracker ended its test")
		return
	}
	r.boundary("the learn tracker began a test")
	// A test's view the store installed before the tracker began this test
	// (it opened the test first), and bound to none, is the test the
	// tracker begins now.
	if v := m.exact.view.Load(); v != nil && v.test != "" && !v.learnBound && r.bound == nil {
		v.learnNumber, v.learnBound = n, true
		r.bound = v
	}
}

// bindLocked binds v, a test's view being installed, to the learn tracker's
// open test, holding the round lock: when the tracker's boundaries reach
// this manager, a test is open on it, and no view is bound to that test
// yet. A second view installed under one open test is bound to none: the
// open test is the first's, and the tracker's next begin binds the second.
func (r *roundState) bindLocked(v *exactView) {
	if !r.learn.routed || !r.learn.open || r.bound != nil {
		return
	}
	v.learnNumber, v.learnBound = r.learn.number, true
	r.bound = v
}

// endedLocked is, holding the round lock, the view bound to the test the
// learn tracker has open (number, open) where v, the view in force, is not
// it: the store installed its pool view, or a test's view bound to none,
// after that test's, while the tracker kept the test open
// (integrations.RoundSnapshot.Ended). nil otherwise. It is the view last
// bound to that number (roundState.bound): an older test's view where a view
// was opened and ended without its test's begin reaching the tracker, and
// one that survives a staging of the set again until the tracker ends that
// test. Held as a call's own, it fails closed either way.
func (r *roundState) endedLocked(v *exactView, number uint64, open bool) *exactView {
	if b := r.bound; open && b != nil && !v.learnBound && b.learnNumber == number {
		return b
	}
	return nil
}

// phaseLocked is where the round is (integrations.RoundPhase), from v and
// the tracker read l, holding the round lock, with which side moved at a
// boundary, and the test the store ended that the tracker still has open
// (integrations.RoundSnapshot.Ended).
func (m *MockManager) phaseLocked(v *exactView, l integrations.LearnRead) (phase integrations.RoundPhase, boundary, ended string) {
	open := l.Open
	if !l.Bound {
		// No tracker read: the store's view alone.
		open = v.test != ""
	}
	if l.Bound && l.SetAhead {
		// The tracker began a set the store has not staged: whatever view
		// the store shows, its pool's or a test's, is the set before's.
		return integrations.RoundBoundary, "the learn tracker began a test set the store has not staged", ""
	}
	if v.test != "" {
		switch {
		case !open:
			return integrations.RoundBoundary, "the store shows " + v.test + "'s view and the learn tracker has no test open", ""
		case l.Bound && v.learnBound && v.learnNumber != l.Number:
			return integrations.RoundBoundary, "the learn tracker began another test than " + v.test + ", whose view the store still shows", ""
		}
		if l.Bound {
			if b := m.exact.round.endedLocked(v, l.Number, true); b != nil {
				return integrations.RoundBoundary, "the learn tracker's open test is " + b.test + "'s, and the store shows " + v.test + "'s view", b.test
			}
		}
		return integrations.RoundInTest, "", ""
	}
	if !open {
		if !m.exact.fired.Load() {
			return integrations.RoundStartUp, "", ""
		}
		return integrations.RoundBetween, "", ""
	}
	if b := m.exact.round.endedLocked(v, l.Number, open); b != nil {
		return integrations.RoundBoundary, "the store ended " + b.test + ", which the learn tracker still has open", b.test
	}
	return integrations.RoundBoundary, "the learn tracker began a test the store has not opened", ""
}

// RoundSnapshot implements integrations.RoundReader: every part read holding
// swapMu (read) and the round lock, which every change of the round takes.
func (m *MockManager) RoundSnapshot(q integrations.RoundQuery) (integrations.RoundSnapshot, bool) {
	m.swapMu.RLock()
	defer m.swapMu.RUnlock()
	r := &m.exact.round
	r.mu.Lock()
	defer r.mu.Unlock()
	v := m.exact.view.Load()
	if v == nil {
		return integrations.RoundSnapshot{}, false
	}
	read := func(step string) {
		if r.onRead != nil {
			r.onRead(step)
		}
	}
	l := m.logLocked()
	n := len(l.takes)
	s := integrations.RoundSnapshot{
		At:         integrations.RoundAt{Epoch: l.epoch, Gen: r.gen, Bounds: r.bounds, Taken: n},
		Reads:      m.windowReads.Load(),
		Test:       v.test,
		Ahead:      v.test == "" && len(v.owned) > 0,
		Fired:      m.exact.fired.Load(),
		Background: v.bg != nil,
		Staging:    v.staging,
		Taken:      l.takes[:n:n],
	}
	read("view")

	if t := q.Tracker; t != nil {
		// One read of the tracker: the test's number and openness of the
		// same instant as its places and whether it awaits its request,
		// which a call's learn step holds its later passes to.
		p := t.Placed()
		s.Learn = integrations.LearnRead{Bound: true, Number: p.Test, Open: p.Open, Begun: p.Begun}
		s.Learn.SetAhead = r.learn.routed && p.SetAt != r.stagedSet
		s.Learn.Earlier, s.Learn.Served, s.Learn.Before, s.Learn.Awaiting = p.Earlier, p.Served, p.Before, p.Awaiting
		s.Learn.Set, s.Learn.Order = p.Set, p.Order
		s.Learn.Answered = t.AnsweredMocks()
	}
	s.Phase, s.Boundary, s.Ended = m.phaseLocked(v, s.Learn)
	s.Open = s.Phase == integrations.RoundInTest
	read("learn")

	m.treesMu.RLock()
	filtered, unfiltered, startup := m.filtered, m.unfiltered, m.startup
	m.treesMu.RUnlock()
	// What the view shows of each tier (shownOf): perTest and reusable keep
	// only those, so the snapshot never walks every test's recordings.
	filteredVals, unfilteredVals, startupVals := v.shownOf(filtered, ""), v.shownOf(unfiltered, ""), v.shownOf(startup, "")
	s.PerTest = v.perTest("", filteredVals, m.perTestFromStartup(v, startupVals, s.Fired))
	read("per-test")
	s.Startup = v.reusable("", startupVals, unfilteredVals)
	s.SessionScoped = v.reusable("", unfilteredVals, startupVals)
	s.Session = sessionUnion(v.unanswered(""), s.Startup, s.SessionScoped)
	read("session")

	owners := make(map[string]integrations.RoundOwner, len(v.owned))
	claimants := map[string]string{}
	v.mu.Lock()
	s.Consumed = make([]*models.Mock, 0, len(v.order))
	for _, i := range v.order {
		s.Consumed = append(s.Consumed, v.owned[i])
	}
	v.mu.Unlock()
	epoch := m.stagingEpoch.Load()
	for i, mk := range v.owned {
		switch {
		case v.test == "":
			owners[mk.Name] = integrations.OwnerAhead
		case v.isCand(i):
			if t, ok := v.claims.claimant(epoch, mk.Name); ok && t != v.test {
				owners[mk.Name], claimants[mk.Name] = integrations.OwnerSharedTaken, t
			} else {
				owners[mk.Name] = integrations.OwnerShared
			}
		case v.inFlight[mk.Name]:
			owners[mk.Name] = integrations.OwnerInFlight
		default:
			owners[mk.Name] = integrations.OwnerTest
		}
	}
	read("consumed")

	if q.Kind != "" && v.bg != nil {
		for _, mk := range m.stagedOfKind(q.Kind) {
			if !v.isBackgroundRecording(mk) {
				continue
			}
			if v.spentBackground(mk.Name) {
				s.BackgroundSpent = append(s.BackgroundSpent, mk)
			} else {
				s.BackgroundUnused = append(s.BackgroundUnused, mk)
			}
		}
		byRecorded(s.BackgroundUnused)
		byRecorded(s.BackgroundSpent)
	}
	read("background")

	if q.Pairing && q.Kind != "" {
		s.Candidates, s.OnDisk, s.OnDiskErr = m.pairingCandidatesLocked(q.Kind)
	}
	read("candidates")

	parts := integrations.RoundParts{
		Owners: owners, Claimants: claimants, Worker: v.worker, Pool: v.pool,
		Windows: m.testWindows.Load(), PoolNames: m.exact.poolNames.Load(),
		ViewGen: v.round, View: viewCursor{m: m, v: v},
	}
	if b := r.bound; s.Ended != "" && b != nil {
		// A call read here may be the ended test's late one: its
		// recordings are no other test's for it (CallRound).
		parts.EndedOwn = make(map[string]struct{}, len(b.owned))
		for _, mk := range b.owned {
			parts.EndedOwn[mk.Name] = struct{}{}
		}
	}
	if v.bg != nil {
		parts.BackgroundPool, parts.BackgroundWorker = v.bg.pool, v.bg.worker
	}
	m.windowMu.RLock()
	parts.WindowStart, parts.WindowEnd = m.windowStart, m.windowEnd
	m.windowMu.RUnlock()
	return s.WithParts(parts), true
}

// sessionUnion is GetSessionMocks under exact serving, of the reads it
// unites: the view's unanswered own, then the start-up tier, then the
// session tier, each mock once.
func sessionUnion(owned, startup, session []*models.Mock) []*models.Mock {
	if len(owned) > 0 {
		startup = append(append(make([]*models.Mock, 0, len(owned)+len(startup)), owned...), startup...)
	}
	if len(startup) == 0 {
		return session
	}
	out := make([]*models.Mock, 0, len(startup)+len(session))
	seen := make(map[*models.Mock]struct{}, len(startup)+len(session))
	for _, list := range [][]*models.Mock{startup, session} {
		for _, mk := range list {
			if mk == nil {
				continue
			}
			if _, dup := seen[mk]; dup {
				continue
			}
			seen[mk] = struct{}{}
			out = append(out, mk)
		}
	}
	return out
}

// byRecorded sorts ms by recorded request time, stably.
func byRecorded(ms []*models.Mock) {
	sort.SliceStable(ms, func(i, j int) bool {
		return ms[i].Spec.ReqTimestampMock.Before(ms[j].Spec.ReqTimestampMock)
	})
}

// pairingCandidatesLocked is PairingCandidates of kind holding the round
// lock: those the trees hold, in recorded order, and those the agent keeps
// on disk by name, not decoded (integrations.RoundSnapshot.LoadCandidates
// decodes them outside the lock), with what the disk store's read of their
// names returned.
func (m *MockManager) pairingCandidatesLocked(kind models.Kind) ([]*models.Mock, []integrations.DiskCandidate, error) {
	v, epoch := m.pairing()
	if v == nil {
		return nil, nil, nil
	}
	seen := map[string]bool{}
	var mem []*models.Mock
	// What the staging took is no candidate, though the replay's account
	// of it (consumedInSet) is written only after the take.
	for _, mk := range m.stagedOfKind(kind) {
		seen[mk.Name] = true
		if m.pairable(v, epoch, mk) && !m.takenInStagingLocked(mk.Name) {
			mem = append(mem, mk)
		}
	}
	byRecorded(mem)
	m.exact.ofKind.mu.Lock()
	src := m.exact.ofKind.src
	m.exact.ofKind.mu.Unlock()
	var disk []integrations.DiskCandidate
	var err error
	if src != nil {
		err = src(kind, func(name string, load func() (*models.Mock, error)) bool {
			if seen[name] {
				return true
			}
			seen[name] = true
			// A recording only the disk holds is taken in the staging by a
			// claim alone, which pairableName reads.
			if m.pairableName(v, epoch, name) {
				disk = append(disk, integrations.DiskCandidate{Name: name, Load: load})
			}
			return true
		})
	}
	return mem, disk, err
}

// ClaimPairingIn implements integrations.RoundReader.
func (m *MockManager) ClaimPairingIn(mk *models.Mock, at integrations.RoundAt) (claimed, moved bool, why string) {
	g := &roundGuard{at: at}
	claimed, why = m.claimPairingAt(mk, g)
	return claimed, g.moved != "", why
}

// SpendByKindIn implements integrations.RoundReader.
func (m *MockManager) SpendByKindIn(mk *models.Mock, at integrations.RoundAt) (spent, moved bool, why string) {
	g := &roundGuard{at: at}
	spent, why = m.spendByKindAt(mk, g)
	return spent, g.moved != "", why
}

// TakenLog implements integrations.RoundReader: entries [from, to) of
// staging epoch's take log, clipped to it; nil for a staging the store
// left.
func (m *MockManager) TakenLog(epoch uint64, from, to int) []integrations.Take {
	m.exact.round.mu.Lock()
	defer m.exact.round.mu.Unlock()
	l := m.logLocked()
	if l.epoch != epoch {
		return nil
	}
	to = min(to, len(l.takes))
	if from < 0 || from >= to {
		return nil
	}
	return l.takes[from:to:to]
}

// WithinRound implements integrations.RoundReader: the manager, with its
// consume doors settled only while it is at at.
func (m *MockManager) WithinRound(at integrations.RoundAt) integrations.RoundScope {
	return &roundScope{MockManager: m, g: roundGuard{at: at}}
}

// WithinRoundOf implements integrations.RoundViewReader: WithinRound of the
// round snap read, whose consume of one of the recordings of the test whose
// view snap read settles in that view (roundGuard.ownView). Record's full
// round's learn step claims through it, so a call it decided in its test's
// view is answered by its test's recording, unanswered there, after the
// replay ended that test or began the next, as it would have been at once.
func (m *MockManager) WithinRoundOf(snap integrations.RoundSnapshot) integrations.RoundScope {
	s := &roundScope{MockManager: m, g: roundGuard{at: snap.At}}
	if c, mine := snap.StoreView().(viewCursor); mine && c.m == m {
		s.g.ownIn = c.v
	}
	return s
}

var (
	_ integrations.RoundReader     = (*MockManager)(nil)
	_ integrations.RoundViewReader = (*MockManager)(nil)
	_ integrations.RoundWaiter     = (*MockManager)(nil)
)

// roundScope is the manager whose consume doors settle only while it is at
// one snapshot's round (WithinRound); every other method is the manager's.
// One call's: not for sharing between goroutines.
type roundScope struct {
	*MockManager
	g roundGuard
}

// Moved implements integrations.RoundScope.
func (s *roundScope) Moved() string { return s.g.moved }

func (s *roundScope) DeleteFilteredMock(mock models.Mock) bool {
	return s.deleteFilteredMock(mock, &s.g)
}

func (s *roundScope) DeleteUnFilteredMock(mock models.Mock) bool {
	return s.deleteUnFilteredMock(mock, &s.g)
}

func (s *roundScope) DeleteStartupMock(mock models.Mock) bool {
	return s.deleteStartupMock(mock, &s.g, true)
}

func (s *roundScope) UpdateUnFilteredMock(old, new *models.Mock) bool {
	return s.updateUnFilteredMock(old, new, &s.g)
}

func (s *roundScope) MarkMockAsUsed(mock models.Mock) bool {
	return s.markMockAsUsed(mock, &s.g)
}

var _ integrations.RoundScope = (*roundScope)(nil)

// ServingFor implements integrations.ServeTeller: the manager, whose consume
// doors tell the served observer that what they serve is served in the
// request f, and check no round.
func (m *MockManager) ServingFor(f *learn.Flight) integrations.MockMemDb {
	return &roundScope{MockManager: m, g: roundGuard{in: f, free: true}}
}

// ServingFor implements integrations.ServeTeller: s, whose consume doors tell
// the served observer that what they serve is served in the request f. s is
// one call's, so it is told in place.
func (s *roundScope) ServingFor(f *learn.Flight) integrations.MockMemDb {
	s.g.in = f
	return s
}

var (
	_ integrations.ServeTeller = (*MockManager)(nil)
	_ integrations.ServeTeller = (*roundScope)(nil)
)

// restagedWhy is what a guard keeps when the staging moved under its take.
const restagedWhy = "the staging moved: the test set was staged again"

// HoldToRound implements integrations.RoundHolder: the manager held to the
// staging call was marked in (heldScope). restaged reports whether a read or
// a consume door of it found the staging moved since.
func (m *MockManager) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	h := &heldScope{roundScope: roundScope{MockManager: m, g: roundGuard{at: integrations.RoundAt{Epoch: call.Epoch()}, staging: true}}, heldIn: m.exactView()}
	h.read.at, h.read.known = call.Round()
	snapped := false
	if c, mine := call.StoreView().(viewCursor); mine && c.m == m {
		// Marked from a round snapshot (integrations.RoundSnapshot.CallRound:
		// record's full round, its learn step's read), which reads and walks
		// in the view in force: held to the view that snapshot read, so a
		// view installed after it, before this hold or between two holds of
		// the call, is a move to every hold (CallViewMoved), and no verdict
		// joins a read of one view with a read of the next.
		h.heldIn, snapped = c.v, true
	}
	if call.TrackerAhead() {
		// Decided in the test the store opens next: the call reads, walks
		// and spends in the view in force (readView).
		return h, h.restaged
	}
	if seq, ok := call.Sequence(); ok {
		if c, mine := seq.Cursor.(viewCursor); mine && c.m == m {
			// Read in the view its mark kept: held there too, so a read in
			// any other is a move (CallViewMoved), even when a test's begin
			// or end installed the view in force between the mark and here.
			h.seq, h.g.readIn, h.heldIn = seq, c.v, c.v
		}
	}
	h.snapped = snapped
	return h, h.restaged
}

var (
	_ integrations.RoundHolder = (*MockManager)(nil)
	_ integrations.ServeTeller = (*heldScope)(nil)
	// A parser's per-generation index serves only a store that tells whose
	// a recording is (the HTTP strict index).
	_ integrations.TestOwner = (*heldScope)(nil)
	// A late call's variables are its own test's (Variables).
	_ integrations.VariableStore  = (*heldScope)(nil)
	_ integrations.VariableScopes = (*heldScope)(nil)
)

// heldScope is the manager held to one call's staging (HoldToRound): its
// consume doors (roundScope's, and the background spend and claim) settle
// only while the staging is still the call's, and each read that serves or
// diagnoses a call finds nothing once the staging moved, checked after the
// read is made: a staging moves its epoch before it changes the trees or
// installs a view, so a read the staging is still the call's after was of
// the call's set. A miss of a call marked from a round snapshot finds
// nothing, too, once its view was replaced (viewGone). One call's: not for
// sharing between goroutines.
type heldScope struct {
	roundScope
	// seq is the sequence view of the view the call was read in
	// (integrations.CallRound.Sequence, the view g.readIn), which h walks
	// while it reads there (SequenceView).
	seq integrations.SequenceView
	// owns is the call's decision whether it is one of the open test's own
	// calls (HideBackgroundWhen), until a door or HidesBackground asks it.
	owns func() bool
	// read is the round the call was read in (CallRound.Round), so what it
	// teaches across windows names that window only while the round is
	// still there (learnElsewhereAt).
	read callRead
	// hideBg says the call is one of them (HidesBackground): the set's
	// background is in none of h's reads but KindBackground's, and its doors
	// refuse it (hides).
	hideBg bool
	// ownGen is the generation h reports while it reads elsewhere
	// (elsewhereGen), 0 until then.
	ownGen uint64
	// heldIn is the view h's call was read in: its mark's (readIn) when the
	// mark kept one, the round snapshot's its mark was made from
	// (integrations.CallRound.StoreView), else the view in force as h was
	// held (HoldToRound). A call that reads the view in force (readIn nil:
	// from its hold on, or since the view in force refused a consume,
	// settled) is told it moved from it (CallViewMoved). Views are installed
	// new and never
	// again, so one still in force is the one the call was read in.
	heldIn *exactView
	// snapped says h's call was marked from a round snapshot, not one
	// decided in the test the store opens next: it reads, walks and spends
	// in the view in force, its miss diagnosed in the view that snapshot
	// read (DiagnoseInCallView).
	snapped bool
	// diagnosing says h reads for its call's miss only in the view its
	// round snapshot read (DiagnoseInCallView), nothing once a test's begin
	// or end replaced it (viewGone).
	diagnosing bool
}

// DiagnoseInCallView implements integrations.CallViewDiagnoser: h, told in
// place (one call's), reads from now on for its call's miss. A call marked
// from a round snapshot (record's full round's learn step) reads only in
// the view that snapshot read: nothing once a test's begin or end replaced
// it (viewGone), so its miss names no recording of the view in force (the
// next test's, or the pool's) as its closest, nor diffs the call against
// one. The snapshot kept no walk of its view to read that view in. Any
// other call reads as before: its mark's view (readIn), or the view in
// force for one decided in the test the store opens next, where it is
// decided.
func (h *heldScope) DiagnoseInCallView() integrations.MockMemDb {
	h.diagnosing = h.snapped
	return h
}

// CallViewGoneWhy implements integrations.CallViewGoneTeller: once h reads
// nothing for its call's miss because a test's begin or end replaced the
// view its round snapshot read (viewGone), which test the replay ended or
// began, and that the miss therefore names no recording; "" otherwise.
func (h *heldScope) CallViewGoneWhy() string {
	if !h.viewGone() {
		return ""
	}
	// heldIn is a round snapshot's view (diagnosing: snapped), never nil.
	was, now := h.heldIn.test, h.exactView().testName()
	where, what := "between tests", "served another view of its recordings between tests"
	switch {
	case was != "" && now != "":
		where, what = "in "+was, "ended "+was+" and began "+now
	case was != "":
		where, what = "in "+was, "ended "+was
	case now != "":
		what = "began " + now
	}
	return "keploy record's full round decided this request " + where + ", and the replay " + what + " before keploy diagnosed its miss, so no recording is named for it: keploy reads nothing of a view a test's begin or end replaced."
}

// testName is v's open test, "" for none or no v.
func (v *exactView) testName() string {
	if v == nil {
		return ""
	}
	return v.test
}

var (
	_ integrations.CallViewDiagnoser  = (*heldScope)(nil)
	_ integrations.CallViewGoneTeller = (*heldScope)(nil)
)

// SequenceView implements integrations.SequenceViewer: the sequence view of
// the view h's call was read in, so a call read in one view and walked after
// the next one is installed (a test opening or ending between the read and
// the walk) walks the view it was read in, its cursor and its window, never
// the next view's; while h reads in the view in force (readView), that
// view's.
func (h *heldScope) SequenceView() (integrations.SequenceView, bool) {
	if h.g.readIn != nil {
		return h.seq, true
	}
	return h.MockManager.SequenceView()
}

// readView is the view h's reads of the staged recordings are made in: the
// view the call was read in, as its walk is (SequenceView) and its worker
// scope's spend (roundGuard.windowed), never one a test's begin or end
// installed after the read. So a read that lands late is answered as one
// that landed on time: a call read between tests reads the worker scope in
// gap order and whatever a window spent of it, a test's call what the windows
// left, and neither reads the recordings of a test whose view came or went
// meanwhile. A consume of one of its own test's recordings settles in that
// view too (roundGuard.ownView), in force or not: at zero latency keploy
// decides the call there, so a recording its test left unanswered answers
// it, and the next test's never does. Before, it settled in the view in
// force, which refused it, and the call missed with its own recording
// unanswered, or read on in the view in force and was answered by the next
// test's. Its other consumes settle in the view in force (roundGuard),
// spending the worker scope for the view the call was read in (windowed):
// one the view in force refuses because that spend says another call took
// the recording keeps the call reading in its own view, which no longer
// offers it, so the call is answered by the next recording its own view
// offers, or misses there, as at zero latency (settled). A call marked where
// the learn tracker had begun a test the store had not opened is decided in
// the test the store opens next (integrations.CallRound.TrackerAhead): it
// reads the view in force, and walks and spends the worker scope there too,
// as does a call whose mark kept no view, and one whose consume the view in
// force refused while its own view still offers the recording (settled).
// One rule for the three, so one worker recording never answers two calls
// in the windows, wherever a test's begin lands between a call's mark and
// its read.
func (h *heldScope) readView() *exactView {
	if h.g.readIn != nil {
		return h.g.readIn
	}
	return h.exactView()
}

// IsTestWindowActive implements integrations.TestWindows' question for h's
// call: whether a test was open in the view its reads are made in
// (readView), which tells a parser that routes by it which tier serves it.
func (h *heldScope) IsTestWindowActive() bool {
	if v := h.readView(); v != nil {
		return v.test != ""
	}
	return h.MockManager.IsTestWindowActive()
}

// Variables implements integrations.VariableStore for h's call: the table of
// the variable scope the view its call was read in was stamped with
// (exactView.scope, variables.Table.In) while it reads there (readIn), as
// when keploy decides the call at once. While that scope runs, the table in
// force; once the replay began the next test, the call's own test's scope
// as it ended, which the variable pass checks the call in and binds what it
// binds in, and the own-call check and the miss read: never the next test's
// bindings, which a call of the test before neither reads nor binds in. The
// table in force for a call that reads the view in force (readView), or one
// read in a view stamped with no scope.
func (h *heldScope) Variables() *variables.Table {
	if v := h.g.readIn; v != nil && v.scoped {
		return h.vars.In(v.scope)
	}
	return h.MockManager.Variables()
}

// InAheadScope implements integrations.VariableScopes' question for h's
// call, read in the view it was read in (readIn) as Variables is: a call of
// the gap that the pool view answers with one of the next test's calls in
// flight binds in the table begun early for that test (MockManager.
// AheadVariables). The manager's for a call that reads the view in force.
// The table begun early is the manager's for every call (MockManager.
// AheadVariables): one decided after that test began has its consume of the
// pool view's copy refused in that test's view (a copy another view served,
// TestModeInfo.ServedIn), and is decided again in the view in force, where
// it binds in that test's scope (settled, Variables), as at once.
func (h *heldScope) InAheadScope(mk *models.Mock) bool {
	if v := h.g.readIn; v != nil {
		return v.answersAhead(mk)
	}
	return h.MockManager.InAheadScope(mk)
}

// restaged reports whether a read or a consume door of h found the staging
// moved since its call's mark.
func (h *heldScope) restaged() bool { return h.g.restaged }

// moved reports whether the staging moved since h's call's mark, h then
// restaged.
func (h *heldScope) moved() bool {
	if h.stagingEpoch.Load() == h.g.at.Epoch {
		return false
	}
	h.g.restaged = true
	h.g.refuse(restagedWhy)
	return true
}

// StagingGeneration, StagingGenerationByKind, Revision and RevisionByKind are
// the manager's while the staging is still h's call's after they were
// sampled, and integrations.RestagedGeneration once it moved
// (integrations.StagingGenerations): h reads nothing then, so the generation
// it reports must not be the one of the set staged after the call, or a
// parser that keyed what it read on it would keep that nothing for the next
// set and miss each later call of it. Each is sampled before the check, as a
// read is made before held's: a staging moves its epoch before it changes the
// trees or a generation, so a sample the staging is still the call's after
// is of the call's set. While h reads in a view other than the one in force
// (elsewhere) each is h's own (elsewhereGen): h's reads are not the view in
// force's, so a parser must neither answer the call from its index of the
// view in force, nor file what h read under that view's generation for the
// view's own calls to be answered from.
func (h *heldScope) StagingGeneration() uint64 {
	return h.generation(h.MockManager.StagingGeneration())
}

func (h *heldScope) StagingGenerationByKind(kind models.Kind) uint64 {
	return h.generation(h.MockManager.StagingGenerationByKind(kind))
}

func (h *heldScope) Revision() uint64 { return h.generation(h.MockManager.Revision()) }

func (h *heldScope) RevisionByKind(kind models.Kind) uint64 {
	return h.generation(h.MockManager.RevisionByKind(kind))
}

// generation is g, sampled before the call, while the staging is still h's
// call's and h reads in the view in force; integrations.RestagedGeneration
// once the staging moved; h's own while it reads in another view.
func (h *heldScope) generation(g uint64) uint64 {
	if h.moved() {
		return integrations.RestagedGeneration
	}
	if h.elsewhere() {
		return h.elsewhereGen()
	}
	return g
}

// elsewhere reports whether h reads in a view other than the one in force:
// the view its call was read in (readIn), or the round snapshot's its call
// was marked from (viewGone), which a test's begin or end has since
// replaced.
func (h *heldScope) elsewhere() bool {
	return (h.g.readIn != nil && h.g.readIn != h.exactView()) || h.viewGone()
}

// viewGone reports whether h reads for the miss of a call marked from a
// round snapshot (diagnosing) whose view is no longer in force: a test's
// begin or end installed another since. h reads nothing then, checked after
// each read, as a staging's move is (held), so no text the miss prints (its
// closest recording, its diff) is of another view than the one its learn
// step read. Views are installed new and never again, so one still in force
// after a read was in force for all of it.
func (h *heldScope) viewGone() bool {
	return h.diagnosing && h.exactView() != h.heldIn
}

// elsewhereStride parts the generations held stores report while they read
// elsewhere: wider than any version a view over the store adds to them
// (scopedMockDb.viewVersion), so no two stores' meet.
const elsewhereStride = 1 << 20

// elsewhereGens counts the held stores that read elsewhere.
var elsewhereGens atomic.Uint64

// elsewhereGen is h's own generation while it reads elsewhere
// (integrations.ElsewhereGeneration): the same at each sample, and no other
// store's.
func (h *heldScope) elsewhereGen() uint64 {
	if h.ownGen == 0 {
		h.ownGen = integrations.ElsewhereGeneration + elsewhereGens.Add(1)*elsewhereStride
	}
	return h.ownGen
}

var _ integrations.StagingGenerations = (*heldScope)(nil)

// held is mocks when the staging is still h's call's after they were read;
// none once it moved, nor once the view a round snapshot's call is
// diagnosed in was replaced (viewGone).
func (h *heldScope) held(mocks []*models.Mock, err error) ([]*models.Mock, error) {
	if h.moved() || h.viewGone() {
		return nil, nil
	}
	return h.unhidden(mocks), err
}

// heldOf is held for a read that returns no error.
func (h *heldScope) heldOf(mocks []*models.Mock) []*models.Mock {
	out, _ := h.held(mocks, nil)
	return out
}

// ServingFor implements integrations.ServeTeller: h, whose consume doors
// also tell the served observer that what they serve is served in the
// request f. h is one call's, so it is told in place.
func (h *heldScope) ServingFor(f *learn.Flight) integrations.MockMemDb {
	h.g.in = f
	return h
}

func (h *heldScope) GetFilteredMocks() ([]*models.Mock, error) {
	return h.held(h.getFilteredMocksIn(h.readView()))
}

func (h *heldScope) GetFilteredMocksInWindow() ([]*models.Mock, error) {
	return h.held(h.getFilteredMocksInWindowIn(h.readView()))
}

func (h *heldScope) GetUnFilteredMocks() ([]*models.Mock, error) {
	return h.held(h.getUnFilteredMocksIn(h.readView()))
}

func (h *heldScope) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	return h.held(h.getPerTestMocksInWindowIn(h.readView()))
}

// RangePerTestMocksInWindow is the manager's per-test walk
// (integrations.PerTestRanger), each mock checked after it is read: the walk
// stops once the staging moved, or the call's view was replaced (viewGone).
func (h *heldScope) RangePerTestMocksInWindow(fn func(*models.Mock) bool) error {
	if h.moved() {
		return nil
	}
	return h.rangePerTestMocksInWindowIn(h.readView(), func(mk *models.Mock) bool {
		return !h.moved() && !h.viewGone() && (h.hides(mk) || fn(mk))
	})
}

func (h *heldScope) GetSessionMocks() ([]*models.Mock, error) {
	return h.held(h.getSessionMocksIn(h.readView()))
}

func (h *heldScope) GetSessionMocksInWindow(start, end time.Time) ([]*models.Mock, error) {
	return h.held(h.getSessionMocksInWindowIn(h.readView(), start, end))
}

func (h *heldScope) GetSessionScopedMocks() ([]*models.Mock, error) {
	return h.held(h.getSessionScopedMocksIn(h.readView()))
}

func (h *heldScope) GetStartupMocks() ([]*models.Mock, error) {
	return h.held(h.getStartupMocksIn(h.readView()))
}

func (h *heldScope) GetStartupMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	return h.held(h.getStartupMocksByKindIn(h.readView(), kind))
}

// RangeSessionMocksWithKey is the manager's keyed walk of the session tier
// (integrations.SessionKeyReader), each mock checked after it is read: the
// walk stops once the staging moved, or the call's view was replaced
// (viewGone).
func (h *heldScope) RangeSessionMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	if h.moved() {
		return nil
	}
	return h.rangeSessionMocksWithKeyIn(h.readView(), ix, key, func(mk *models.Mock) bool {
		return !h.moved() && !h.viewGone() && (h.hides(mk) || fn(mk))
	})
}

func (h *heldScope) GetConnectionMocks(connID string) ([]*models.Mock, error) {
	return h.held(h.getConnectionMocksIn(h.readView(), connID))
}

func (h *heldScope) GetFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	return h.held(h.getFilteredMocksByKindIn(h.readView(), kind))
}

func (h *heldScope) GetUnFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	return h.held(h.getUnFilteredMocksByKindIn(h.readView(), kind))
}

func (h *heldScope) GetCarryOverMocks() ([]*models.Mock, error) {
	return h.held(h.getCarryOverMocksIn(h.readView()))
}

func (h *heldScope) GetCarryOverMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	return h.held(h.getCarryOverMocksByKindIn(h.readView(), kind))
}

func (h *heldScope) Background(kind models.Kind) []*models.Mock {
	return h.heldOf(h.MockManager.Background(kind))
}

func (h *heldScope) SpentBackground(kind models.Kind) []*models.Mock {
	return h.heldOf(h.MockManager.SpentBackground(kind))
}

// KindBackground is the manager's, what h hides kept: no parser reads it
// from a call's held store (answering by kind reads the round,
// integrations.SnapshotRound), and SpendByKind's door refuses a recording h
// hides all the same.
func (h *heldScope) KindBackground(kind models.Kind) (unused, spent []*models.Mock) {
	unused, spent = h.MockManager.KindBackground(kind)
	if h.moved() || h.viewGone() {
		return nil, nil
	}
	return unused, spent
}

func (h *heldScope) PairingCandidates(kind models.Kind) []*models.Mock {
	return h.heldOf(h.MockManager.PairingCandidates(kind))
}

// ConsumedTestMocks implements integrations.ExactConsumedReader for h's
// call: the test mocks that have answered in the view h's reads are made in
// (readView), in the order they did, as its other reads are. So a call read
// in a test's view is told that test's answered recordings, which none of
// its reads shows any more, even once the test ended before it asks; never
// those of the view in force since. None once the staging moved.
func (h *heldScope) ConsumedTestMocks() []*models.Mock {
	return h.heldOf(consumedTestMocksIn(h.readView()))
}

// AnswerRepeatRead is the manager's (integrations.RepeatReadAnswerer) while
// the staging is still h's call's: a call of a set the replay staged another
// under is answered by no recording of it. The check and the answer are one
// step under the round lock, which a staging holds as it moves its epoch
// (bumpStagingEpoch), as a by-kind spend's are (spendByKindAt): a staging
// lands before the check, and the call is refused, or after the answer.
func (h *heldScope) AnswerRepeatRead(matched []*models.Mock) (integrations.RepeatReadAnswer, bool) {
	m := h.MockManager
	m.lockRound()
	if ok, _ := m.guardLocked(&h.g, nil); !ok {
		return integrations.RepeatReadAnswer{}, false
	}
	defer m.exact.round.mu.Unlock()
	if f := m.exact.round.onAnswerAgain; f != nil {
		f()
	}
	return m.AnswerRepeatRead(matched)
}

// CallViewMoved implements integrations.CallViewTeller: whether h's call
// reads the view in force (readView: one marked from a round snapshot, one
// decided in the test the store opens next,
// integrations.CallRound.TrackerAhead, or one whose consume the view in force
// refused, settled) and that view is not the one the call was read in
// (heldIn: its mark's, its round snapshot's, or the one in force as it was
// held), so two of its reads may have been made in two views. A call that
// still reads the view it was read in (readIn) makes every read there: false.
func (h *heldScope) CallViewMoved() bool {
	return h.g.readIn == nil && h.exactView() != h.heldIn
}

var _ integrations.CallViewTeller = (*heldScope)(nil)

func (h *heldScope) SkippedReadsAnswered() []*models.Mock {
	return h.heldOf(h.MockManager.SkippedReadsAnswered())
}

// FindAnsweredEarlier is the manager's (integrations.AnsweredFinder) while
// the staging is still h's call's after the lookup: the answered log is the
// staged set's, and recordings' names restart in each set, so a call of one
// set is never named a repeat of a recording another set staged under it
// answered.
func (h *heldScope) FindAnsweredEarlier(kind models.Kind, keys []string, match func(*models.Mock) bool) (integrations.AnsweredCall, bool) {
	a, ok := h.MockManager.FindAnsweredEarlier(kind, keys, match)
	if h.moved() {
		return integrations.AnsweredCall{}, false
	}
	return a, ok
}

// AnsweredRedacting is the manager's (integrations.AnsweredRedactingReader)
// while the staging is still h's call's after the read; once another set was
// staged under the call, the log is that set's, and the call's recordings are
// not known: none, and not complete, so the miss is redacted by every place.
// So too for the miss of a call marked from a round snapshot once its view
// was replaced (viewGone): h then reads none of the pool its places are
// summed from.
func (h *heldScope) AnsweredRedacting(kind models.Kind) ([]*models.Mock, bool) {
	mocks, complete := h.MockManager.AnsweredRedacting(kind)
	if h.moved() || h.viewGone() {
		return nil, false
	}
	return mocks, complete
}

func (h *heldScope) ExactTakenCandidates(kind models.Kind) []*models.Mock {
	return h.heldOf(h.MockManager.ExactTakenCandidates(kind))
}

func (h *heldScope) SpendByKind(mk *models.Mock) bool {
	if h.refuses(mk) {
		return false
	}
	ok, _ := h.spendByKindAt(mk, &h.g)
	return ok
}

func (h *heldScope) ClaimPairing(mk *models.Mock) bool {
	ok, _ := h.claimPairingAt(mk, &h.g)
	return ok
}

// LearnElsewhereCandidates is the manager's while the store is still at h's
// round; LearnElsewhere keeps what a call taught only while it is
// (learnElsewhereAt): a recording of a later staging that shares a name is
// taught nothing by a call of h's.
func (h *heldScope) LearnElsewhereCandidates(kind models.Kind, keys []string) []*models.Mock {
	return h.heldOf(h.MockManager.LearnElsewhereCandidates(kind, keys))
}

func (h *heldScope) LearnElsewhere(mk *models.Mock, clocks map[string]models.ClockOffset) bool {
	return h.learnElsewhereAt(mk, clocks, &h.g, &h.read)
}
