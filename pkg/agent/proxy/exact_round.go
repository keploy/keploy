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
// it, as one with no guard is. restaged: a staging guard found the staging
// moved.
type roundGuard struct {
	at       integrations.RoundAt
	moved    string
	in       *learn.Flight
	free     bool
	staging  bool
	restaged bool
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
		if m.stagingEpoch.Load() == g.at.Epoch {
			return true, nil
		}
		m.exact.round.mu.Unlock()
		g.restaged = true
		g.refuse(restagedWhy)
		return false, nil
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
	var teardown []*models.Mock
	if v.afterTests(s.Fired) {
		teardown = m.teardown(v.poolKey, startupVals)
	}
	s.PerTest = v.perTest("", filteredVals, teardown)
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
		ViewGen: v.round,
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

var (
	_ integrations.RoundReader = (*MockManager)(nil)
	_ integrations.RoundWaiter = (*MockManager)(nil)
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
	h := &heldScope{roundScope: roundScope{MockManager: m, g: roundGuard{at: integrations.RoundAt{Epoch: call.Epoch()}, staging: true}}}
	h.seq, h.seqOK = call.Sequence()
	return h, h.restaged
}

var (
	_ integrations.RoundHolder = (*MockManager)(nil)
	_ integrations.ServeTeller = (*heldScope)(nil)
	// A parser's per-generation index serves only a store that tells whose
	// a recording is (the HTTP strict index).
	_ integrations.TestOwner = (*heldScope)(nil)
)

// heldScope is the manager held to one call's staging (HoldToRound): its
// consume doors (roundScope's, and the background spend and claim) settle
// only while the staging is still the call's, and each read that serves or
// diagnoses a call finds nothing once the staging moved, checked after the
// read is made: a staging moves its epoch before it changes the trees or
// installs a view, so a read the staging is still the call's after was of
// the call's set. One call's: not for sharing between goroutines.
type heldScope struct {
	roundScope
	// seq is the sequence view of the view the call was read in
	// (integrations.CallRound.Sequence), seqOK whether its mark kept one.
	seq   integrations.SequenceView
	seqOK bool
	// owns is the call's decision whether it is one of the open test's own
	// calls (HideBackgroundWhen), until a door or HidesBackground asks it.
	owns func() bool
	// hideBg says the call is one of them (HidesBackground): the set's
	// background is in none of h's reads but KindBackground's, and its doors
	// refuse it (hides).
	hideBg bool
}

// SequenceView implements integrations.SequenceViewer: the sequence view of
// the view h's call was read in, so a call read in one view and walked after
// the next one is installed (a test opening or ending between the read and
// the walk) walks the view it was read in, its cursor and its window, never
// the next view's; with no view kept by the mark (exact serving began after
// it), the view in force now.
func (h *heldScope) SequenceView() (integrations.SequenceView, bool) {
	if h.seqOK {
		return h.seq, true
	}
	return h.MockManager.SequenceView()
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
// is of the call's set.
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
// call's; integrations.RestagedGeneration once it moved.
func (h *heldScope) generation(g uint64) uint64 {
	if h.moved() {
		return integrations.RestagedGeneration
	}
	return g
}

var _ integrations.StagingGenerations = (*heldScope)(nil)

// held is mocks when the staging is still h's call's after they were read;
// none once it moved.
func (h *heldScope) held(mocks []*models.Mock, err error) ([]*models.Mock, error) {
	if h.moved() {
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
	return h.held(h.MockManager.GetFilteredMocks())
}

func (h *heldScope) GetFilteredMocksInWindow() ([]*models.Mock, error) {
	return h.held(h.MockManager.GetFilteredMocksInWindow())
}

func (h *heldScope) GetUnFilteredMocks() ([]*models.Mock, error) {
	return h.held(h.MockManager.GetUnFilteredMocks())
}

func (h *heldScope) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	return h.held(h.MockManager.GetPerTestMocksInWindow())
}

// RangePerTestMocksInWindow is the manager's per-test walk
// (integrations.PerTestRanger), each mock checked after it is read: the walk
// stops once the staging moved.
func (h *heldScope) RangePerTestMocksInWindow(fn func(*models.Mock) bool) error {
	if h.moved() {
		return nil
	}
	return h.MockManager.RangePerTestMocksInWindow(func(mk *models.Mock) bool {
		return !h.moved() && (h.hides(mk) || fn(mk))
	})
}

func (h *heldScope) GetSessionMocks() ([]*models.Mock, error) {
	return h.held(h.MockManager.GetSessionMocks())
}

func (h *heldScope) GetSessionMocksInWindow(start, end time.Time) ([]*models.Mock, error) {
	return h.held(h.MockManager.GetSessionMocksInWindow(start, end))
}

func (h *heldScope) GetSessionScopedMocks() ([]*models.Mock, error) {
	return h.held(h.MockManager.GetSessionScopedMocks())
}

func (h *heldScope) GetStartupMocks() ([]*models.Mock, error) {
	return h.held(h.MockManager.GetStartupMocks())
}

func (h *heldScope) GetStartupMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	return h.held(h.MockManager.GetStartupMocksByKind(kind))
}

// RangeSessionMocksWithKey is the manager's keyed walk of the session tier
// (integrations.SessionKeyReader), each mock checked after it is read: the
// walk stops once the staging moved.
func (h *heldScope) RangeSessionMocksWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) error {
	if h.moved() {
		return nil
	}
	return h.MockManager.RangeSessionMocksWithKey(ix, key, func(mk *models.Mock) bool {
		return !h.moved() && (h.hides(mk) || fn(mk))
	})
}

func (h *heldScope) GetConnectionMocks(connID string) ([]*models.Mock, error) {
	return h.held(h.MockManager.GetConnectionMocks(connID))
}

func (h *heldScope) GetFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	return h.held(h.MockManager.GetFilteredMocksByKind(kind))
}

func (h *heldScope) GetUnFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	return h.held(h.MockManager.GetUnFilteredMocksByKind(kind))
}

func (h *heldScope) GetCarryOverMocks() ([]*models.Mock, error) {
	return h.held(h.MockManager.GetCarryOverMocks())
}

func (h *heldScope) GetCarryOverMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	return h.held(h.MockManager.GetCarryOverMocksByKind(kind))
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
	if h.moved() {
		return nil, nil
	}
	return unused, spent
}

func (h *heldScope) PairingCandidates(kind models.Kind) []*models.Mock {
	return h.heldOf(h.MockManager.PairingCandidates(kind))
}

func (h *heldScope) ConsumedTestMocks() []*models.Mock {
	return h.heldOf(h.MockManager.ConsumedTestMocks())
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
