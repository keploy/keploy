package proxy

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Exact serving: what a parser may answer a request with, under the proven
// switch (design v14 §5.2). It is the one path that decides what a
// parser sees there, whatever tier a mock was staged in and whatever lifetime
// its kind derives; the agent turns it on for a replay and moves it at every
// begin and end of a test.
//
//   - While a test is open, the test's own mocks (the data-plane mocks of its
//     mapping) answer, each at most one request, in recorded order, and the
//     pool answers only protocol-phase requests (handshakes, session
//     settings, DNS). One more request identical to one a test mock already
//     answered finds no test mock: a miss (models.MissExtraCall). Under the
//     proven switch no worker-scope mock (below) answers it either
//     (integrations.OwnRepeatRead); with the switch off, one the run has
//     reached that is that request does.
//   - While no test is open, the current key's pool slice answers, with
//     reuse or cursor-saturate as its mocks were recorded, beside the
//     protocol-phase mocks. Its stateful cursors are the key's own.
//   - Whether a test is open or not, the set's worker-scope mocks its run
//     has reached the place of answer (ServeExactWorker,
//     models.Mapping.Worker): no test owns them. In a test they come after
//     the test's own. Each answers at most one request in the staging,
//     consumed through a delete door as a data-plane recording is outside
//     every test (decision 31): no begin or end brings it back.
//
// A data-plane recording a parser consumes through a delete door while no
// test is open (a call before the first test or between tests) is consumed
// for the staging, not for the view (founder decision 31): no begin or end
// brings it back, and the next staging (stagingEpoch) does. The manager
// gives no last answer to repeat: which recordings are one call's sequence
// is the parser's to know (its matching), so a parser that has one
// saturates itself (HTTP's stateful cursor; SQS answers a drained
// ReceiveMessage with its empty answer), and an exhausted call of any other
// misses. A protocol-phase recording (a handshake, ZooKeeper's connect, an
// SQS config call) stays reusable as before: one consumed through a delete
// door is gone until the view changes. In a test, a consume through a
// delete door is gone for the rest of the test, as before.
//
// The staged trees keep every mock of the set. Each read a parser makes is
// narrowed to what the view shows, and the open test's mocks are read from
// the view itself, as per-test mocks (the manager's copies, consumed through
// whichever door a parser uses: DeleteFilteredMock, DeleteUnFilteredMock,
// DeleteStartupMock, UpdateUnFilteredMock or MarkMockAsUsed). A consume of a
// test mock that already answered, or of a mock the view does not show,
// fails, so two requests that read the same snapshot cannot both be answered
// by one test mock. Each change of what the view shows (a begin, an end, a
// test mock answering) bumps the revisions, so a parser that keeps an index
// of what it read reads again.

// exactView is what exact serving shows. It is replaced, never changed,
// except for which of its test mocks have answered and what its open test
// was served (mu).
type exactView struct {
	// test is the open test's ID; "" while no test is open.
	test string
	// gen is the test view's own number: each copy it serves carries it
	// (TestModeInfo.ServedIn), so a copy read in an earlier view, a previous
	// attempt of the same test, cannot answer for this one.
	gen uint64
	// owned are the open test's test mocks, the manager's own copies, in
	// recorded order; index is each one's position, by name. session marks
	// those staged in the session tier (their kind derives a reusable
	// lifetime): a parser that reads only that tier finds them there too.
	owned   []*models.Mock
	index   map[string]int
	session []bool
	// served is, on a pool view (no test open), the copy each owned mock
	// answered with, by position: what its consume says (noise, variables)
	// when it is flagged consumed, as the next test opens (ServeExactTest),
	// not when it answered, so the ended test's drain never reads it.
	served []*models.Mock
	// carried names, on a test view, its mocks the pool view before it
	// answered (ServeExactPool's ahead): the variables their answers bound
	// are this test's (MockManager.StartVariables).
	carried map[string]bool
	// inFlight names, on a test view, its mocks the pool view before it
	// served as its calls in flight when it began, answered or not: recorded
	// in the gap before its window, and its own (InFlightIntoOpenTest).
	inFlight map[string]bool
	// pool is, while no test is open, the names of the pool slice served.
	pool map[string]struct{}
	// worker is the set's worker-scope mocks the run has reached the place
	// of, by name, as ServeExactWorker last said when the view was
	// installed: shown whether a test is open or not.
	worker map[string]struct{}
	// poolKey is the key of that slice: its stateful cursors are its own.
	// winStart and winEnd are, on a test view, the test's recorded window
	// (the staging's recorded windows, by its name), where its sequences
	// start (SequenceView); zero for a test the replay sent none of.
	poolKey          string
	winStart, winEnd time.Time
	// cand marks, by position in owned, the recordings the open test shares
	// with the tests whose requests were open beside its own at record time
	// (ServeExactTestConcurrent): filed after every one of its own, each
	// answers at most once in the staging, for whichever of those tests
	// makes its call first (claims), and none is required of the test. nil
	// for a view with none.
	cand []bool
	// claims is the manager's claims on the shared recordings.
	claims *exactClaims
	// bg is the set's background under the background rules (ServeExactBackground)
	// when the view was installed; nil for today's rules.
	bg *backgroundState
	// staging is whether the view was installed while a set's mocks were
	// being staged (StageExactBackground until ServeExactBackground): a miss
	// it refuses says so (stagingMissNote).
	staging bool
	// workerOrder is, under the background rules, each shown worker-scope mock's
	// position in the order ServeExactWorker gave (its place's order, then
	// delivering before idle, then recorded order).
	workerOrder map[string]int

	mu sync.Mutex
	// closed: another view replaces this one; nothing consumes in it after
	// (closeAhead, installExact), so what it says it consumed is final.
	closed bool
	// replaced: the view that replaces this one is installed and the
	// revisions moved (installExact). A consume or a spend that finds this
	// view closed waits for it (closedWait), then settles against the view
	// in force: a view's close is never what refuses a call (settleExact).
	// replacedCh is closed once replaced is set; made by the first reader
	// that waits.
	replaced   bool
	replacedCh chan struct{}
	// consumed says which owned mock has answered; order lists their
	// positions in the order they did.
	consumed []bool
	order    []int
	// spent are the staged mocks this view shows that a parser consumed
	// through a delete door in its test, or a protocol-phase mock one
	// evicted outside every test: gone until the view changes. The staged
	// trees keep them. A data-plane recording consumed outside every test is
	// the staging's (outside), not the view's. Keyed by name; written under mu
	// (exactConsumeIn), read without it, so a read of a tier takes no lock
	// per mock (unspentNow).
	spent sync.Map
	// outside is the manager's outsideAnswers, and epoch its stagingEpoch.
	outside *outsideAnswers
	epoch   *atomic.Uint64
	// kept is the manager's kept learning (SetKeptLearning): each staged
	// recording the view hands out carries it (learned). nil on a view no
	// manager installed.
	kept *keptOverlay

	// round is the round's generation this view was installed at
	// (roundState.gen), set holding the round lock before the view is
	// visible. learnNumber is, on a test's view, the learn tracker's test
	// it is bound to (learnBound: the tracker's boundaries reach this
	// manager, LearnBoundary): a tracker whose open test is another is at a
	// boundary (integrations.RoundBoundary). Set once, holding the round
	// lock: at the install (roundState.bindLocked), or at the tracker's
	// begin after it.
	round       uint64
	learnNumber uint64
	learnBound  bool
	// shared is the view's SharedGen's View, which a test's view reports:
	// what it shows of the trees beyond the protocol-phase recordings, the
	// last view's number when that view's was the same (sharedViews.of).
	// Set holding the round lock before the view is visible.
	shared uint64

	// heard is every mock a parser was served while the view was installed,
	// as the served observer was told of it (observeServed): of any lifetime
	// (the test's own, a protocol-phase or reusable one, one spent through a
	// delete door, a worker-scope one), through any door, once each by
	// identity (learn.MockID, heardIDs), in the order they came. A consume
	// the view or the tier refused is not one (exactConsumeAt): nothing
	// served it. Kept only while there is an
	// observer (the proven switch), and read only of a test view
	// (ServedInOpenTest).
	heard    []*models.Mock
	heardIDs map[string]bool

	// keyed files, for each integrations.MockIndex a keyed read used
	// (RangeSessionMocksWithKey), the positions in owned of the copies the
	// index files under each key, in recorded order. owned never changes
	// once the view is installed, so each copy's keys are computed once per
	// view and index (ownedKeys), never on a call. Built on the first keyed
	// read of each index, under keyedMu, and never changed after: read
	// without the lock.
	keyedMu sync.Mutex
	keyed   map[*integrations.MockIndex]map[string][]int
	// live is, for each kind (every kind for "") and session filter a
	// per-test read used (unanswered, rangeUnanswered), and each MockIndex
	// and key a keyed read used (rangeUnansweredWithKey), the positions in
	// owned it reads, with links forward past the consumed ones
	// (liveList). Built on the first read of each, under mu.
	live map[liveKey]*liveList
	// shown is, for each tree, MockIndex and key a keyed read walked
	// (rangeShownKeyed), the entries of the tree's split under the key
	// (keyPart.rest) the view shows, at the tree's generation they were
	// filed at: rebuilt only when that moves, never on a call. Under
	// shownMu.
	shownMu sync.Mutex
	shown   map[shownAt]*shownExtra
	// stamp is the learn tracker's open test (learn.Tracker.OpenEpoch) as
	// the test's view was installed (exactState.stampTest), set before the
	// view is published and never changed: 0 for a pool view, with no
	// tracker (SetOpenTestEpoch), when the tracker had no test open, and in
	// a tracker test another test's view was installed in too.
	stamp uint64
}

// hear files mk in v's account of what its open test was served (heard),
// once by identity (learn.MockID, the tracker's). A pool view files nothing:
// its account would be read by no one (ServedInOpenTest reads a test view's
// only), and the start-up's and the gaps' serves would each build an entry
// kept until the view is replaced. A replaced view's account is read by no
// one either; a serve read in it before it was replaced is filed there.
func (v *exactView) hear(mk *models.Mock) {
	if v == nil || v.test == "" {
		return
	}
	id := learn.MockID(mk)
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.heardIDs[id] {
		return
	}
	if v.heardIDs == nil {
		v.heardIDs = map[string]bool{}
	}
	v.heardIDs[id] = true
	v.heard = append(v.heard, mk)
}

// exactState is a manager's exact serving: off until the agent turns it on
// for a replay session (view nil), off again at the next one.
type exactState struct {
	view atomic.Pointer[exactView]
	// beforePublish, set only by tests and before the install it is to see,
	// is called with each view as installExact is about to publish it in
	// view: it sees the view as a reader loading view first will.
	beforePublish func(*exactView)
	// afterPublish, set only by tests and before the install it is to see,
	// is called with each view as installExact's last step before it
	// returns: the view published, the view before it closed, every kind's
	// revision and the window signal moved. It sees what a reader the signal
	// woke reads.
	// It runs with no view's lock held; on a test's view the install lock is
	// held, and on the open doors the variables' lock too.
	afterPublish func(*exactView)
	// aheadLocked, set only by tests and before the install it is to see, is
	// called with the pool view closeAhead closes and reads, at the end of
	// the hold of that view's lock in which it does both. The lock is still
	// held, so it must not take it, nor read or consume through the manager
	// (each takes the view's lock). No install lock is held.
	aheadLocked func(*exactView)
	// aheadRead, set only by tests and before the install it is to see, is
	// called with the pool view closeAhead closed and read, as closeAhead
	// returns: the view's lock released, before the door builds its view
	// from what it read. It runs with no view's lock and no install lock
	// held.
	aheadRead func(*exactView)
	// poolNames are the names the replay's scope table puts in a pool slice,
	// of every key (SetPoolNames): nil until a replay sent one.
	poolNames atomic.Pointer[map[string]struct{}]
	// poolOwners is, of the same scope table, the key of the test process
	// whose tests list each mock a test lists (SetPoolOwners), "" where no
	// one key is known: what bounds each test process's teardown
	// (teardownAfter). nil until a replay sent one.
	poolOwners atomic.Pointer[map[string]string]
	// teardownLast is keploy mock replay's teardown bound (teardownAfter),
	// cached for one staging generation of the trees (StagingGeneration: an
	// exact serve does not move it), one scope table, one recording's times
	// and one key.
	teardownLast atomic.Pointer[teardownBound]
	// times is where the teardown bound reads the times of the set's
	// recordings the trees do not hold (SetRecordingTimes): nil until the
	// agent set one.
	times atomic.Pointer[recordingTimes]
	// fired: a test has begun since exact serving was turned on. It is what
	// tells a tier-routing parser (Postgres v3) "between tests" from
	// "before the first test".
	fired atomic.Bool
	// last is the latest test view while it is in force: its outcome is
	// read after the test stops being served (ExactTestOutcome). Once
	// another view replaces it, its outcome is kept by name (ended), and
	// the view goes, with the recordings it holds: kept until the next
	// test's open, it held a closed test's recordings between tests.
	last atomic.Pointer[exactView]
	// ended is the outcome of the latest test view, kept as it was replaced.
	ended atomic.Pointer[testOutcome]
	// gen numbers the test views.
	gen atomic.Uint64
	// outside is what the calls made outside every test consumed in the
	// current staging (outsideAnswers).
	outside outsideAnswers
	// worker is the worker scope's reachable mocks (ServeExactWorker), which
	// each view installed after it shows, with their order in it: one value,
	// so a view never pairs one call's set with another's order.
	worker atomic.Pointer[reachedWorker]
	// bg is the staged set's background under the background rules
	// (ServeExactBackground), nil for today's (exact_background.go).
	bg atomic.Pointer[backgroundState]
	// shared numbers the test views' shared parts (exactView.shared); held
	// under the round lock.
	shared sharedViews
	// staging is set while a set's mocks are staged: from its staging's
	// StageExactBackground to its ServeExactBackground (exact_background.go).
	// A view installed meanwhile is a staging view (exactView.staging).
	staging atomic.Bool
	// poolCursor is the key of the pool view installed last: the cursors
	// the calls between tests advance are under it (cursorSlotIn), and so
	// are the background's (BackgroundCursor) and, in a test, the worker
	// scope's (SequenceView.Between).
	poolCursor atomic.Pointer[string]
	// pairs says the session pairs (SetPairing); claimed, the recordings a
	// pairing claimed in the current staging; ofKind, where the pairing
	// reads recordings of a kind the trees do not hold.
	pairs atomic.Bool
	// learnEnded says the set's learn step ended (SetLearnEnded): a retry
	// cycle of record's full round, served and judged, learns nothing and
	// pairs nothing.
	learnEnded atomic.Bool
	// learnHeld says the learn step is held while a test whose recording
	// the round does not learn onto is open (SetLearnHeld): it pairs nothing
	// and learns nothing until the hold is released.
	learnHeld atomic.Bool
	// kept is what keploy record keeps of the set's first-cycle learning,
	// put on the staged recordings its retry cycles are served
	// (SetKeptLearning).
	kept    keptOverlay
	claimed stagingNames
	ofKind  recordedOfKindSource
	// begun is the mocks the tests begun in the current staging own
	// (StagedOwnerBegun); named, those a miss's diagnosis named
	// (FindStagedNotOwned), each once.
	begun, named stagingNames
	// onMarked runs right after a call's round is marked (MarkCallRound),
	// onNaming right before a lookup checks the staging and spends a name,
	// and onNamed right after it spent one (findNotOwned): seams a test
	// changes the round at.
	onMarked, onNaming, onNamed func()
	// recorded is where a miss's diagnosis reads the set's recordings the
	// trees do not hold (SetRecordingSource).
	recorded recordingSource
	// before is where RecordedBefore reads what the trees do not hold.
	before recordedBeforeSource
	// claims are the shared recordings the tests of the current staging
	// answered with (exactClaims).
	claims exactClaims
	// round is what exact serving's round changes holding its one lock
	// (exact_round.go): the generation, the take log, the learn tracker's
	// boundaries.
	round roundState
	// beforeInstall, beforeBump and onClosedWait are test seams, nil outside
	// tests: beforeInstall runs after a view is closed (closeAhead) and
	// before its replacement is installed; beforeBump once installExact has
	// swapped the new view in and closed the one it replaced (prev), before
	// the revisions move; onClosedWait as a reader starts to wait on a closed
	// view (closedWait). A test drives the steps' interleaving with them,
	// never with sleeps.
	beforeInstall func()
	beforeBump    func(prev *exactView)
	onClosedWait  func()

	// installMu orders the installs of tests' views (serveExactTest): each
	// reads the learn tracker's open test, stamps its view (stampTest) and
	// makes it the view served under it, so the stamps' order is the order
	// the views start serving in, and no other test's view is installed
	// between an install's read of the tracker and its view's start.
	installMu sync.Mutex
	// stamped is, under installMu, the learn tracker's open test the latest
	// test's view was installed in, and which tests' views were (stampTest).
	stamped *stampedTest
}

// stampedTest is, of the learn tracker's open test epoch, the test whose
// view was installed last in it, and whether another test's view was too.
type stampedTest struct {
	epoch uint64
	test  string
	mixed bool
}

// stampTest is the stamp of test's view as it is installed (exactView.stamp),
// called under installMu and the round lock: the learn tracker's open test (open,
// learn.Tracker.OpenEpoch; nil when the manager has none), so a reader
// compares the view's test with the tracker's by number. It is 0 when there
// is no tracker, when no test is open in it (the view was installed before
// the tracker's test began, or after it ended), and once two tests' views
// were installed in one open tracker test, for every view installed in it
// after that: the tracker began no test for the second, and files the serves
// of both in the one test. A test's view installed again with no other
// test's between keeps the stamp. The reads are made in installMu's order and
// the tracker's numbers only grow, so a read is never older than the stamp
// before it, unless it is of no open test (0).
// What the stamp does not see is a tracker test that begins while a test's
// view is served: the tracker files that view's serves in the test until the
// next view is installed. The replay begins each test case only over the
// pool (replay poolBeforeBegin).
func (e *exactState) stampTest(open *func() uint64, test string) uint64 {
	if open == nil {
		return 0
	}
	n := (*open)()
	next := &stampedTest{epoch: n, test: test}
	if prev := e.stamped; prev != nil && prev.epoch == n {
		next.mixed = prev.mixed || prev.test != test
	}
	e.stamped = next
	if next.mixed {
		return 0
	}
	return n
}

// exactClaims are, in one staging (stagingEpoch), the shared recordings
// (models.ExactServing.Concurrent) the tests answered calls with, each by the
// test that did: a shared recording answers at most once in the staging,
// whichever of the tests it is shared with makes its call first. A staging
// after it starts afresh.
type exactClaims struct {
	mu    sync.Mutex
	epoch uint64
	by    map[string]string
}

// in is the claims of staging epoch, with mu held: emptied when they were
// another staging's.
func (c *exactClaims) in(epoch uint64) map[string]string {
	if c.by == nil || c.epoch != epoch {
		c.epoch, c.by = epoch, map[string]string{}
	}
	return c.by
}

// claimant is the test that answered with name in staging epoch, if one did.
func (c *exactClaims) claimant(epoch uint64, name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != epoch {
		return "", false
	}
	t, ok := c.by[name]
	return t, ok
}

// claim gives name to test in staging epoch, and reports whether it is test's
// now: it was no test's, or test's already.
func (c *exactClaims) claim(epoch uint64, name, test string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	by := c.in(epoch)
	if t, ok := by[name]; ok {
		return t == test
	}
	by[name] = test
	return true
}

// release gives back every shared recording test answered with in staging
// epoch: a test opened again (a retry) starts afresh, as with its own.
func (c *exactClaims) release(epoch uint64, test string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	by := c.in(epoch)
	for n, t := range by {
		if t == test {
			delete(by, n)
		}
	}
}

// takenByOther reports whether name was claimed in staging epoch by a test
// other than test.
func (c *exactClaims) takenByOther(epoch uint64, name, test string) bool {
	if c == nil {
		return false
	}
	t, ok := c.claimant(epoch, name)
	return ok && t != test
}

// reachedWorker is what ServeExactWorker last said: the worker-scope mocks
// the run has reached, and each one's position in the order it gave.
type reachedWorker struct {
	set   map[string]struct{}
	order map[string]int
}

// stagingNames is a set of mock names that belongs to one staging
// (stagingEpoch): a staging after it starts afresh.
type stagingNames struct {
	mu    sync.Mutex
	epoch uint64
	names map[string]struct{}
}

// in is the set of staging epoch, with mu held: emptied when it was another
// staging's.
func (s *stagingNames) in(epoch uint64) map[string]struct{} {
	if s.names == nil || s.epoch != epoch {
		s.epoch, s.names = epoch, map[string]struct{}{}
	}
	return s.names
}

// add puts the named mocks of mocks in the set of staging epoch.
func (s *stagingNames) add(epoch uint64, mocks []*models.Mock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.in(epoch)
	for _, mk := range mocks {
		if mk != nil && mk.Name != "" {
			set[mk.Name] = struct{}{}
		}
	}
}

// claimIn is claim for a staging the store may have left: holding the set's
// lock, it claims name in the set of staging epoch only while now (the
// store's staging) is still epoch, and otherwise claims nothing (moved). A
// staging after epoch claims under the lock only once it began (now moved
// first), so no claim of a staging left behind empties the set of the one
// in force.
func (s *stagingNames) claimIn(epoch uint64, now func() uint64, name string) (claimed, moved bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now() != epoch {
		return false, true
	}
	set := s.in(epoch)
	if _, ok := set[name]; ok {
		return false, false
	}
	set[name] = struct{}{}
	return true, false
}

// claim puts name in the set of staging epoch, reporting whether it was not
// there yet.
func (s *stagingNames) claim(epoch uint64, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.in(epoch)
	if _, ok := set[name]; ok {
		return false
	}
	set[name] = struct{}{}
	return true
}

// has reports whether name is in the set of staging epoch.
func (s *stagingNames) has(epoch uint64, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.names[name]
	return ok && s.epoch == epoch
}

// StagedOwnerBegun reports whether name, a staged mock, is owned by a test
// that has begun in the current staging (integrations.BegunOwnerReader): a
// call made after its test began, where that test's view no longer shows it,
// is the recording made later than recorded. A poll whose answer depends on
// what a test caused (a queue's message) is told so from one made before the
// test that caused it, which rightly finds nothing.
func (m *MockManager) StagedOwnerBegun(name string) bool {
	return m.exact.begun.has(m.stagingEpoch.Load(), name)
}

var _ integrations.BegunOwnerReader = (*MockManager)(nil)

// outsideAnswers is the staged data-plane recordings a parser consumed
// through a delete door while no test was open, in one staging
// (stagingEpoch): hidden from every read of exact serving until the next
// staging.
type outsideAnswers struct {
	// mu serializes building cur; reads and spends need no lock. It is taken
	// under swapMu, never the other way round.
	mu  sync.Mutex
	cur atomic.Pointer[outsideState]
}

// outsideState is outsideAnswers for one staging.
type outsideState struct {
	epoch uint64
	// staged is, by name, every staged recording of the staging, read under
	// the staging lock: a consume is checked against it.
	staged map[string]*models.Mock
	// spent holds the names consumed: the first consume of one wins.
	spent sync.Map
	// unstaged holds the names of the recordings a door named that staged
	// does not hold, once each is said (MockManager.servedAsNamed).
	unstaged sync.Map
}

// hidden reports whether name was consumed outside every test in the staging
// epoch is.
func (o *outsideAnswers) hidden(name string, epoch uint64) bool {
	st := o.cur.Load()
	if st == nil || st.epoch != epoch {
		return false
	}
	_, spent := st.spent.Load(name)
	return spent
}

// prepareOutside builds the outside answers of the current staging, once: the
// staged recordings by name, read with the staging lock held, so no staging
// is ever half read. It runs before any view's lock is taken.
func (m *MockManager) prepareOutside() {
	m.swapMu.RLock()
	defer m.swapMu.RUnlock()
	o := &m.exact.outside
	o.mu.Lock()
	defer o.mu.Unlock()
	epoch := m.stagingEpoch.Load()
	if st := o.cur.Load(); st != nil && st.epoch == epoch {
		return
	}
	st := &outsideState{epoch: epoch, staged: map[string]*models.Mock{}}
	m.treesMu.RLock()
	trees := []*TreeDb{m.filtered, m.unfiltered, m.startup}
	m.treesMu.RUnlock()
	for _, tree := range trees {
		for _, mk := range m.treeValues(tree, "") {
			if _, dup := st.staged[mk.Name]; !dup {
				st.staged[mk.Name] = mk
			}
		}
	}
	o.cur.Store(st)
}

// RestartExactKey starts key's calls outside every test afresh: a test
// process that claims key again in the session (a retried package,
// --rerun-fails) is a new scope, as a test begun again is. The recordings of
// names, key's pool slice, that calls outside every test consumed in the
// staging answer again, and key's stateful cursors (cursorSlotIn) start
// over. No other key's are touched: pool slices name disjoint recordings.
func (m *MockManager) RestartExactKey(key string, names []string) {
	// One step of the round (exact_round.go): a snapshot reads each name
	// spent and in the take log, or answering again and given back there,
	// never shown and taken both.
	m.lockRound()
	if st := m.exact.outside.cur.Load(); st != nil {
		for _, n := range names {
			st.spent.Delete(n)
		}
	}
	m.restoreLocked(names)
	m.exact.round.mu.Unlock()
	// A carry-over mock of the pool the app consumed as it started is the
	// retried package's to consume again.
	m.forgetBootTaken(names)
	prefix := key + "\x00"
	m.cursorMu.Lock()
	for k := range m.cursors {
		if strings.HasPrefix(k, prefix) {
			delete(m.cursors, k)
		}
	}
	m.cursorMu.Unlock()
	// Parsers that keep an index of what they read read again.
	m.bumpRevisionAll()
}

// consumeOutside settles a delete-door consume of a data-plane recording mk
// while no test is open (decision 31), once prepareOutside has run. The
// first consume of a recording in the staging is served and spends it. A
// recording another call consumed since it was read is refused, and so is
// one that is not the staging's own (read before the staging landed, under
// a name the staging stages too or none it stages) and any consume while a
// staging lands: the parser reads again. It takes no tree lock.
func (m *MockManager) consumeOutside(mk *models.Mock) exactVerdict {
	st := m.exact.outside.cur.Load()
	if st == nil || st.epoch != m.stagingEpoch.Load() {
		return exactRefused
	}
	staged := st.staged[mk.Name]
	if staged == nil || staged.Kind != mk.Kind || !staged.Spec.ReqTimestampMock.Equal(mk.Spec.ReqTimestampMock) ||
		!staged.Spec.ResTimestampMock.Equal(mk.Spec.ResTimestampMock) {
		return exactRefused
	}
	if _, taken := st.spent.LoadOrStore(mk.Name, true); taken {
		return exactRefused
	}
	return exactServed
}

// exactView returns the view exact serving shows, nil when it is off.
func (m *MockManager) exactView() *exactView { return m.exact.view.Load() }

// ExactServing reports whether the manager serves each test exactly its own
// mocks (KEPLOY_PROVEN): every read shows one view, which changes as a
// test begins and ends, and a consume the view refuses returns false. A
// parser that keeps an index across reads (Postgres v3's tier engines) reads
// it at each dispatch: exact serving can begin after a connection opened.
func (m *MockManager) ExactServing() bool { return m.exactView() != nil }

var _ integrations.ExactServingReader = (*MockManager)(nil)

// MarkCallRound is, under exact serving, the call's round as it is now
// (integrations.StagedFinder): the staging and the view in force, read
// together under the round lock, in O(1). The view is kept, never copied: a
// view is not changed once installed, and tells its own by name later, for
// a miss's lookup, and whether it is a staging's (exactView.staging), for
// the miss to say so (integrations.StampMark), a test's (exactView.test),
// for the miss to be filed in it, and one of a set served by proven routing
// (exactView.bg). The zero CallRound without exact serving.
//
// Marked where the store no longer shows the view of the test the learn
// tracker still has open (roundState.endedLocked: its pool view, or another
// test's, installed before the tracker ended the test), the call may be that
// test's late one: the recordings that test's view held are held as the
// call's own too, as a round snapshot's CallRound holds them, so no lookup
// names one as made elsewhere (record's judging would bake a test's own
// recording into the worker scope), nor pairs the call with one as another
// test's. Marked where the tracker began a test whose view the store has not
// installed (integrations.RoundSnapshot.TrackerAhead), every recording is
// held: nothing tells which are that test's own. Marked where the tracker
// began a test set the store has not staged
// (integrations.RoundSnapshot.SetUnstaged), the mark is a staging's, outside
// every test, holding every recording.
//
// The mark carries the round's generation and boundary count, read under the
// round lock with the staging and the view (integrations.CallRound.Round): a
// learn step whose first read of the round comes after the mark tells from
// it whether the round moved between the two
// (integrations.CallRound.MovedSince), a test's end or the next one's
// begin, which no epoch tells.
func (m *MockManager) MarkCallRound() integrations.CallRound {
	r := &m.exact.round
	r.mu.Lock()
	v := m.exact.view.Load()
	at := integrations.RoundAt{Epoch: m.stagingEpoch.Load(), Gen: r.gen, Bounds: r.bounds}
	var ended *exactView
	ahead, unstaged := false, false
	if v != nil && r.learn.routed {
		ended = r.endedLocked(v, r.learn.number, r.learn.open)
		l := integrations.LearnRead{Bound: true, Number: r.learn.number, Open: r.learn.open, SetAhead: r.learn.set != r.stagedSet}
		phase, _, _ := m.phaseLocked(v, l)
		s := integrations.RoundSnapshot{Phase: phase, Learn: l, Ended: endedName(ended)}
		ahead, unstaged = s.TrackerAhead(), s.SetUnstaged()
	}
	r.mu.Unlock()
	if f := m.exact.onMarked; f != nil {
		f()
	}
	if v == nil {
		return integrations.CallRound{}
	}
	if unstaged {
		// The tracker began a test set the store has not staged
		// (integrations.RoundSnapshot.SetUnstaged): the call the staging
		// note describes, outside every test, its miss naming none made
		// elsewhere.
		return integrations.NewCallRound(at.Epoch, integrations.HoldsEvery).ReadAt(at).InStaging(true).InView(false, v.bg != nil).WithSequence(m.sequenceViewOf(v))
	}
	held := v.holds
	switch {
	case ahead:
		// The tracker began a test whose view the store has not
		// installed (integrations.RoundSnapshot.TrackerAhead): nothing
		// tells which recordings are that test's own, so every one is
		// held, and the call's miss names none made elsewhere.
		held = integrations.HoldsEvery
	case ended != nil:
		held = func(name string) bool { return v.holds(name) || ended.holds(name) }
	}
	return integrations.NewCallRound(at.Epoch, held).ReadAt(at).InStaging(v.staging).InView(v.test != "", v.bg != nil).WithSequence(m.sequenceViewOf(v))
}

// endedName is the test of v, the view the store ended while the learn
// tracker kept its test open (roundState.endedLocked), "" for none.
func endedName(v *exactView) string {
	if v == nil {
		return ""
	}
	return v.test
}

// holds reports whether the view holds the recording name as its own (the
// open test's, in flight into it, shared with it, a pool view's ahead): a
// name its index has. Read only once the view is installed, when it is
// never written again.
func (v *exactView) holds(name string) bool {
	_, own := v.index[name]
	return own
}

// FindStagedNotOwned is, under exact serving, the name of the first mock of
// kind staged in the manager's trees (per-test, session, startup, in that
// order) that the open view does not show, that no call of the set has
// consumed, that no earlier miss of the staging was named as, and that match
// accepts (integrations.StagedFinder); "" for none. Each recording is named
// for one miss at most, as it answers one call at most: two identical calls
// made elsewhere name two recordings of it, in recorded order.
// With a test open that is a mock the test does not own: another test's, or
// a worker-scope mock (models.Mapping.Worker) whose place the run has not
// reached. With none open, it is any test's. Such a miss is the call a
// recording elsewhere holds, made where it was not recorded, which record's
// judging reads to bake the recording into the worker scope (a2). For a
// miss's diagnosis only: the mock is never handed out, nor served.
//
// It is asked of call, the round the call was made in: a recording call's
// view held as its own is never named, nor any once the replay staged
// another set (restaged), checked right before a name is spent, which is
// spent in call's staging, and again before it is returned. The zero
// CallRound names nothing.
func (m *MockManager) FindStagedNotOwned(kind models.Kind, call integrations.CallRound, match func(*models.Mock) bool) (string, bool) {
	return m.findNotOwned(kind, call, nil, match)
}

// FindStagedNotOwnedByKeys is FindStagedNotOwned, looking also, when the
// trees hold no recording that is the call, in the recordings the agent keeps
// on disk of kind indexed under any of keys (models.RequestKeys): with the
// on-disk store a test's own are staged only as it opens, so a call made
// before its test would find none (integrations.StagedKeyFinder). Only those
// keys' recordings are decoded, on each such miss, and none is kept. No keys
// looks in the trees alone.
func (m *MockManager) FindStagedNotOwnedByKeys(kind models.Kind, call integrations.CallRound, keys []string, match func(*models.Mock) bool) (string, bool) {
	return m.findNotOwned(kind, call, keys, match)
}

var _ integrations.StagedKeyFinder = (*MockManager)(nil)

// requestKeyIndex files staged mocks under their request keys
// (models.RequestKeys), for a miss's diagnosis by keys (findNotOwned). A
// recording's keys are its parser's, of what it recorded.
var requestKeyIndex = &integrations.MockIndex{Keys: models.RequestKeys}

// notOwnedTried counts the staged mocks a miss's diagnosis took up
// (findNotOwned's try), each checked by name and then by the parser's match:
// what one costs, read by the rows that pin it.
var notOwnedTried atomic.Uint64

// findNotOwned is FindStagedNotOwned, and with keys FindStagedNotOwnedByKeys.
// The staging only moves on, and moves before the trees change (a staging
// bumps it first, holding swapMu), so a recording read from the trees while
// the staging is still call's is of call's set.
//
// With keys, of a kind whose parser files its recordings under request keys
// (models.HasRequestKeys), each tree hands only its recordings filed under
// one of keys (TreeDb.keyedValues), in tree order: a parser declares its keys
// so that every call its match accepts asks by a key the recording is under
// (models.RegisterRequestKeys), so the others are recordings match refuses.
// The tree's index is built once, on the first such miss after it is staged,
// so a miss costs what the call's keys file, not every recording of kind.
func (m *MockManager) findNotOwned(kind models.Kind, call integrations.CallRound, keys []string, match func(*models.Mock) bool) (string, bool) {
	v := m.exactView()
	if v == nil || match == nil || !call.Read() {
		return "", false
	}
	epoch := call.Epoch()
	moved := func() bool { return m.stagingEpoch.Load() != epoch }
	if moved() {
		return "", true
	}
	m.treesMu.RLock()
	trees := []*TreeDb{m.filtered, m.unfiltered, m.startup}
	m.treesMu.RUnlock()
	found, restaged := "", false
	seen := map[string]bool{}
	try := func(mk *models.Mock) bool {
		notOwnedTried.Add(1)
		if mk == nil || mk.Kind != kind || seen[mk.Name] {
			return false
		}
		seen[mk.Name] = true
		// The view's own: the open test's, or, in a pool view, the next
		// test's calls in flight (ServeExactPool's ahead), answered in the
		// gap as that test's and flagged consumed only once it opens. And
		// the call's first read's own: the replay ended the call's test
		// under it, whose recordings are no other test's for the call.
		if _, own := v.index[mk.Name]; own || call.Held(mk.Name) {
			return false
		}
		if v.shows(mk) || m.consumedInSet(mk.Name) || m.exact.named.has(epoch, mk.Name) || !match(mk) {
			return false
		}
		if f := m.exact.onNaming; f != nil {
			f()
		}
		claimed, stop := m.exact.named.claimIn(epoch, m.stagingEpoch.Load, mk.Name)
		if stop {
			restaged = true
			return true
		}
		if claimed {
			found = mk.Name
			if f := m.exact.onNamed; f != nil {
				f()
			}
			return true
		}
		return false
	}
	// Each tree's values are read first, and tried with no tree's lock held:
	// match is the parser's.
	narrow := len(keys) > 0 && models.HasRequestKeys(kind)
	for _, t := range trees {
		var values []*models.Mock
		if narrow {
			values = t.keyedValues(requestKeyIndex, keys, kind)
		} else {
			values = m.treeValues(t, kind)
		}
		for _, mk := range values {
			if found != "" || restaged {
				break
			}
			try(mk)
		}
	}
	if found == "" && !restaged && len(keys) > 0 {
		// The recordings kept on disk, in recorded order, each decoded only
		// once the checks by name have passed it, and none after the first
		// that is the call.
		m.rangeRecorded(kind, keys, func(name string, load func() (*models.Mock, error)) bool {
			if seen[name] || m.consumedInSet(name) || m.exact.named.has(epoch, name) || call.Held(name) {
				return true
			}
			if _, own := v.index[name]; own {
				return true
			}
			mk, err := load()
			if err != nil {
				if m.logger != nil {
					m.logger.Debug("a miss's diagnosis could not read a recording kept on disk; it names none of them", zap.String("mock", name), zap.Error(err))
				}
				return false
			}
			return !try(mk)
		})
	}
	if restaged || moved() {
		// Named in call's staging, if at all, and the replay staged the
		// next set before the miss is told: it is no longer call's round.
		return "", true
	}
	return found, false
}

// RecordingSource visits, of the test set staged, every recorded mock of kind
// indexed under any of keys (models.RequestKeys) that the agent keeps out of
// the manager's trees (its on-disk store of per-test mocks), once each, in
// recorded order, until visit returns false: its name, and load, which decodes
// it (DiskMocks.RangeKeys). For a miss's diagnosis only
// (FindStagedNotOwnedByKeys): what it reads is never served.
type RecordingSource func(kind models.Kind, keys []string, visit func(name string, load func() (*models.Mock, error)) bool) error

// recordingSource is the manager's RecordingSource for the current staging.
type recordingSource struct {
	mu  sync.Mutex
	src RecordingSource
}

// SetRecordingSource sets where a miss's diagnosis reads the staged set's
// recordings the trees do not hold (FindStagedNotOwnedByKeys); nil for none,
// when every one is in the trees. The agent sets it as it stages a set.
func (m *MockManager) SetRecordingSource(src RecordingSource) {
	m.exact.recorded.mu.Lock()
	defer m.exact.recorded.mu.Unlock()
	m.exact.recorded.src = src
}

// rangeRecorded visits what the recording source holds of kind under keys
// (RecordingSource): decoded only as visit loads it, kept by nobody; nothing
// when there is no source, or it fails.
func (m *MockManager) rangeRecorded(kind models.Kind, keys []string, visit func(name string, load func() (*models.Mock, error)) bool) {
	m.exact.recorded.mu.Lock()
	src := m.exact.recorded.src
	m.exact.recorded.mu.Unlock()
	if src == nil {
		return
	}
	if err := src(kind, keys, visit); err != nil && m.logger != nil {
		m.logger.Debug("a miss's diagnosis could not read the set's recordings kept on disk; it names none of them", zap.String("kind", string(kind)), zap.Error(err))
	}
}

// consumedInSet reports whether a call of the current test set consumed the
// mock named name (flagMockAsUsed's history, reset as a set is staged).
func (m *MockManager) consumedInSet(name string) bool {
	m.consumedMu.Lock()
	defer m.consumedMu.Unlock()
	_, ok := m.consumedPersistent[name]
	return ok
}

// recordedBeforeSource is where RangeRecordedBefore reads the staged set's
// recordings the trees do not hold, by request time (SetRecordedBefore).
type recordedBeforeSource struct {
	mu  sync.Mutex
	src func(t time.Time, visit func(*models.Mock) bool) error
}

// SetRecordedBefore sets where RangeRecordedBefore reads the staged set's
// recordings the trees do not hold (the agent's on-disk store of per-test
// mocks: DiskMocks.RangeRecordedBefore, which decodes one at a time); nil for
// none, when every one is in the trees. The agent sets it as it stages a
// set.
func (m *MockManager) SetRecordedBefore(src func(t time.Time, visit func(*models.Mock) bool) error) {
	m.exact.before.mu.Lock()
	defer m.exact.before.mu.Unlock()
	m.exact.before.src = src
}

// RangeRecordedBefore visits every recording of the staged set made before
// t, or at t's own instant, by request time, of any kind
// (integrations.RecordedBeforeReader): those
// in the manager's trees (StagedMocks), then those the agent keeps on disk
// (SetRecordedBefore), each once, until visit returns false. A recording a
// tree holds a copy of (the open test's own, loaded from disk) is visited
// from the tree. The disk's are visited as the source decodes them, one at a
// time: the manager keeps none of them, only which it visited.
func (m *MockManager) RangeRecordedBefore(t time.Time, visit func(*models.Mock) bool) error {
	seen := map[string]bool{}
	id := recordingID
	for _, mk := range m.StagedMocks() {
		if mk == nil || mk.Spec.ReqTimestampMock.After(t) || seen[id(mk)] {
			continue
		}
		seen[id(mk)] = true
		if !visit(mk) {
			return nil
		}
	}
	m.exact.before.mu.Lock()
	src := m.exact.before.src
	m.exact.before.mu.Unlock()
	if src == nil {
		return nil
	}
	return src(t, func(mk *models.Mock) bool {
		if mk == nil || mk.Spec.ReqTimestampMock.After(t) || seen[id(mk)] {
			return true
		}
		return visit(mk)
	})
}

var _ integrations.RecordedBeforeReader = (*MockManager)(nil)

// recordingID names a recording across the manager's trees and the disk:
// a tree's copy of one the disk keeps (the open test's own, loaded from
// disk) has the disk's name, kind and request time.
func recordingID(mk *models.Mock) string {
	return mk.Name + "\x00" + string(mk.Kind) + "\x00" + strconv.FormatInt(mk.Spec.ReqTimestampMock.UnixNano(), 10)
}

// rangeRecordedInOrder visits what RangeRecordedBefore visits, each once,
// in recorded order across the trees and the disk: the trees' recordings
// (StagedMocks: the open test's own, and the set's session and startup
// recordings, made through all of its tests) sorted by request time and
// merged into the disk's, which its source visits by request time. A
// recording of an earlier test is visited before any of a later test's,
// whichever store holds it. For the learn step's calls, which read every
// recording before them before they decide, the order is no matter, and
// RangeRecordedBefore visits the trees' first. For the set's staging
// (warmLearnSet), whose readers take recordings as they are visited, it
// is: visited trees first, the last test's session recordings were read
// before the first test's own on disk, so with few readers (an agent of
// two CPUs) a test could open before the recordings its calls read were.
func (m *MockManager) rangeRecordedInOrder(t time.Time, visit func(*models.Mock) bool) error {
	seen := map[string]bool{}
	var trees []*models.Mock
	for _, mk := range m.StagedMocks() {
		if mk == nil || mk.Spec.ReqTimestampMock.After(t) || seen[recordingID(mk)] {
			continue
		}
		seen[recordingID(mk)] = true
		trees = append(trees, mk)
	}
	sort.SliceStable(trees, func(i, j int) bool {
		return trees[i].Spec.ReqTimestampMock.Before(trees[j].Spec.ReqTimestampMock)
	})
	next, stopped := 0, false
	// upTo visits the trees' recordings made at or before at (every one
	// left, where all), until visit returns false.
	upTo := func(at time.Time, all bool) bool {
		for ; next < len(trees) && (all || !trees[next].Spec.ReqTimestampMock.After(at)); next++ {
			if !visit(trees[next]) {
				next++
				stopped = true
				return false
			}
		}
		return true
	}
	m.exact.before.mu.Lock()
	src := m.exact.before.src
	m.exact.before.mu.Unlock()
	if src != nil {
		err := src(t, func(mk *models.Mock) bool {
			if mk == nil || mk.Spec.ReqTimestampMock.After(t) {
				return true
			}
			if !upTo(mk.Spec.ReqTimestampMock, false) {
				return false
			}
			if seen[recordingID(mk)] {
				return true
			}
			if !visit(mk) {
				stopped = true
				return false
			}
			return true
		})
		if stopped {
			return err
		}
		// A disk that fails mid-read leaves the trees' recordings readable:
		// they are visited still, and the disk's error returned.
		upTo(time.Time{}, true)
		return err
	}
	upTo(time.Time{}, true)
	return nil
}

// StagedMocks is every mock staged in the manager's trees (per-test, session,
// startup, in that order), whatever the open view shows: exact serving spends
// a served mock without taking it out of its tree, so a recording served or
// not is here (integrations.StagedReader). For the learn step's earlier
// places only: the mocks are never handed out, nor served.
func (m *MockManager) StagedMocks() []*models.Mock {
	m.treesMu.RLock()
	trees := []*TreeDb{m.filtered, m.unfiltered, m.startup}
	m.treesMu.RUnlock()
	var out []*models.Mock
	for _, t := range trees {
		out = append(out, m.treeValues(t, "")...)
	}
	return out
}

var _ integrations.StagedReader = (*MockManager)(nil)

// RangeStagedRecordings implements integrations.StagedRecordingReader: the
// recordings of kind the trees hold (StagedMocks, of kind), then those of
// kind the agent keeps on disk under any of keys (SetRecordingSource), each
// decoded only once the trees showed no recording of its name. No keys
// visits the trees alone. Nothing visited is handed out, and no name is
// claimed (FindStagedNotOwned's naming is the miss's): a call that is
// exactly a recording is told so however many calls are.
func (m *MockManager) RangeStagedRecordings(kind models.Kind, keys []string, visit func(*models.Mock) bool) error {
	m.treesMu.RLock()
	trees := []*TreeDb{m.filtered, m.unfiltered, m.startup}
	m.treesMu.RUnlock()
	seen := map[string]bool{}
	for _, t := range trees {
		for _, mk := range m.treeValues(t, kind) {
			if mk == nil || mk.Kind != kind || seen[mk.Name] {
				continue
			}
			seen[mk.Name] = true
			if !visit(mk) {
				return nil
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	m.exact.recorded.mu.Lock()
	src := m.exact.recorded.src
	m.exact.recorded.mu.Unlock()
	if src == nil {
		return nil
	}
	var loadErr error
	err := src(kind, keys, func(name string, load func() (*models.Mock, error)) bool {
		if seen[name] {
			return true
		}
		seen[name] = true
		mk, err := load()
		if err != nil {
			loadErr = fmt.Errorf("read the recording %s kept on disk: %w", name, err)
			return false
		}
		return mk == nil || visit(mk)
	})
	if err != nil {
		return err
	}
	return loadErr
}

var _ integrations.StagedRecordingReader = (*MockManager)(nil)

// ServeExactTest opens test under exact serving: from now its test mocks,
// owned (the data-plane mocks of its mapping, in recorded order), each answer
// at most one request, and the pool answers only protocol-phase requests.
// The manager serves copies of them; owned is not written. A test opened
// again (a retry) starts with all its test mocks unanswered, but for those the
// pool view just before it answered as the test's mocks in flight when it
// began (ServeExactPool's ahead): those count as answered, first, in the order
// they did.
//
// It begins none of the test's variables: begun after, a call the test's own
// mock answers between the two binds in no scope of the test's
// (OpenExactTest).
func (m *MockManager) ServeExactTest(test string, owned []*models.Mock) {
	m.serveExactTest(test, owned, nil, false, nil)
}

// OpenExactTest opens test as ServeExactTest does, and begins its variables
// (StartVariables) with given bound, in the same step (variables.Table.
// StartOpening): no call its mocks answer binds before them. Opened in two
// steps, a call of a set's first test answered between them bound in the
// run's, which every later test of the set reads (NewVariableSet): another
// test's value answered as this one's, a false pass.
func (m *MockManager) OpenExactTest(test string, owned []*models.Mock, given []models.VariableBinding) {
	m.serveExactTest(test, owned, nil, true, given)
}

// GiveExactTest binds b, a value the open test gave, as a given variable of
// its scope (variables.Table.Give): test must be the test the view serves,
// and b's name not bound yet.
func (m *MockManager) GiveExactTest(test string, b models.VariableBinding) error {
	if v := m.exactView(); v == nil || v.test != test || test == "" {
		return fmt.Errorf("%s is not the test keploy serves now", test)
	}
	if have, ok := m.vars.Give(b); !ok {
		if have.Name == "" {
			return fmt.Errorf("no scope of %s's variables has begun", test)
		}
		return fmt.Errorf("variable %s is bound already in %s, to %q", b.Name, test, have.Value)
	}
	return nil
}

// ServeExactTestConcurrent is ServeExactTest for a test that shares
// recordings with the tests whose requests were open beside its own at
// record time (keploy record's full round, models.ExactServing.Concurrent):
// after its own test mocks, the view shows cands (in recorded order), each
// answering at most once in the staging, for whichever of those tests makes
// its call first. Each copy says it is shared (TestModeInfo.Concurrent), so a
// parser's learn step never learns against it. A
// shared recording another test answered with is not shown, and a consume of
// it is refused. None is required of the test: ExactTestOutcome and the
// mocks of the tests begun (StagedOwnerBegun) hold only its own. A shared
// recording owned names too is its own. A test opened again (a retry) gives
// back every shared recording it answered with, as it starts its own afresh.
func (m *MockManager) ServeExactTestConcurrent(test string, owned, cands []*models.Mock) {
	m.serveExactTest(test, owned, cands, false, nil)
}

// OpenExactTestConcurrent opens test as ServeExactTestConcurrent does, and
// begins its variables with given bound in the same step, as OpenExactTest
// does.
func (m *MockManager) OpenExactTestConcurrent(test string, owned, cands []*models.Mock, given []models.VariableBinding) {
	m.serveExactTest(test, owned, cands, true, given)
}

// serveExactTest is ServeExactTest and ServeExactTestConcurrent, and, with
// begin, OpenExactTest and OpenExactTestConcurrent.
func (m *MockManager) serveExactTest(test string, owned, cands []*models.Mock, begin bool, given []models.VariableBinding) {
	answered, ahead := m.closeAhead()
	if m.exact.beforeInstall != nil {
		m.exact.beforeInstall()
	}
	v := &exactView{test: test, gen: m.exact.gen.Add(1), index: make(map[string]int, len(owned)+len(cands))}
	if w, ok := m.testWindows.Load().Of(test); ok {
		v.winStart, v.winEnd = w.Start, w.End
	}
	v.addOwned(owned)
	if n := len(v.owned); len(cands) > 0 {
		v.addShared(cands)
		if len(v.owned) > n {
			v.cand = make([]bool, len(v.owned))
			for i := n; i < len(v.owned); i++ {
				v.cand[i] = true
			}
		}
	}
	v.claims = &m.exact.claims
	v.consumed = make([]bool, len(v.owned))
	for _, name := range ahead {
		if i, ok := v.index[name]; ok && !v.isCand(i) {
			if v.inFlight == nil {
				v.inFlight = map[string]bool{}
			}
			v.inFlight[name] = true
		}
	}
	for _, a := range answered {
		i, ok := v.index[a.owned.Name]
		if !ok || v.consumed[i] || v.isCand(i) {
			continue
		}
		v.consumed[i] = true
		v.order = append(v.order, i)
		if v.carried == nil {
			v.carried = map[string]bool{}
		}
		v.carried[a.owned.Name] = true
	}
	install := func() {
		// The install lock is taken here, on every door, so on the open
		// doors (OpenExactTest, OpenExactTestConcurrent) it is held under
		// the variables' lock; the round lock the install takes is taken
		// under it.
		m.exact.installMu.Lock()
		defer m.exact.installMu.Unlock()
		// One step with the swap, holding the round lock: no snapshot sees
		// the claims of a retried test released, what its earlier view
		// answered given back, its mocks begun, or its carried answers,
		// before the view that holds them.
		m.installExactWith(v, func(r *roundState) {
			// Stamped as it is installed: what the tracker's open test is
			// when the view starts serving, read holding the round lock
			// with the view's binding to the tracker's test
			// (roundState.bindLocked), so no snapshot reads the tracker
			// between the stamp's read and the view's start.
			v.stamp = m.exact.stampTest(m.openTestEpoch.Load(), test)
			epoch := m.stagingEpoch.Load()
			m.exact.claims.release(epoch, test)
			m.reopenLocked(v)
			m.exact.begun.add(epoch, owned)
			m.exact.fired.Store(true)
			m.exact.last.Store(v)
			m.settleAheadLocked(r, answered, v, func(name string) (bool, bool) { return v.carried[name], false })
		})
	}
	if begin {
		// What the test's calls in flight bound is read before the
		// variables' lock is taken: the scope they bound in reads the run's
		// of vars, and so locks vars first (variables.Table.ReadRunOf).
		m.vars.StartOpening(install, given, m.carriedInto(v))
		m.aheadVars.Reset()
	} else {
		install()
	}
	// Their consumes are flagged now, as this test's: the test before it was
	// drained since (the replay drains it after the pool call), and this
	// test's drain reads them. One this test does not own was answered for
	// a test that did not open next (one the replay could not open): it is
	// no consume of this test's, and is not flagged as one.
	for _, a := range answered {
		if v.carried[a.owned.Name] {
			m.noteExactServed(a.owned, a.served)
		} else if m.logger != nil {
			m.logger.Debug("a call answered in the gap as the next test's is not this test's, which opened instead",
				zap.String("mock", a.owned.Name), zap.String("test", test))
		}
	}
}

// aheadAnswer is a mock a pool view answered as the next test's (its ahead):
// the view's copy and the copy it answered with.
type aheadAnswer struct{ owned, served *models.Mock }

// closeAhead closes the view being served, if it is a pool view that answers
// the next test's mocks in flight when it began (ServeExactPool's ahead), and
// returns those that answered, in the order they did, and the names of all of
// them. It closes the view whatever of its ahead the view answered, all, some
// or none, in the same hold of its lock as the read, so no consume lands
// between the read and the next view: one that comes in between waits for the
// next view (closedWait), and is settled there.
func (m *MockManager) closeAhead() (answered []aheadAnswer, ahead []string) {
	// A close changes nothing a snapshot of the round reads (its answers are
	// final under the view's own lock, which every consume checks closed
	// under): what moves the round is the install that follows.
	prev := m.exact.view.Load()
	if prev == nil || prev.test != "" || len(prev.owned) == 0 {
		return nil, nil
	}
	// One hold, released by its defer even if a test's seam in it stops
	// the goroutine.
	func() {
		prev.mu.Lock()
		defer prev.mu.Unlock()
		prev.closed = true
		answered = make([]aheadAnswer, 0, len(prev.order))
		for _, i := range prev.order {
			answered = append(answered, aheadAnswer{owned: prev.owned[i], served: prev.served[i]})
		}
		ahead = make([]string, 0, len(prev.owned))
		for _, mk := range prev.owned {
			ahead = append(ahead, mk.Name)
		}
		if m.exact.aheadLocked != nil {
			m.exact.aheadLocked(prev)
		}
	}()
	if m.exact.aheadRead != nil {
		m.exact.aheadRead(prev)
	}
	return answered, ahead
}

// addOwned files owned into v as the mocks it answers, each at most once:
// the manager's copies, in owned's order.
func (v *exactView) addOwned(owned []*models.Mock) {
	v.add(owned, false)
}

// addShared files cands into v after its own, as the shared recordings it
// answers (exactView.cand): each copy says so (TestModeInfo.Concurrent). One v
// files already is left as it is.
func (v *exactView) addShared(cands []*models.Mock) {
	v.add(cands, true)
}

// isCand reports whether owned position i is a shared recording.
func (v *exactView) isCand(i int) bool {
	return i < len(v.cand) && v.cand[i]
}

// takenByOther reports whether owned position i is a shared recording another
// test answered with in the current staging.
func (v *exactView) takenByOther(i int) bool {
	return v.isCand(i) && v.epoch != nil && v.claims.takenByOther(v.epoch.Load(), v.owned[i].Name, v.test)
}

// add files mocks into v as the mocks it answers, each at most once: the
// manager's copies, in mocks' order, marked shared when shared is set.
func (v *exactView) add(owned []*models.Mock, shared bool) {
	for _, mk := range owned {
		if mk == nil || mk.Name == "" {
			continue
		}
		if _, dup := v.index[mk.Name]; dup {
			continue
		}
		v.session = append(v.session, mk.TestModeInfo.Lifetime == models.LifetimeSession)
		c := mk.ShallowCopy()
		// Served as a per-test mock, consumed once, whatever lifetime its
		// kind derives: every parser consumes a per-test mock through one
		// of the doors exact serving holds.
		c.TestModeInfo.Lifetime = models.LifetimePerTest
		c.TestModeInfo.LifetimeDerived = true
		c.TestModeInfo.Consume = models.ConsumeReuse
		c.TestModeInfo.IsFiltered = true
		c.TestModeInfo.ID = len(v.owned)
		c.TestModeInfo.SortOrder = int64(len(v.owned)) + 1
		c.TestModeInfo.ServedIn = v.gen
		c.TestModeInfo.Concurrent = shared
		c.MarkPooled()
		v.index[c.Name] = len(v.owned)
		v.owned = append(v.owned, c)
	}
}

// SetPoolNames installs the names the replay's scope table puts in a pool
// slice, of every key (keploy mock replay under the proven switch: the mocks
// its recording made while no test was open). InPool reads them; a replay
// that sends an empty table installs an empty set, which says no mock is the
// pool's.
func (m *MockManager) SetPoolNames(names []string) {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		if n != "" {
			set[n] = struct{}{}
		}
	}
	m.exact.poolNames.Store(&set)
}

// SetRecordingTimes sets where the teardown bound (teardownAfter) reads the
// staged set's recordings the trees do not hold: src visits every recording
// the agent keeps on disk, staged or not, with its name, its request and
// answer times, whether it is protocol-phase and its clock
// (models.Origin.Clock, "" for none), decoding none
// (DiskMocks.RangeTimes); nil for none, when every one is in the trees.
// keploy mock replay's staging reads no test's own recording (each test
// reads its own as it opens), and the bound is the latest of them: from the
// trees alone it is told by none. The agent sets it as it stages a set.
func (m *MockManager) SetRecordingTimes(src func(visit func(name string, req, res time.Time, protocolPhase bool, clock string) bool) error) {
	m.exact.times.Store(&recordingTimes{src: src})
}

// recordingTimes is where the teardown bound reads the times of the set's
// recordings the trees do not hold (SetRecordingTimes). A new one is stored
// on every set, so the bound cached under an older one is read again.
type recordingTimes struct {
	src func(visit func(name string, req, res time.Time, protocolPhase bool, clock string) bool) error
}

// SetPoolOwners installs, of the replay's scope table, the key of the test
// process whose tests list each mock (keploy mock replay under the proven
// switch): a test's key is its ID's path where that path is the key of a
// pool slice (the agent's teardownOwners), and a mock no one known key's
// tests list is "". It bounds each test process's teardown (teardownAfter):
// a recording only another process's tests list is no bound of this one's.
// The agent installs it before the pool names of the same table.
func (m *MockManager) SetPoolOwners(owners map[string]string) {
	cp := make(map[string]string, len(owners))
	for n, k := range owners {
		if n != "" {
			cp[n] = k
		}
	}
	m.exact.poolOwners.Store(&cp)
}

// InPool reports whether mk is the pool's by the scope table SetPoolNames
// installed (integrations.PoolMembership); known is false until one was. A
// mock is named once in a recording, and the table is the running
// recording's.
func (m *MockManager) InPool(mk *models.Mock) (pool, known bool) {
	set := m.exact.poolNames.Load()
	if set == nil || mk == nil {
		return false, false
	}
	_, pool = (*set)[mk.Name]
	return pool, true
}

var _ integrations.PoolMembership = (*MockManager)(nil)

// ServeExactPool turns exact serving on, or keeps it on, with no test open:
// the pool slice names, of key, answers beside the protocol-phase mocks.
// ahead, when given, is the next test's mocks that were in flight when it
// began (models.ExactServing.Ahead): recorded in the gap before its window,
// answered once it had begun. They answer now too, each at most once, as that
// test's mocks: one answered so counts as answered when the test opens
// (ServeExactTest), and the replay judges it with the test.
//
// A pool view that replaces one which already answered some of its ahead
// keeps those it names too answered, and what they bound: no call is answered
// twice. One it does not name was answered for a test that did not open next
// (one the replay could not open): it is no consume of the next test's, and is
// not flagged as one (ServeExactTest).
func (m *MockManager) ServeExactPool(key string, names []string, ahead ...*models.Mock) {
	answered, _ := m.closeAhead()
	if m.exact.beforeInstall != nil {
		m.exact.beforeInstall()
	}
	v := &exactView{poolKey: key, pool: make(map[string]struct{}, len(names))}
	for _, n := range names {
		v.pool[n] = struct{}{}
	}
	kept := map[string]bool{}
	if len(ahead) > 0 {
		v.gen = m.exact.gen.Add(1)
		v.index = make(map[string]int, len(ahead))
		v.addOwned(ahead)
		v.consumed = make([]bool, len(v.owned))
		v.served = make([]*models.Mock, len(v.owned))
		for _, a := range answered {
			if i, ok := v.index[a.owned.Name]; ok && !v.consumed[i] {
				v.consumed[i] = true
				v.order = append(v.order, i)
				v.served[i] = a.served
				kept[a.owned.Name] = true
			}
		}
	}
	// The scope the next test's calls in flight bind in (AheadVariables)
	// starts over with what the answers kept bound.
	var bound []models.VariableBinding
	for _, b := range m.aheadVars.Snapshot() {
		if kept[b.Mock] {
			bound = append(bound, b)
		}
	}
	m.aheadVars.StartCarrying(nil, bound)
	m.installExactWith(v, func(r *roundState) {
		m.settleAheadLocked(r, answered, v, func(name string) (bool, bool) { return false, kept[name] })
	})
	for _, a := range answered {
		if !kept[a.owned.Name] && m.logger != nil {
			m.logger.Debug("a call answered in the gap as the next test's is not the next test's: the one named did not open",
				zap.String("mock", a.owned.Name))
		}
	}
}

// ServeExactWorker names the set's worker-scope mocks its run has reached the
// place of (models.Mapping.Worker), cumulative: each view installed after it
// shows them, whether a test is open or not (agent.ExactWorkerServer). It
// installs no view itself: the next ServeExactTest or ServeExactPool does.
func (m *MockManager) ServeExactWorker(names []string) {
	set := make(map[string]struct{}, len(names))
	order := make(map[string]int, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		if _, dup := set[n]; !dup {
			order[n] = len(order)
		}
		set[n] = struct{}{}
	}
	m.exact.worker.Store(&reachedWorker{set: set, order: order})
}

// installExact makes v the view, then tells every parser that keeps an index
// of what it read that what it may read changed: every kind's revision moves,
// and so does the window signal. v is complete as it is published: what the
// door installing it gives it (its test and its mocks; on a test's view,
// its stamp, the claims a shared recording is answered under, its mocks the
// pool view before it answered in the gap (consumed, order, carried) and its
// mocks that pool view served as its calls in flight, answered or not
// (inFlight); on a concurrent door's view, which of its mocks are shared; on
// a pool view, its pool names, and, when it replaces a pool view that
// answered some of the calls in flight it names too, those kept answered
// (consumed, order, served)) and what this gives it from the manager (the
// outside answers, the staging epoch, the worker set and the round's
// generation) are all set before the swap that publishes it.
func (m *MockManager) installExact(v *exactView) { m.installExactWith(v, nil) }

// installExactWith is installExact that runs under, when not nil, holding the
// round lock in the step that moves the round's generation and swaps v in:
// what it changes is seen by a snapshot with v, never before it.
func (m *MockManager) installExactWith(v *exactView, under func(r *roundState)) {
	v.outside, v.epoch, v.kept = &m.exact.outside, &m.stagingEpoch, &m.exact.kept
	if v.test == "" {
		key := v.poolKey
		m.exact.poolCursor.Store(&key)
	}
	w := m.exact.worker.Load()
	if w != nil {
		v.worker = w.set
	}
	// A test's view is never a staging's: a test opens once its set is staged.
	v.staging = v.test == "" && m.exact.staging.Load()
	// Under the background rules the view orders its worker scope as the
	// agent placed it, and shows the pool in a test (exact_background.go).
	if bg := m.exact.bg.Load(); bg != nil {
		v.bg = bg
		if w != nil {
			v.workerOrder = w.order
		}
	}
	// The generation moves, and the learn tracker's open test is bound to a
	// test's view, before the view is visible: holding the round lock, which
	// every snapshot reads under.
	m.lockRound()
	r := &m.exact.round
	r.boundary("a view was installed")
	v.round = r.gen
	m.exact.shared.of(v, w)
	if v.test != "" {
		r.bindLocked(v)
	}
	if under != nil {
		under(r)
	}
	if m.exact.beforePublish != nil {
		m.exact.beforePublish(v)
	}
	// The test's cursors exist before its view is published, so a call
	// that reads the new view never has its commit refused for want of
	// them (a lost served mark would answer its recording again). No row
	// pins this order: it needs a hook between the two steps, and a call
	// reading the view between them is concurrency only.
	if v.test != "" {
		m.openTestCursors(v.gen)
	}
	prev := m.exact.view.Swap(v)
	if prev != nil {
		prev.mu.Lock()
		prev.closed = true
		// Closed, its outcome is final: no consume reaches it any more.
		if prev.test != "" && m.exact.last.Load() == prev {
			m.exact.ended.Store(prev.outcomeLocked())
			m.exact.last.Store(nil)
		}
		prev.mu.Unlock()
	}
	r.mu.Unlock()
	if m.exact.beforeBump != nil {
		m.exact.beforeBump(prev)
	}
	kinds := m.stagedKinds()
	for _, list := range [][]*models.Mock{v.owned, ownedOf(prev)} {
		for _, mk := range list {
			kinds[mk.Kind] = struct{}{}
		}
	}
	// The trees did not change: the stored generations stay (SharedGen).
	for k := range kinds {
		m.bumpViewKind(k)
	}
	m.bumpViewAll()
	m.signalWindowChange()
	// Only now, with v in force and the revisions moved, do the consumes
	// and spends that found prev closed go on, against v.
	if prev != nil {
		prev.markReplaced()
		if prev.test != "" {
			m.closeTestCursors(prev.gen)
		}
	}
	if m.exact.afterPublish != nil {
		m.exact.afterPublish(v)
	}
	if m.logger != nil {
		m.logger.Debug("exact serving", zap.String("test", v.test), zap.Int("test_mocks", len(v.owned)),
			zap.String("pool_key", v.poolKey), zap.Int("pool_mocks", len(v.pool)))
	}
}

func ownedOf(v *exactView) []*models.Mock {
	if v == nil {
		return nil
	}
	return v.owned
}

// stagedKinds is every kind the staged trees hold or have held. It never
// walks the startup tier: an install runs at every test's open and close,
// and a walk there cost each test the size of the whole staged set (an
// initial staging files the session pool in the startup tier too). Each
// kind the startup tier holds has a revision of its own (revByKind): the
// staging that filed a mock there moved its kind's (setMocksWithWindowLocked
// moves every kind it files, the startup tier's included), and a session
// update mirrored there (mirrorStartup) moves its new kind's.
func (m *MockManager) stagedKinds() map[models.Kind]struct{} {
	kinds := map[models.Kind]struct{}{}
	m.treesMu.RLock()
	for k := range m.filteredByKind {
		kinds[k] = struct{}{}
	}
	for k := range m.unfilteredByKind {
		kinds[k] = struct{}{}
	}
	m.treesMu.RUnlock()
	m.revMu.RLock()
	for k := range m.revByKind {
		kinds[k] = struct{}{}
	}
	m.revMu.RUnlock()
	return kinds
}

// ExactTestOutcome is, under exact serving, the latest test opened, open or
// ended, and which of its test mocks have answered (in the order they did)
// and which have not (in recorded order). Read once the test stops being
// served, it is final: no consume reaches a view that is not served. test is
// "" when no test has been opened; ok is false when exact serving is off.
func (m *MockManager) ExactTestOutcome() (test string, consumed, unconsumed []string, ok bool) {
	if m.exactView() == nil {
		return "", nil, nil, false
	}
	var o *testOutcome
	if v := m.exact.last.Load(); v != nil {
		v.mu.Lock()
		o = v.outcomeLocked()
		v.mu.Unlock()
	} else if o = m.exact.ended.Load(); o == nil {
		return "", nil, nil, true
	}
	return o.test, append([]string(nil), o.consumed...), append([]string(nil), o.unconsumed...), true
}

// testOutcome is a test view's outcome, by name (ExactTestOutcome).
type testOutcome struct {
	test                 string
	consumed, unconsumed []string
}

// outcomeLocked is v's outcome, v.mu held: which of its test mocks have
// answered (in the order they did) and which have not (in recorded order).
// Only the test's own: a shared recording is never required of it.
func (v *exactView) outcomeLocked() *testOutcome {
	o := &testOutcome{test: v.test}
	for _, i := range v.order {
		if !v.isCand(i) {
			o.consumed = append(o.consumed, v.owned[i].Name)
		}
	}
	for i, done := range v.consumed {
		if !done && !v.isCand(i) {
			o.unconsumed = append(o.unconsumed, v.owned[i].Name)
		}
	}
	return o
}

// ConsumedTestMocks implements integrations.ExactConsumedReader: the open
// test's mocks that have answered, in the order they did.
func (m *MockManager) ConsumedTestMocks() []*models.Mock {
	v := m.exactView()
	if v == nil || v.test == "" {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*models.Mock, 0, len(v.order))
	for _, i := range v.order {
		out = append(out, v.owned[i])
	}
	return out
}

var _ integrations.ExactConsumedReader = (*MockManager)(nil)

// ServedInOpenTest implements integrations.ExactServedReader: the open
// test's view's stamp (exactView.stamp) and every mock a parser was served
// while the view was installed, of any lifetime and through any door, once
// each, in the order they came (see exactView.heard), both of the one view
// loaded once; 0 and none while no test is open, and none with no served
// observer. The observer is told of a serve before the serve is filed here,
// and the view a serve is filed in is the one installed before the observer
// was told: a mock this returns is one the observer has heard of, and one
// served while this view was the open one.
func (m *MockManager) ServedInOpenTest() (uint64, []*models.Mock) {
	v := m.exactView()
	if v == nil || v.test == "" {
		return 0, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stamp, append([]*models.Mock(nil), v.heard...)
}

var _ integrations.ExactServedReader = (*MockManager)(nil)

// ClaimantOf implements integrations.ExactConcurrentReader: the test that
// answered a call with the shared recording name in the current staging.
func (m *MockManager) ClaimantOf(name string) (string, bool) {
	return m.exact.claims.claimant(m.stagingEpoch.Load(), name)
}

// ExactTakenCandidates implements integrations.ExactConcurrentReader: the open
// test's shared recordings of kind another test answered with.
func (m *MockManager) ExactTakenCandidates(kind models.Kind) []*models.Mock {
	v := m.exactView()
	if v == nil || v.test == "" || len(v.cand) == 0 {
		return nil
	}
	var out []*models.Mock
	for i, mk := range v.owned {
		if (kind == "" || mk.Kind == kind) && v.takenByOther(i) {
			out = append(out, mk)
		}
	}
	return out
}

var _ integrations.ExactConcurrentReader = (*MockManager)(nil)

// exactMissKind is where the miss of report r, reported now, was made: in
// a test, or outside every test. A parser that marked the call's round says
// it (models.MockMismatchReport.MarkedInTest): the call was made where its
// first read found it, and the view serving as the report reaches the proxy
// can be the pool view after the call's test ended, or the next test's. A
// marked call was made under exact serving. For a miss of a parser that
// marks no round, the view serving now: "" when exact serving is off.
func (m *MockManager) exactMissKind(r *models.MockMismatchReport) string {
	if r != nil && r.Mark != models.RoundUnmarked {
		if r.MarkedInTest {
			return models.MissInTest
		}
		return models.MissOutsideTest
	}
	v := m.exactView()
	switch {
	case v == nil:
		return ""
	case v.test != "":
		return models.MissInTest
	default:
		return models.MissOutsideTest
	}
}

// exactMissStaging reports whether a miss reaching the proxy now was made
// while a set's mocks were being staged: the view serving is a staging view
// (exactView.staging), which no test's recording answers.
func (m *MockManager) exactMissStaging() bool {
	v := m.exactView()
	return v != nil && v.staging
}

// missMadeInStaging reports whether the miss of report r was made while a
// set's mocks were being staged. A parser that marked the call's round says
// it (models.MockMismatchReport.Mark): the miss was decided where the call
// was marked, and the view serving as the report reaches the proxy can be
// the next staging's (a call decided under a staged set's view) or a staged
// set's (a call decided under the staging before it). For a miss of a parser
// that marks no round, the view serving now (exactMissStaging).
func (m *MockManager) missMadeInStaging(r *models.MockMismatchReport) bool {
	if r != nil {
		switch r.Mark {
		case models.RoundMarkedStaging:
			return true
		case models.RoundMarkedSettled:
			return false
		}
	}
	return m.exactMissStaging()
}

// unanswered is the view's test mocks that have not answered, of kind (every
// kind for ""), in recorded order; with sessionOnly, those staged in the
// session tier. A pool view's are the next test's mocks in flight when it
// began (ServeExactPool's ahead). It walks only the positions not consumed
// (liveList), so a test's read never walks again the mocks it answered.
func (v *exactView) unanswered(kind models.Kind, sessionOnly ...bool) []*models.Mock {
	if len(v.owned) == 0 {
		return nil
	}
	only := len(sessionOnly) > 0 && sessionOnly[0]
	v.mu.Lock()
	defer v.mu.Unlock()
	k := liveKey{kind: kind, session: only}
	l := v.liveListLocked(k)
	var out []*models.Mock
	visited := 0
	for j := l.firstLive(0, v.consumed, &visited); j < len(l.at); j = l.firstLive(j+1, v.consumed, &visited) {
		visited++
		if i := l.at[j]; !v.takenByOther(i) {
			out = append(out, v.owned[i])
		}
	}
	unansweredVisited.Add(uint64(visited))
	if exactViewVerify.Load() {
		verifyExactRead(k.what(), v.unansweredWalkLocked(k, 0, len(v.owned)), out)
	}
	return out
}

// rangeUnanswered calls fn with each of unanswered(kind, sessionOnly) in
// order until fn returns false, and reports whether fn never did. It reads
// them a few at a time (unansweredChunk) under mu and calls fn without it, so fn may call back
// into the view (a consume); each later few are read as the view is by then,
// as successive reads would be. A read that stops at the test's next
// unanswered mock visits only it: the consumed ones before it are skipped by
// their links (liveList), not walked.
func (v *exactView) rangeUnanswered(kind models.Kind, only bool, fn func(*models.Mock) bool) bool {
	return v.rangeLive(liveKey{kind: kind, session: only}, fn)
}

// rangeUnansweredWithKey is rangeUnanswered("") narrowed to the owned mocks
// ix files under key, in the same order, read the same way: a few at a time,
// past the answered ones by their links. The positions come from the view's
// own index of its copies' keys (ownedKeys), so a keyed read never calls Keys
// on a call, and a read that stops at the key's next unanswered mock visits
// only it, however many of the key's mocks answered before it (a statement
// the test ran thousands of times).
func (v *exactView) rangeUnansweredWithKey(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) bool {
	if len(v.owned) == 0 {
		return true
	}
	// Built outside mu: the first keyed read of a view keys every copy. No
	// copy filed under key (an index whose OwnedKeys files none, MySQL's)
	// reads nothing: no lock, no list kept for it. The verify mode still
	// reads it, so a copy the index lost is caught by the walk.
	if len(v.ownedKeys(ix)[key]) == 0 && !exactViewVerify.Load() {
		return true
	}
	return v.rangeLive(liveKey{ix: ix, key: key}, fn)
}

// rangeLive calls fn with each unanswered mock of the liveList k names, in
// its order, until fn returns false, and reports whether fn never did
// (rangeUnanswered, rangeUnansweredWithKey).
func (v *exactView) rangeLive(k liveKey, fn func(*models.Mock) bool) bool {
	if len(v.owned) == 0 {
		return true
	}
	var buf [unansweredChunk]*models.Mock
	j, size := 0, 1
	for {
		got, next, end := v.liveChunk(k, j, size, buf[:0])
		for _, mk := range got {
			if !fn(mk) {
				return false
			}
		}
		if end {
			return true
		}
		j, size = next, min(2*size, len(buf))
	}
}

// liveChunk appends to got, under mu, up to size unanswered mocks of the
// liveList k names, read from its position j on, and returns them, the
// position after the last one it looked at, and whether that is the list's
// end. The lock is released however it returns: a verify-mode panic leaves
// the view readable.
func (v *exactView) liveChunk(k liveKey, j, size int, got []*models.Mock) ([]*models.Mock, int, bool) {
	visited := 0
	v.mu.Lock()
	defer v.mu.Unlock()
	l := v.liveListLocked(k)
	from := 0
	if j < len(l.at) {
		from = l.at[j]
	}
	for len(got) < size {
		if j = l.firstLive(j, v.consumed, &visited); j >= len(l.at) {
			break
		}
		visited++
		if i := l.at[j]; !v.takenByOther(i) {
			got = append(got, v.owned[i])
		}
		j++
	}
	unansweredVisited.Add(uint64(visited))
	if exactViewVerify.Load() {
		// What a walk of these positions gives, in the same hold of the
		// lock: no consume falls between the two.
		to := len(v.owned)
		if j < len(l.at) {
			to = l.at[j]
		}
		verifyExactRead(k.what()+" read a few at a time", v.unansweredWalkLocked(k, from, to), got)
	}
	return got, j, j >= len(l.at)
}

// unansweredChunk is the most unanswered mocks rangeUnanswered reads in one
// hold of the view's lock: it reads one, then twice as many each time, so a
// read that stops at the first looks at no more, and a whole read takes the
// lock once per unansweredChunk.
const unansweredChunk = 64

// unansweredWalkLocked is the answer of the read k names for the owned
// positions in [from, to), by a walk of each, the keys of a keyed read
// computed again: what the verify mode compares the kept read with. Under
// v.mu.
func (v *exactView) unansweredWalkLocked(k liveKey, from, to int) []*models.Mock {
	var out []*models.Mock
	for i := from; i < to; i++ {
		if mk := v.owned[i]; !v.consumed[i] && k.reads(mk, v.session[i]) && !v.takenByOther(i) {
			out = append(out, mk)
		}
	}
	return out
}

// liveKey names one of a view's liveLists: the kind (every kind for "") and
// whether the session tier's alone; or, with ix set, the owned mocks ix files
// under key (a keyed read, rangeUnansweredWithKey), of every kind and tier.
type liveKey struct {
	kind    models.Kind
	session bool
	ix      *integrations.MockIndex
	key     string
}

// reads reports whether the read k names reads mk, staged in the session
// tier when session is set, by mk's fields and keys alone (no index).
func (k liveKey) reads(mk *models.Mock, session bool) bool {
	if k.ix != nil {
		return slices.Contains(ownedKeysOf(k.ix, mk), k.key)
	}
	return (k.kind == "" || mk.Kind == k.kind) && (!k.session || session)
}

// what names the read k names, for the verify mode's report.
func (k liveKey) what() string {
	if k.ix != nil {
		return "the open test's mocks under key " + strconv.Quote(k.key)
	}
	return "the open test's unanswered mocks of kind " + strconv.Quote(string(k.kind))
}

// liveList is the positions in a view's owned that one per-test read reads
// (liveKey), in recorded order, each linked forward past consumed ones. A
// view's consumed only ever turns true, so a link, once set, stays true: a
// walk skips each run of consumed positions in one step, and shortens the
// links it followed to where it stopped. A test that answers in recorded
// order so finds its next unanswered mock without walking the ones before.
type liveList struct {
	at []int
	// skip[j] > j says each of at[j:skip[j]] is consumed; 0 says nothing.
	skip []int32
}

// liveListLocked is v's liveList for k, built on its first use. A keyed
// one's positions are the view's index of its copies' keys under k.key
// (ownedKeys), shared, not copied. Under v.mu.
func (v *exactView) liveListLocked(k liveKey) *liveList {
	if l, ok := v.live[k]; ok {
		return l
	}
	l := &liveList{}
	if k.ix != nil {
		l.at = v.ownedKeys(k.ix)[k.key]
	} else {
		for i, mk := range v.owned {
			if k.reads(mk, v.session[i]) {
				l.at = append(l.at, i)
			}
		}
	}
	l.skip = make([]int32, len(l.at))
	if v.live == nil {
		v.live = map[liveKey]*liveList{}
	}
	v.live[k] = l
	return l
}

// firstLive is the first j at or after from whose position consumed does not
// mark, or len(at); it adds each consumed position it looks at to *visited.
// Under the view's lock.
func (l *liveList) firstLive(from int, consumed []bool, visited *int) int {
	r := from
	for r < len(l.at) && consumed[l.at[r]] {
		*visited++
		if s := int(l.skip[r]); s > r && !liveReadsWalk.Load() {
			r = s
		} else {
			r++
		}
	}
	if liveReadsWalk.Load() {
		return r
	}
	for x := from; x < r; {
		next := int(l.skip[x])
		if next <= x {
			next = x + 1
		}
		l.skip[x] = int32(r)
		x = next
	}
	return r
}

// unansweredAt is the positions of at, in owned, whose mocks have not
// answered and no other test took (unanswered's rule), in at's order: a
// parser that indexed the view's owned mocks (OwnedRecordings) reads what
// is left of one bucket, never the whole owned list. Each position of at
// counts one visit.
func (v *exactView) unansweredAt(at []int) []int {
	if len(at) == 0 {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]int, 0, len(at))
	for _, i := range at {
		if i >= 0 && i < len(v.owned) && !v.consumed[i] && !v.takenByOther(i) {
			out = append(out, i)
		}
	}
	unansweredVisited.Add(uint64(len(at)))
	return out
}

// ownedKeys is the view's index of its owned copies' keys under ix (keyed,
// ownedKeysOf), built on its first use.
func (v *exactView) ownedKeys(ix *integrations.MockIndex) map[string][]int {
	v.keyedMu.Lock()
	defer v.keyedMu.Unlock()
	if lists, ok := v.keyed[ix]; ok {
		return lists
	}
	lists := map[string][]int{}
	for i, mk := range v.owned {
		for _, k := range ownedKeysOf(ix, mk) {
			lists[k] = append(lists[k], i)
		}
	}
	if v.keyed == nil {
		v.keyed = map[*integrations.MockIndex]map[string][]int{}
	}
	v.keyed[ix] = lists
	return lists
}

// ownedKeysOf returns, each once, the keys ix files the open test's own mock
// mk under: OwnedKeys when ix sets it, Keys otherwise. The staged mocks are
// filed by Keys alone (the trees' key indexes).
func ownedKeysOf(ix *integrations.MockIndex, mk *models.Mock) []string {
	if ix.OwnedKeys == nil {
		return distinctKeys(ix, mk)
	}
	var out []string
	for _, k := range ix.OwnedKeys(mk) {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// shows reports whether the view shows mk among the staged mocks: a
// protocol-phase mock always; a worker-scope mock the run has reached the
// place of, whether a test is open or not; any other data-plane mock only
// between tests, and only in the pool slice served, or, under the
// background rules, in a test too when it is the pool's. The open test's own
// mocks are read from the view itself (unanswered), never from the staged
// trees.
func (v *exactView) shows(mk *models.Mock) bool {
	if mk == nil {
		return false
	}
	return v.unspentNow(mk) && v.showsStatic(mk)
}

// inBackgroundPool reports whether mk, consumed in an open test, is one of
// the pool's data-plane recordings the background rules show there.
func (v *exactView) inBackgroundPool(mk *models.Mock) bool {
	return v.test != "" && v.spendsAsPool(mk)
}

// spendsAsPool reports whether mk is one of the pool's data-plane recordings
// under the background rules, in a test or between tests: one answers at
// most one call in the staging, through whichever door a parser consumes it
// (exactConsumeIn). An async lane's recording keeps today's rules.
func (v *exactView) spendsAsPool(mk *models.Mock) bool {
	return v.bg.inPool(mk.Name) && !mk.IsProtocolPhase() && !mk.IsAsync()
}

// isWorker reports whether name is one of the set's worker-scope mocks the
// view shows (ServeExactWorker).
func (v *exactView) isWorker(name string) bool {
	_, ok := v.worker[name]
	return ok
}

// spentOutside reports whether name, a data-plane recording, was consumed
// outside every test in the current staging (outsideAnswers).
func (v *exactView) spentOutside(name string) bool {
	return v.outside != nil && v.outside.hidden(name, v.epoch.Load())
}

var _ integrations.StagingGenerations = (*MockManager)(nil)

// spentNow reports whether name has answered in a way that drops it from the
// reads while v is in force (MockManager.MockSpent): one of v's own (the open
// test's, or a pool view's ahead) consumed or taken by another test, as
// unanswered drops it; a staged mock spent in v, or consumed outside every
// test in the staging, as shows and unspent drop it.
func (v *exactView) spentNow(name string) bool {
	_, gone := v.spent.Load(name)
	v.mu.Lock()
	if i, own := v.index[name]; own && !gone {
		gone = v.consumed[i] || v.takenByOther(i)
	}
	v.mu.Unlock()
	return gone || v.spentOutside(name)
}

// unspent is in less the mocks a parser consumed through a delete door in
// this view, or outside every test in the staging; in itself when there are
// none.
func (v *exactView) unspent(in []*models.Mock) []*models.Mock {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(in) == 0 {
		return in
	}
	var out []*models.Mock
	for i, mk := range in {
		gone := mk != nil && (v.spentIn(mk.Name) || v.spentOutside(mk.Name))
		if gone && out == nil {
			out = append(make([]*models.Mock, 0, len(in)), in[:i]...)
		}
		if !gone && out != nil {
			out = append(out, mk)
		}
	}
	if out == nil {
		return in
	}
	return out
}

// keep narrows staged mocks to those the view shows, in their order, each
// as the view hands it out (learned).
func (v *exactView) keep(in []*models.Mock) []*models.Mock {
	return v.keepWith(in, v.shows)
}

// keepWith is keep with show in place of shows: showsStatic gives what the
// view shows of in whatever it spent (SharedMocks).
func (v *exactView) keepWith(in []*models.Mock, show func(*models.Mock) bool) []*models.Mock {
	viewNarrowed.Add(uint64(len(in)))
	out := make([]*models.Mock, 0, len(in))
	if v.bg != nil {
		return v.backgroundOrder(out, in, show)
	}
	for _, mk := range in {
		if show(mk) {
			out = append(out, v.learned(mk))
		}
	}
	return out
}

// backgroundOrder appends to out what the view shows of in, in the
// background order: what is not background first (the
// protocol-phase mocks), in their order; then the worker scope, the earlier
// place first, delivering before idle, then recorded order (workerOrder);
// then the pool, in recorded order, whichever trees in came from. Each is
// as the view hands it out (learned).
func (v *exactView) backgroundOrder(out, in []*models.Mock, show func(*models.Mock) bool) []*models.Mock {
	var worker, pool []*models.Mock
	for _, mk := range in {
		if !show(mk) {
			continue
		}
		l := v.learned(mk)
		switch {
		case mk.IsProtocolPhase():
			out = append(out, l)
		case v.isWorker(mk.Name):
			worker = append(worker, l)
		default:
			pool = append(pool, l)
		}
	}
	sort.SliceStable(worker, func(i, j int) bool {
		return v.workerOrder[worker[i].Name] < v.workerOrder[worker[j].Name]
	})
	sort.SliceStable(pool, func(i, j int) bool {
		return pool[i].Spec.ReqTimestampMock.Before(pool[j].Spec.ReqTimestampMock)
	})
	return append(append(out, worker...), pool...)
}

// perTest is a per-test read under exact serving: the open test's unanswered
// mocks of kind (between tests, the next test's ahead), then what the view
// shows of the per-test tree's values (staged) and of teardown.
//
// teardown is the set's teardown recordings (MockManager.teardown), which the
// callers (exactPerTest, the round snapshot) pass only while no test is open
// after the first test fired (afterTests). The staging files every per-test
// recording in the startup tree alone, so a call recorded after the set's
// last test (a Go mongo-driver's endSessions at client.Disconnect) is in no
// per-test tree, and Mongo's index reads the startup tier only until the
// first test fires: from then on it reads such a call's recording only
// through a per-test read, and without teardown the call missed though the
// view shows its recording. What the view does not show is never added (a
// recording outside the pool slice and the reached worker scope), and a pool
// recording consumed through a delete door (Mongo's per-test consume) is spent
// outside every test and shown no more, so the same call made once more
// misses, as a call with no recording does.
func (v *exactView) perTest(kind models.Kind, staged, teardown []*models.Mock) []*models.Mock {
	return append(v.unanswered(kind), v.keepPerTest(staged, teardown)...)
}

// keepPerTest is what perTest hands after the test's own mocks: what v shows
// of staged, then of teardown.
func (v *exactView) keepPerTest(staged, teardown []*models.Mock) []*models.Mock {
	all := staged
	if len(teardown) > 0 {
		all = append(append(make([]*models.Mock, 0, len(staged)+len(teardown)), staged...), teardown...)
	}
	return v.keep(all)
}

// afterTests reports whether a per-test read in v reads the set's teardown
// recordings too (perTest): while no test is open, once the first test fired.
func (v *exactView) afterTests(fired bool) bool { return v.test == "" && fired }

// teardown is, of startup (the startup tree's values), the per-test
// recordings sent after every test of the set's recording, or under keploy
// mock replay after every test of the test process of key (the view's pool
// key): the teardown (teardownAfter). Those alone, never a pool recording
// made before the first test or in a gap between two tests: a late call
// paired with such a recording, made before another test's own, would learn
// as noise a value that test's answer gave the application, which the learn
// step's earlier places (every recording made before the paired one) do not
// hold. Every recording of the calling process's tests is an earlier place
// of a teardown recording. A set whose tests' place nothing tells gets none,
// and such a call misses.
func (m *MockManager) teardown(key string, startup []*models.Mock) []*models.Mock {
	on := m.teardownBy(key)
	if on == nil {
		return nil
	}
	viewNarrowed.Add(uint64(len(startup)))
	var out []*models.Mock
	for _, mk := range startup {
		if mk == nil || mk.TestModeInfo.Lifetime != models.LifetimePerTest {
			continue
		}
		if after := on(mockClock(mk)); after != nil && after(mk.Spec.ReqTimestampMock) {
			out = append(out, mk)
		}
	}
	return out
}

// teardownAfter is what tells a request sent after every test of the set's
// recording, or under keploy mock replay of the test process of key, nil
// when nothing does. keploy test's staging names no pool and sends the
// recorded windows of every test of the set, the unselected ones included:
// after every window's end (models.WindowSchedule.After); its application is
// one process. keploy mock replay names the pool's recordings (SetPoolNames)
// and the key of each test's (SetPoolOwners): after every data-plane
// recording of the set outside the pool, request and answer, but one that
// only another key's tests list. That counts every recording a test of key
// owns, and any other one outside the pool too (one no test owns, or one a
// test of no known key owns): a later bound, never an earlier one. The
// recordings are the staged trees' and, of a set the agent keeps on disk,
// every one the store holds (SetRecordingTimes), staged or not: the staging
// reads no test's own (each test reads its own as it opens), so the trees
// alone hold no recording of a test that has not opened, and a bound read
// from them would fall before the last test (its call after the last test
// would miss) or before a gap between two tests (a gap's recording would be
// the teardown). A recording the bound reads from neither is no bound: one
// the store cannot be read for gives none, and the call after the last test
// misses, failing closed. Under the proven switch keploy mock replay
// sends the recorded windows too; the pool's names still decide its bound,
// so the windows serve the learn step and a test's stateful sequence only:
// another process's tests' windows never move this process's bound.
//
// So of a suite of several test processes (go test ./..., one test binary
// per package, run at once by go test's default -p, or one after another),
// each process's teardown is its own: the calls a binary makes after its
// own last test (a Go mongo-driver's endSessions at client.Disconnect) are
// answered by its pool slice's recordings of them, which only the process
// of that key is served. A recording another process's tests list is no bound of this
// one's: whether or not that process ran at the same time, none of its
// answers ever reached this one. Where a test's key is not known (its ID's
// path names its test file, not a pool slice's key; or its process recorded
// no pool call), its recordings bound every process, as the whole
// recording's bound did; two processes of one key share one bound, the
// later's, so the earlier's call after its last test misses.
//
// Once the first test fired, the teardown is shown in every gap, not only
// after the last test, and in a later process's start-up: a call made there
// may be answered by a teardown recording, and the teardown's own call then
// misses, failing closed.
//
// Under keploy mock replay the bound is per clock (models.Origin.Clock), as
// fakeconn.SetClockID's contract orders stamps: only two on one clock may be
// ordered against each other. A set may hold recordings of several record
// runs, each on its own clock: a partial record keeps the tests it did not
// record anew, with the pool of the run that recorded them, beside the tests
// it did. Of a recording on a clock, the bound is the latest recording on
// that clock and on none; a recording on another clock is no bound of it.
// So a newer run's test, though recorded later, never moves an older run's
// call after its own last test (a Go mongo-driver's endSessions) out of the
// teardown, and that call is answered. A recording on no clock ("", an
// untrusted stamp) is bounded by every recording, whatever its clock, as
// before, and bounds every one: an untrusted time widens the bound, failing
// closed. One record run stamps its recordings on one clock (the proxy's, or
// proxyless capture's ktime, which has no relay) or on none, so the bound of
// a set recorded in one run is the whole set's, as it was. Its limits: a
// clock none of whose tests' recordings remain (every test of that run was
// recorded anew) tells no teardown of it, and its call after the last test
// misses (fail closed); and where the test recorded anew was its run's last,
// that run's pool calls made between its previous test and it are after
// every recording left on its clock, so they are its teardown too. keploy
// test's staging (the recorded windows alone) reads no clock: it never
// stages two record runs' recordings as one set.
func (m *MockManager) teardownAfter(key, clock string) func(time.Time) bool {
	on := m.teardownBy(key)
	if on == nil {
		return nil
	}
	return on(clock)
}

// mockClock is mk's clock (models.Origin.Clock) as the teardown bound reads
// it: "" for none, and for every mock without the proven switch, which uses
// no origin (models.Origin): there every recording bounds every other, as
// before.
func mockClock(mk *models.Mock) string {
	if mk.Origin == nil || !provenswitch.On() {
		return ""
	}
	return mk.Origin.Clock
}

// teardownBy is teardownAfter of every clock at once: the bound of a
// recording on a clock, nil when nothing tells a teardown of any.
func (m *MockManager) teardownBy(key string) func(clock string) func(time.Time) bool {
	pool := m.exact.poolNames.Load()
	if pool == nil {
		if s := m.testWindows.Load(); s.Len() > 0 {
			return func(string) func(time.Time) bool { return s.After }
		}
		return nil
	}
	owners := m.exact.poolOwners.Load()
	// The generation first, then the trees (the revision's sample-read-install
	// idiom): a bound read from trees a staging swaps meanwhile is cached
	// under a generation that is already old. The staging generation, not
	// Revision: the bound reads the trees, the pool's names and its owners
	// alone, which no consume under exact serving changes, so a read between
	// tests after each call's consume does not walk the whole staged set
	// again.
	rev := m.StagingGeneration()
	times := m.exact.times.Load()
	if c := m.exact.teardownLast.Load(); c != nil && c.rev == rev && c.pool == pool && c.owners == owners && c.times == times && c.key == key {
		return c.on()
	}
	b := &teardownBound{rev: rev, pool: pool, owners: owners, times: times, key: key}
	// bounds reports whether a recording named name bounds key's process.
	bounds := func(name string, protocolPhase bool) bool {
		if _, inPool := (*pool)[name]; inPool || protocolPhase {
			return false
		}
		if owners != nil && key != "" && key != models.PoolKeyNone {
			if k := (*owners)[name]; k != "" && k != key {
				return false
			}
		}
		return true
	}
	m.treesMu.RLock()
	trees := []*TreeDb{m.filtered, m.unfiltered, m.startup}
	m.treesMu.RUnlock()
	for _, tree := range trees {
		for _, mk := range m.treeValues(tree, "") {
			if bounds(mk.Name, mk.IsProtocolPhase()) {
				b.add(mockClock(mk), mk.Spec.ReqTimestampMock, mk.Spec.ResTimestampMock)
			}
		}
	}
	if times != nil && times.src != nil {
		err := times.src(func(name string, req, res time.Time, protocolPhase bool, clock string) bool {
			if bounds(name, protocolPhase) {
				b.add(clock, req, res)
			}
			return true
		})
		if err != nil {
			// The bound is not known: no teardown, not cached, so the
			// next read tries again.
			m.logger.Debug("the set's teardown bound could not read the recording's times", zap.Error(err))
			return nil
		}
	}
	m.exact.teardownLast.Store(b)
	return b.on()
}

// teardownBound is teardownAfter's bound for keploy mock replay: the latest
// request or answer of the data-plane recordings of the set outside the pool
// that bound key's process, of every clock (last), of those on no clock
// (unclocked) and of those on each clock (clocks), as the trees stood at
// generation rev (MockManager.StagingGeneration) under the scope table pool
// and owners, with the set's recordings on disk read from times: the
// per-test reads after the tests read it once per staging generation, not
// every recording at every read. An exact serve moves Revision but
// not the staging generation: exact serving hides what it serves rather
// than deleting it from the trees, so the bound stands.
type teardownBound struct {
	rev       uint64
	pool      *map[string]struct{}
	owners    *map[string]string
	times     *recordingTimes
	key       string
	last      time.Time
	unclocked time.Time
	clocks    map[string]time.Time
}

// add moves the bound to ts, of a recording on clock, where they are later.
func (b *teardownBound) add(clock string, ts ...time.Time) {
	for _, at := range ts {
		if at.After(b.last) {
			b.last = at
		}
		if clock == "" {
			if at.After(b.unclocked) {
				b.unclocked = at
			}
			continue
		}
		if at.After(b.clocks[clock]) {
			if b.clocks == nil {
				b.clocks = make(map[string]time.Time)
			}
			b.clocks[clock] = at
		}
	}
}

// on is the bound's teardownBy: nil when no recording bounds key's process.
func (b *teardownBound) on() func(clock string) func(time.Time) bool {
	if b.last.IsZero() {
		return nil
	}
	return b.after
}

// after is the bound of a recording on clock: the latest recording on that
// clock or on none, and of one on no clock, the latest of every clock; nil
// for a zero bound.
func (b *teardownBound) after(clock string) func(time.Time) bool {
	last := b.last
	if clock != "" {
		last = b.clocks[clock]
		if b.unclocked.After(last) {
			last = b.unclocked
		}
	}
	if last.IsZero() {
		return nil
	}
	return last.Before
}

// exactPerTest is v's per-test read of staged, of kind (exactView.perTest),
// with the set's teardown recordings of kind when the read takes them.
func (m *MockManager) exactPerTest(v *exactView, kind models.Kind, staged []*models.Mock) []*models.Mock {
	return v.perTest(kind, staged, m.exactTeardown(v, kind))
}

// exactTeardown is the set's teardown recordings of kind v's per-test read
// takes (exactView.afterTests), nil when it takes none.
func (m *MockManager) exactTeardown(v *exactView, kind models.Kind) []*models.Mock {
	if !v.afterTests(m.exact.fired.Load()) {
		return nil
	}
	// What the view shows of the startup tier: perTest keeps only those.
	return m.teardown(v.poolKey, v.shownOf(m.startupTree(), kind))
}

// rangeExactPerTest calls fn with each mock of exactPerTest(v, kind, staged
// of tree) in order until fn returns false. The test's own unanswered mocks
// are read first, a few at a time (rangeUnanswered); the tier is read only
// when fn has not stopped by then, so a parser that stops at the test's next
// recording reads neither the tier nor the mocks the test answered.
func (m *MockManager) rangeExactPerTest(v *exactView, kind models.Kind, tree *TreeDb, fn func(*models.Mock) bool) {
	if !v.rangeUnanswered(kind, false, fn) {
		return
	}
	for _, mk := range v.keepPerTest(v.shownOf(tree, kind), m.exactTeardown(v, kind)) {
		if !fn(mk) {
			return
		}
	}
}

// reusable is a session-tier read under exact serving: the open test's
// unanswered mocks staged in that tier, then what the view shows of the
// session and the startup trees (each mock once). The reusable tiers show
// one set, whichever a parser reads: the staging filed the pool in both, a
// per-test-lifetime mock in the startup tree only, and a parser that reads
// one tier alone (enterprise SQS reads the session tier, Kafka never the
// startup tier) finds everything the view shows it.
func (v *exactView) reusable(kind models.Kind, staged ...[]*models.Mock) []*models.Mock {
	// Between tests a pool view's ahead (per-test mocks, in flight when the
	// next test began) is in every reusable read too: a tier-routing parser
	// reads only the reusable tiers while no test is open.
	return v.reusableWith(kind, v.unanswered(kind, v.test != ""), v.shows, staged...)
}

// reusableWith is reusable led by out, with show in place of shows
// (keepWith): SharedMocks reads it led by nothing, with showsStatic.
func (v *exactView) reusableWith(kind models.Kind, out []*models.Mock, show func(*models.Mock) bool, staged ...[]*models.Mock) []*models.Mock {
	seen := make(map[*models.Mock]struct{}, len(out))
	if v.bg != nil {
		// One order across the tiers (backgroundOrder), each mock once.
		var all []*models.Mock
		for _, list := range staged {
			viewNarrowed.Add(uint64(len(list)))
			for _, mk := range list {
				if _, dup := seen[mk]; dup || (kind != "" && mk.Kind != kind) {
					continue
				}
				seen[mk] = struct{}{}
				all = append(all, mk)
			}
		}
		return v.backgroundOrder(out, all, show)
	}
	for _, list := range staged {
		for _, mk := range v.keepWith(list, show) {
			if _, dup := seen[mk]; dup || (kind != "" && mk.Kind != kind) {
				continue
			}
			seen[mk] = struct{}{}
			out = append(out, mk)
		}
	}
	return out
}

// exactVerdict is how exact serving settles a consume a parser reports.
type exactVerdict int

const (
	// exactPass: a mock the view shows from the staged trees (protocol-phase,
	// or the pool slice between tests): consumed through its tier, as
	// without exact serving.
	exactPass exactVerdict = iota
	// exactServed: one of the open test's mocks, answering for the first
	// time; it answers nothing more until the test begins again.
	exactServed
	// exactRefused: a test mock that already answered, or a mock the view
	// does not show now (a parser that read before a begin or an end).
	exactRefused
)

// exactConsume settles a parser's consume of mk under exact serving. evicts
// is a delete door (DeleteFilteredMock, DeleteUnFilteredMock,
// DeleteStartupMock): a staged mock the view shows is then spent, and the
// staged trees keep it. A data-plane recording consumed with no test open is
// spent for the staging (consumeOutside); any other, until the view changes.
// One of the worker scope's, and under the background rules one of the
// pool's, is spent for the staging through a door that does not evict too.
//
// A consume that finds its view closed (a begin or an end replacing it) is no
// refusal: it waits until the replacing view is installed and the revisions
// moved, and is settled against that one (closedWait), as one that came a
// moment later would be.
func (m *MockManager) exactConsume(mk *models.Mock, evicts bool) exactVerdict {
	return m.exactConsumeAt(mk, evicts, nil, nil, nil)
}

// consumeAt is the one way through a consume door, and the one place that
// tells the served observer (observe, when not nil): only of a consume that
// went through, the view settling it as a serve or, passed to mk's tier, tier
// taking it. tier reports whether it did. With exact serving off the tier
// alone decides, and with g nothing is settled, since no round is read
// without it.
func (m *MockManager) consumeAt(mk *models.Mock, evicts bool, g *roundGuard, tier func() bool, observe func()) bool {
	if m.exactView() != nil {
		return m.exactConsumeAt(mk, evicts, g, tier, observe) != exactRefused
	}
	if g.checks() {
		g.refuse("exact serving is off")
		return false
	}
	if !tier() {
		return false
	}
	if observe != nil {
		observe()
	}
	return true
}

// exactConsumeAt is exactConsume that tells the served observer (observe,
// when not nil, once) in the step that settles the consume, once it settled
// as a serve (exactServed, or exactPass and tier, when not nil, took it from
// its tier in that step), and, with g, settles it only while the store is
// still at g's round (roundGuard): a consume decided on a snapshot never
// lands in another staging, view or learn test. A consume refused (exactRefused:
// a mock the view does not show now, one that answered already, another test's
// claim, one the tier refused) tells the observer nothing, as one refused for
// g's round does: nothing served the mock, so the learn tracker neither
// numbers it nor counts its request as a serve (learn.Flight), and the open
// test's account (ServedInOpenTest) does not hold it. Each settle holds the
// round lock (exact_round.go), as every snapshot does, so a snapshot sees a
// consume, its take and the observer's account of it all, or none of them.
//
// tier runs at most once, holding the round lock, so it takes no lock that
// must come before it (swapMu): what it reads of the staging its door
// prepared before it (learnedAfterEndPrepared). Only UpdateUnFilteredMock's
// and MarkMockAsUsed's tiers run here: a view never passes a delete door's
// consume (exactConsumeIn settles those itself), so a delete tier, which
// reads the staging under swapMu, runs only with exact serving off.
func (m *MockManager) exactConsumeAt(mk *models.Mock, evicts bool, g *roundGuard, tier func() bool, observe func()) exactVerdict {
	if f := m.exact.round.onConsume.Load(); f != nil && mk != nil {
		(*f)(mk.Name)
	}
	return settleInForce(m, exactRefused, func(v *exactView) (exactVerdict, <-chan struct{}) {
		if v != nil && mk != nil && (evicts || v.isWorker(mk.Name) || v.spendsAsPool(mk)) {
			if _, own := v.index[mk.Name]; !own {
				// What a spend reads (consumeOutside), and the staged
				// recording its take names (stagedRecording), before the
				// round lock: it reads the staging under swapMu.
				m.prepareOutside()
			}
		}
		m.lockRound()
		// A take the round moved under is refused, or waits for the install
		// its closing view announces and is settled again in the view that
		// follows (where the round moved): either way the served observer
		// is told nothing, of a mock nothing served.
		if ok, wait := m.guardLocked(g, v); !ok {
			return exactRefused, wait
		}
		verdict, wait, after := func() (exactVerdict, <-chan struct{}, func()) {
			// Let go however this step ends. A parser's goroutine recovers
			// a panic and loses its connection; one that held the round
			// lock past it would leave every consume, snapshot and boundary
			// after it waiting for good, with no error.
			defer m.exact.round.mu.Unlock()
			verdict, wait, after := m.exactConsumeIn(v, mk, evicts)
			// One the view passes is its tier's to take, in this step: a
			// tier that refuses it refuses the consume.
			if wait == nil && verdict == exactPass && tier != nil && !tier() {
				verdict = exactRefused
			}
			// Told once the consume went through, in this step, still
			// holding the round lock: never of one refused. One that found
			// this view closed (wait) is told once, as it settles as a
			// serve in the view that replaces it.
			if wait == nil && verdict != exactRefused && observe != nil {
				observe()
				observe = nil
			}
			return verdict, wait, after
		}()
		if f := m.exact.round.onSettled; f != nil && verdict == exactServed {
			f()
		}
		if after != nil {
			after()
		}
		return verdict, wait
	})
}

// settleInForce settles a consume or a spend (settle) against the view in
// force, and, while the view it finds is closed (settle's wait), again
// against the view that replaces it, once that is installed and the
// revisions moved. A view that was waited on and is in force again has no
// replacement to wait for: what it says it settled is final, and the settle
// is refused (refused), never a wait that spins.
func settleInForce[T any](m *MockManager, refused T, settle func(v *exactView) (T, <-chan struct{})) T {
	var waited *exactView
	for {
		v := m.exactView()
		if v != nil && v == waited {
			return refused
		}
		out, wait := settle(v)
		if wait == nil {
			return out
		}
		m.waitReplaced(wait)
		waited = v
	}
}

// exactConsumeIn is exactConsume settled against v, holding the round lock:
// what it takes is written to the staging's take log in the same step.
// wait, when not nil, says v is closed: nothing was settled, and the consume
// is to be settled again once wait is closed. after, when not nil, is what
// the consume tells the replay and the parsers' indexes, to run once the
// round lock is let go.
func (m *MockManager) exactConsumeIn(v *exactView, mk *models.Mock, evicts bool) (exactVerdict, <-chan struct{}, func()) {
	if v == nil || mk == nil {
		return exactPass, nil, nil
	}
	// The open test's mocks, or a pool view's ahead (the next test's mocks
	// in flight when it began): each answers once.
	if len(v.owned) > 0 {
		if i, ok := v.index[mk.Name]; ok {
			// A copy an earlier view served (a previous attempt's, read
			// before this begin) answers for nothing now. A mock a parser
			// built by name carries no view, and is the open test's.
			if g := mk.TestModeInfo.ServedIn; g != 0 && g != v.gen {
				return exactRefused, nil, nil
			}
			v.mu.Lock()
			if wait := v.closedWait(); wait != nil {
				v.mu.Unlock()
				return exactRefused, wait, nil
			}
			if v.consumed[i] {
				v.mu.Unlock()
				return exactRefused, nil, nil
			}
			// A shared recording answers once in the staging: for the
			// first of the tests it is shared with to make its call.
			if v.isCand(i) && !m.exact.claims.claim(m.stagingEpoch.Load(), mk.Name, v.test) {
				v.mu.Unlock()
				return exactRefused, nil, nil
			}
			v.consumed[i] = true
			v.order = append(v.order, i)
			owned := v.owned[i]
			if v.test == "" {
				// A pool view's ahead: its consume is flagged as the
				// next test opens (ServeExactTest), not now, so the
				// ended test's drain does not read it.
				v.served[i] = mk.ShallowCopy()
				v.mu.Unlock()
				m.takeLocked(integrations.Take{Mock: owned, How: integrations.TakeConsumed, Gen: v.round, Ahead: true})
				return exactServed, nil, func() { m.bumpConsumed(owned.Kind) }
			}
			v.mu.Unlock()
			m.takeLocked(integrations.Take{Mock: owned, How: integrations.TakeConsumed, Gen: v.round, Test: v.test})
			return exactServed, nil, func() { m.noteExactServed(owned, mk) }
		}
	}
	if !v.shows(mk) {
		return exactRefused, nil, nil
	}
	// One of the worker scope's answers at most one request in the staging,
	// as a test's own mock does in its test, through whichever door a parser
	// consumes it: a parser that keeps a session mock it serves (HTTP's
	// MarkMockAsUsed) spends it too. So does one of the pool's under the
	// background rules, in a test or between tests: a parser that consumes
	// only through a door that never evicts (MarkMockAsUsed: enterprise
	// Redis and Aerospike; UpdateUnFilteredMock: keploy's Generic) would
	// else have one pool recording answer every identical call of the set.
	worker := v.isWorker(mk.Name) && !mk.IsProtocolPhase()
	if !evicts && !worker && !v.spendsAsPool(mk) {
		return exactPass, nil, nil
	}
	// A data-plane recording consumed through a delete door with no test
	// open, and one of the worker scope's whether a test is open or not, is
	// spent for the staging: it answers at most one request in the set. So
	// is one of the pool's a test consumes under the background rules,
	// through any door: the background is the set's, whoever's call it
	// answers.
	if (v.test == "" || worker || v.inBackgroundPool(mk)) && !mk.IsProtocolPhase() {
		// The view's closed check and the spend are one step: no begin or
		// end lands between them.
		v.mu.Lock()
		if wait := v.closedWait(); wait != nil {
			v.mu.Unlock()
			return exactRefused, wait, nil
		}
		verdict := m.consumeOutside(mk)
		v.mu.Unlock()
		if verdict != exactServed {
			return verdict, nil, nil
		}
		m.takeLocked(integrations.Take{Mock: m.stagedRecording(mk), How: integrations.TakeSpent, Gen: v.round, Test: v.test})
		return verdict, nil, func() { m.noteExactSpent(mk) }
	}
	v.mu.Lock()
	if wait := v.closedWait(); wait != nil {
		v.mu.Unlock()
		return exactRefused, wait, nil
	}
	if _, dup := v.spent.LoadOrStore(mk.Name, true); dup {
		v.mu.Unlock()
		return exactRefused, nil, nil
	}
	v.mu.Unlock()
	m.takeLocked(integrations.Take{Mock: m.stagedRecording(mk), How: integrations.TakeSpent, Gen: v.round, Test: v.test, ForView: true})
	return exactServed, nil, func() { m.noteExactSpent(mk) }
}

// closedWait is, with v.mu held, nil while v is open, else a channel that is
// closed once the view replacing v is installed and the revisions moved
// (markReplaced): closed already when it is.
func (v *exactView) closedWait() <-chan struct{} {
	if !v.closed {
		return nil
	}
	if v.replacedCh == nil {
		v.replacedCh = make(chan struct{})
		if v.replaced {
			close(v.replacedCh)
		}
	}
	return v.replacedCh
}

// markReplaced says the view replacing v is installed and the revisions
// moved: v is closed, and every consume and spend waiting on it goes on
// against the view in force.
func (v *exactView) markReplaced() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.closed = true
	if v.replaced {
		return
	}
	v.replaced = true
	if v.replacedCh != nil {
		close(v.replacedCh)
	}
}

// waitReplaced waits until wait (closedWait) is closed: the closed view's
// replacement is in force.
func (m *MockManager) waitReplaced(wait <-chan struct{}) {
	if m.exact.onClosedWait != nil {
		m.exact.onClosedWait()
	}
	<-wait
}

// consumeValueAt is consumeAt for a door that takes the mock by value, which
// tells the served observer of a copy of its own when observe is set. Its copy
// is made only under exact serving: a door taking the mock's address would
// move every consume's mock to the heap, switch on or off.
func (m *MockManager) consumeValueAt(mock models.Mock, evicts bool, g *roundGuard, observe bool, tier func(models.Mock) bool) bool {
	if m.exactView() != nil {
		mk := mock
		var tell func()
		if observe {
			tell = func() { m.observeServedCopy(mk, g.flight()) }
		}
		return m.exactConsumeAt(&mk, evicts, g, func() bool { return tier(mk) }, tell) != exactRefused
	}
	if g.checks() {
		g.refuse("exact serving is off")
		return false
	}
	if !tier(mock) {
		return false
	}
	if observe {
		m.observeServedCopy(mock, g.flight())
	}
	return true
}

// dataPlaneTyped is MockState.DataPlane for mk: a test mock whose type tag
// says protocol-phase (a Generic exchange its recorder stated per-test).
func dataPlaneTyped(mk *models.Mock) bool {
	switch mk.Spec.Metadata["type"] {
	case "config", "connection":
		return !mk.IsProtocolPhase()
	}
	return false
}

// noteExactSpent records that a staged mock answered through a delete door,
// and tells the parsers that keep an index that it is gone: until the view
// changes, or for a data-plane recording consumed outside every test, until
// the next staging.
func (m *MockManager) noteExactSpent(mk *models.Mock) {
	if err := m.flagMockAsUsed(models.MockState{
		Name:             mk.Name,
		Kind:             mk.Kind,
		Usage:            models.Updated,
		IsFiltered:       mk.TestModeInfo.IsFiltered,
		SortOrder:        mk.TestModeInfo.SortOrder,
		Type:             mk.Spec.Metadata["type"],
		DataPlane:        dataPlaneTyped(mk),
		Lifetime:         mk.TestModeInfo.Lifetime,
		ReqTimestampMock: models.FormatMockTimestamp(mk.Spec.ReqTimestampMock),
		ResTimestampMock: models.FormatMockTimestamp(mk.Spec.ResTimestampMock),
		AnswerPlaced:     models.IsAnswerPlaced(mk),
		ReqBodyNoise:     reqBodyNoiseOf(mk.Spec),
		Variables:        variablesOf(mk.Spec),
		Clocks:           clocksOf(mk.Spec),
		Binds:            bindsOf(mk.Spec),
		LearnedAfterEnd:  m.learnedAfterEndNamed(mk),
	}, 0); err != nil && m.logger != nil {
		m.logger.Error("failed to flag mock as used", zap.Error(err))
	}
	m.bumpConsumed(mk.Kind)
}

// noteExactServed records that test mock owned answered (consumed, served
// once) and tells the parsers that keep an index that it is gone. served is
// the copy the parser reported, which carries the request-body noise it
// detected, if any, and under the proven switch what record's full round
// learned onto it (its variables and the binds it carries onto earlier
// calls), as the other doors' states do.
func (m *MockManager) noteExactServed(owned, served *models.Mock) {
	if err := m.flagMockAsUsed(models.MockState{
		Name:             owned.Name,
		Kind:             owned.Kind,
		Usage:            models.Deleted,
		IsFiltered:       true,
		SortOrder:        owned.TestModeInfo.SortOrder,
		Type:             owned.Spec.Metadata["type"],
		DataPlane:        dataPlaneTyped(owned),
		Lifetime:         owned.TestModeInfo.Lifetime,
		ReqTimestampMock: models.FormatMockTimestamp(owned.Spec.ReqTimestampMock),
		ResTimestampMock: models.FormatMockTimestamp(owned.Spec.ResTimestampMock),
		AnswerPlaced:     models.IsAnswerPlaced(owned),
		ReqBodyNoise:     reqBodyNoiseOf(served.Spec),
		Variables:        variablesOf(served.Spec),
		Clocks:           clocksOf(served.Spec),
		Binds:            bindsOf(served.Spec),
		Concurrent:       owned.TestModeInfo.Concurrent,
		LearnedAfterEnd:  m.learnedAfterEnd(m.servedAs(owned), served),
	}, 0); err != nil && m.logger != nil {
		m.logger.Error("failed to flag mock as used", zap.Error(err))
	}
	// Whichever door the parser consumed it through, the test's recording
	// answered a call of the set (FindAnsweredEarlier).
	m.noteAnswered(*owned)
	m.bumpConsumed(owned.Kind)
}

// cursorSlot is where a stateful cursor key is kept (cursorSlotIn): in the
// store-wide map under key, or, for gen > 0, in the cursors of the test view
// numbered gen (openTestCursors), which only calls read in that view reach,
// in force or ended (closeTestCursors).
type cursorSlot struct {
	key string
	gen uint64
}

// cursorSlotIn is the stateful cursor key names under the view v: with exact
// serving off (v nil) key itself, one sequence for the set; between tests the
// served pool slice's own, so two keys' calls never share a sequence; in a
// test the test view's own, so a reusable request recorded more than once
// answers in record order in the test, and a test opened again (a retry, or
// the next test) starts its sequences over. Where in its recordings a test's
// sequence starts is the test's recorded window (SequenceView.Span). Under the
// background rules a background recording that answers a call in a test is
// picked with the background's cursor (BackgroundCursor), the one this gives
// the calls between tests.
func cursorSlotIn(v *exactView, key string) cursorSlot {
	switch {
	case v == nil:
		return cursorSlot{key: key}
	case v.test != "":
		return cursorSlot{key: key, gen: v.gen}
	default:
		return cursorSlot{key: v.poolKey + "\x00" + key}
	}
}

// openTestCursors gives the test view numbered gen its cursors, empty, before
// the view is published (installExactWith): a call that reads the view finds
// them there.
func (m *MockManager) openTestCursors(gen uint64) {
	m.cursorMu.Lock()
	defer m.cursorMu.Unlock()
	if m.testCursors == nil {
		m.testCursors = make(map[uint64]map[string]int, 2)
	}
	m.testCursors[gen] = make(map[string]int)
}

// closeTestCursors closes the cursors of the test view numbered gen once
// another view replaced it, and keeps them: no other view reads them (a
// retry or the next test gets a view of its own number), so only a call
// read in the test (a held store's sequence view, one SequenceView) reaches
// them. Such a call walked after the test ended reads the position the
// test's calls reached and moves it (cursorIndexAt, advanceCursorAt), so
// its answer never depends on when the end lands. The next staging drops
// them (ResetStatefulCursors).
func (m *MockManager) closeTestCursors(gen uint64) {
	m.cursorMu.Lock()
	defer m.cursorMu.Unlock()
	if _, open := m.testCursors[gen]; !open {
		return
	}
	if m.closedTests == nil {
		m.closedTests = make(map[uint64]struct{}, 2)
	}
	m.closedTests[gen] = struct{}{}
}

// SequenceView is, under exact serving, the stateful cursor of the view in
// force now, resolved once, with the open test's recorded window
// (integrations.SequenceViewer): a peek and the commit after it through it
// move that view's sequence whichever view is in force when the commit lands.
// False with exact serving off.
func (m *MockManager) SequenceView() (integrations.SequenceView, bool) {
	v := m.exactView()
	if v == nil {
		return integrations.SequenceView{}, false
	}
	return m.sequenceViewOf(v), true
}

// sequenceViewOf is the sequence view of v (SequenceView), the view in force
// now or the one a call was read in (MarkCallRound). In a test that shows
// worker-scope mocks it tells them (Worker) with the cursor the calls between
// tests advance (Between), so a worker-scope request is one sequence for the
// set wherever its calls land: the worker scope belongs to no test.
func (m *MockManager) sequenceViewOf(v *exactView) integrations.SequenceView {
	sv := integrations.SequenceView{Cursor: viewCursor{m: m, v: v}, Test: v.test, Start: v.winStart, End: v.winEnd}
	if v.test != "" && len(v.worker) > 0 {
		sv.Worker = func(mk *models.Mock) bool { return mk != nil && !mk.IsProtocolPhase() && v.isWorker(mk.Name) }
		sv.Between = backgroundCursor{m: m}
	}
	return sv
}

// viewCursor is one view's stateful cursor (SequenceView).
type viewCursor struct {
	m *MockManager
	v *exactView
}

func (c viewCursor) MockCursorIndex(key string, n int) int {
	return c.m.cursorIndexAt(cursorSlotIn(c.v, key), n)
}

func (c viewCursor) AdvanceMockCursor(key string, servedIdx, n int) {
	c.m.advanceCursorAt(cursorSlotIn(c.v, key), servedIdx, n)
}

var _ integrations.SequenceViewer = (*MockManager)(nil)

// startupTree and unfilteredTree are the staged reusable trees now.
func (m *MockManager) startupTree() *TreeDb {
	m.treesMu.RLock()
	defer m.treesMu.RUnlock()
	return m.startup
}

func (m *MockManager) unfilteredTree() *TreeDb {
	m.treesMu.RLock()
	defer m.treesMu.RUnlock()
	return m.unfiltered
}

// treeValues is what tree holds, of kind (every kind for ""), in order.
func (m *MockManager) treeValues(tree *TreeDb, kind models.Kind) []*models.Mock {
	if tree == nil {
		return nil
	}
	out := make([]*models.Mock, 0, 32)
	tree.rangeValues(func(v interface{}) bool {
		if mk, ok := v.(*models.Mock); ok && mk != nil && (kind == "" || mk.Kind == kind) {
			out = append(out, mk)
		}
		return true
	})
	return out
}
