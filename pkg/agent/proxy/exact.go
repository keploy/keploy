package proxy

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
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
//     answered finds no test mock: a miss (models.MissExtraCall), unless a
//     worker-scope mock the run has reached (below) is that request.
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
	poolKey string
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
	// the staging's (outside), not the view's.
	spent map[string]bool
	// outside is the manager's outsideAnswers, and epoch its stagingEpoch.
	outside *outsideAnswers
	epoch   *atomic.Uint64

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

	// heard is every mock a parser was served while the view was installed,
	// as the served observer was told of it (observeServed): of any lifetime
	// (the test's own, a protocol-phase or reusable one, one spent through a
	// delete door, a worker-scope one), through any door, a serve whose
	// claim then failed included, once each by identity (learn.MockID,
	// heardIDs), in the order they came. Kept only while there is an
	// observer (the proven switch), and read only of a test view
	// (ServedInOpenTest).
	heard    []*models.Mock
	heardIDs map[string]bool
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
	// fired: a test has begun since exact serving was turned on. It is what
	// tells a tier-routing parser (Postgres v3) "between tests" from
	// "before the first test".
	fired atomic.Bool
	// last is the latest test view, open or ended: its outcome is read after
	// the test stops being served (ExactTestOutcome).
	last atomic.Pointer[exactView]
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
	// staging is set while a set's mocks are staged: from its staging's
	// StageExactBackground to its ServeExactBackground (exact_background.go).
	// A view installed meanwhile is a staging view (exactView.staging).
	staging atomic.Bool
	// poolCursor is the key of the pool view installed last: the cursors
	// the calls between tests advance are under it (exactCursorKey), and so
	// are the background's (BackgroundCursor).
	poolCursor atomic.Pointer[string]
	// pairs says the session pairs (SetPairing); claimed, the recordings a
	// pairing claimed in the current staging; ofKind, where the pairing
	// reads recordings of a kind the trees do not hold.
	pairs   atomic.Bool
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
// staging answer again, and key's stateful cursors (exactCursorKey) start
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
func (m *MockManager) MarkCallRound() integrations.CallRound {
	r := &m.exact.round
	r.mu.Lock()
	v := m.exact.view.Load()
	epoch := m.stagingEpoch.Load()
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
		return integrations.NewCallRound(epoch, integrations.HoldsEvery).InStaging(true).InView(false, v.bg != nil)
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
	return integrations.NewCallRound(epoch, held).InStaging(v.staging).InView(v.test != "", v.bg != nil)
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

// findNotOwned is FindStagedNotOwned, and with keys FindStagedNotOwnedByKeys.
// The staging only moves on, and moves before the trees change (a staging
// bumps it first, holding swapMu), so a recording read from the trees while
// the staging is still call's is of call's set.
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
	for _, t := range trees {
		for _, mk := range m.treeValues(t, kind) {
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
	id := func(mk *models.Mock) string {
		return mk.Name + "\x00" + string(mk.Kind) + "\x00" + strconv.FormatInt(mk.Spec.ReqTimestampMock.UnixNano(), 10)
	}
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
	v.outside, v.epoch = &m.exact.outside, &m.stagingEpoch
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
	r.bump("a view was installed")
	v.round = r.gen
	if v.test != "" {
		r.bindLocked(v)
	}
	if under != nil {
		under(r)
	}
	if m.exact.beforePublish != nil {
		m.exact.beforePublish(v)
	}
	prev := m.exact.view.Swap(v)
	if prev != nil {
		prev.mu.Lock()
		prev.closed = true
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
	for k := range kinds {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
	m.signalWindowChange()
	// Only now, with v in force and the revisions moved, do the consumes
	// and spends that found prev closed go on, against v.
	if prev != nil {
		prev.markReplaced()
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

// stagedKinds is every kind the staged trees hold or have held.
func (m *MockManager) stagedKinds() map[models.Kind]struct{} {
	kinds := map[models.Kind]struct{}{}
	m.treesMu.RLock()
	for k := range m.filteredByKind {
		kinds[k] = struct{}{}
	}
	for k := range m.unfilteredByKind {
		kinds[k] = struct{}{}
	}
	startup := m.startup
	m.treesMu.RUnlock()
	if startup != nil {
		startup.rangeValues(func(v interface{}) bool {
			if mk, ok := v.(*models.Mock); ok && mk != nil {
				kinds[mk.Kind] = struct{}{}
			}
			return true
		})
	}
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
	v := m.exact.last.Load()
	if v == nil {
		return "", nil, nil, true
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	// Only the test's own: a shared recording is never required of it.
	for _, i := range v.order {
		if !v.isCand(i) {
			consumed = append(consumed, v.owned[i].Name)
		}
	}
	for i, done := range v.consumed {
		if !done && !v.isCand(i) {
			unconsumed = append(unconsumed, v.owned[i].Name)
		}
	}
	return v.test, consumed, unconsumed, true
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
// began (ServeExactPool's ahead).
func (v *exactView) unanswered(kind models.Kind, sessionOnly ...bool) []*models.Mock {
	if len(v.owned) == 0 {
		return nil
	}
	only := len(sessionOnly) > 0 && sessionOnly[0]
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]*models.Mock, 0, len(v.owned))
	for i, mk := range v.owned {
		if !v.consumed[i] && (kind == "" || mk.Kind == kind) && (!only || v.session[i]) && !v.takenByOther(i) {
			out = append(out, mk)
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
	v.mu.Lock()
	spent := v.spent[mk.Name]
	v.mu.Unlock()
	if spent || v.spentOutside(mk.Name) {
		return false
	}
	if mk.IsProtocolPhase() {
		return true
	}
	if v.isWorker(mk.Name) {
		return true
	}
	if v.test != "" {
		// Under the background rules the pool answers in a test too,
		// after the test's own and the worker scope (keep); an async
		// lane's recording keeps today's rules.
		return v.bg.inPool(mk.Name) && !mk.IsAsync()
	}
	_, ok := v.pool[mk.Name]
	return ok
}

// inBackgroundPool reports whether mk, consumed in an open test, is one of
// the pool's data-plane recordings the background rules show there.
func (v *exactView) inBackgroundPool(mk *models.Mock) bool {
	return v.test != "" && v.bg.inPool(mk.Name) && !mk.IsProtocolPhase() && !mk.IsAsync()
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

// keep narrows staged mocks to those the view shows, in their order.
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
		gone := mk != nil && (v.spent[mk.Name] || v.spentOutside(mk.Name))
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

func (v *exactView) keep(in []*models.Mock) []*models.Mock {
	out := make([]*models.Mock, 0, len(in))
	if v.bg != nil {
		return v.backgroundOrder(out, in)
	}
	for _, mk := range in {
		if v.shows(mk) {
			out = append(out, mk)
		}
	}
	return out
}

// backgroundOrder appends to out what the view shows of in, in the
// background order: what is not background first (the
// protocol-phase mocks), in their order; then the worker scope, the earlier
// place first, delivering before idle, then recorded order (workerOrder);
// then the pool, in recorded order, whichever trees in came from.
func (v *exactView) backgroundOrder(out, in []*models.Mock) []*models.Mock {
	var worker, pool []*models.Mock
	for _, mk := range in {
		if !v.shows(mk) {
			continue
		}
		switch {
		case mk.IsProtocolPhase():
			out = append(out, mk)
		case v.isWorker(mk.Name):
			worker = append(worker, mk)
		default:
			pool = append(pool, mk)
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
// mocks of kind, then what the per-test tree holds that the view shows.
func (v *exactView) perTest(kind models.Kind, staged []*models.Mock) []*models.Mock {
	return append(v.unanswered(kind), v.keep(staged)...)
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
	out := v.unanswered(kind, v.test != "")
	seen := make(map[*models.Mock]struct{}, len(out))
	if v.bg != nil {
		// One order across the tiers (backgroundOrder), each mock once.
		var all []*models.Mock
		for _, list := range staged {
			for _, mk := range list {
				if _, dup := seen[mk]; dup || (kind != "" && mk.Kind != kind) {
					continue
				}
				seen[mk] = struct{}{}
				all = append(all, mk)
			}
		}
		return v.backgroundOrder(out, all)
	}
	for _, list := range staged {
		for _, mk := range v.keep(list) {
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
//
// A consume that finds its view closed (a begin or an end replacing it) is no
// refusal: it waits until the replacing view is installed and the revisions
// moved, and is settled against that one (closedWait), as one that came a
// moment later would be.
func (m *MockManager) exactConsume(mk *models.Mock, evicts bool) exactVerdict {
	return m.exactConsumeAt(mk, evicts, nil, nil)
}

// exactConsumeAt is exactConsume that tells the served observer (observe,
// when not nil, once) in the step that settles the consume, and, with g,
// settles it only while the store is still at g's round (roundGuard): a
// consume decided on a snapshot never lands in another staging, view or
// learn test, and one refused for that tells the observer nothing. Each
// settle holds the round lock (exact_round.go), as every snapshot does, so a
// snapshot sees a consume, its take and the observer's account of it all,
// or none of them.
func (m *MockManager) exactConsumeAt(mk *models.Mock, evicts bool, g *roundGuard, observe func()) exactVerdict {
	return settleInForce(m, exactRefused, func(v *exactView) (exactVerdict, <-chan struct{}) {
		if v != nil && mk != nil && (evicts || v.isWorker(mk.Name)) {
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
		// follows (where the round moved): either way before the served
		// observer is told, of a mock nothing served.
		if ok, wait := m.guardLocked(g, v); !ok {
			return exactRefused, wait
		}
		if observe != nil {
			observe()
			observe = nil
		}
		verdict, wait, after := m.exactConsumeIn(v, mk, evicts)
		m.exact.round.mu.Unlock()
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
				return exactServed, nil, func() {
					m.bumpRevisionKind(owned.Kind)
					m.bumpRevisionAll()
				}
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
	// MarkMockAsUsed) spends it too.
	worker := v.isWorker(mk.Name) && !mk.IsProtocolPhase()
	if !evicts && !worker {
		return exactPass, nil, nil
	}
	// A data-plane recording consumed through a delete door with no test
	// open, and one of the worker scope's whether a test is open or not, is
	// spent for the staging: it answers at most one request in the set. So
	// is one of the pool's a test consumes under the background rules:
	// the background is the set's, whoever's call it answers.
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
	if v.spent[mk.Name] {
		v.mu.Unlock()
		return exactRefused, nil, nil
	}
	if v.spent == nil {
		v.spent = map[string]bool{}
	}
	v.spent[mk.Name] = true
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

// exactConsumeValueAt is exactConsumeAt for a door that takes the mock by
// value, which tells the served observer of a copy of its own when observe
// is set: first, with exact serving off; in the step that settles the
// consume, holding the round lock, under it. Its copy is made only under
// exact serving: a door taking the mock's address would move every
// consume's mock to the heap, switch on or off. With g, exact serving off
// refuses the consume, telling the observer nothing: no round is read
// without it.
func (m *MockManager) exactConsumeValueAt(mock models.Mock, evicts bool, g *roundGuard, observe bool) exactVerdict {
	if m.exactView() == nil {
		if g.checks() {
			g.refuse("exact serving is off")
			return exactRefused
		}
		if observe {
			m.observeServedCopy(mock, g.flight())
		}
		return exactPass
	}
	mk := mock
	var tell func()
	if observe {
		tell = func() { m.observeServedCopy(mk, g.flight()) }
	}
	return m.exactConsumeAt(&mk, evicts, g, tell)
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
		Binds:            bindsOf(mk.Spec),
	}, 0); err != nil && m.logger != nil {
		m.logger.Error("failed to flag mock as used", zap.Error(err))
	}
	m.bumpRevisionKind(mk.Kind)
	m.bumpRevisionAll()
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
		Binds:            bindsOf(served.Spec),
		Concurrent:       owned.TestModeInfo.Concurrent,
	}, 0); err != nil && m.logger != nil {
		m.logger.Error("failed to flag mock as used", zap.Error(err))
	}
	// Whichever door the parser consumed it through, the test's recording
	// answered a call of the set (FindAnsweredEarlier).
	m.noteAnswered(*owned)
	m.bumpRevisionKind(owned.Kind)
	m.bumpRevisionAll()
}

// exactCursorKey is the stateful cursor a key names under exact serving: the
// served pool slice's own, so two keys' calls never share a sequence. In a
// test no cursor is used (ok false): the test's mocks answer once each, in
// recorded order, and under today's rules the pool serves no data-plane mock
// there. Under the background rules a background recording that answers a
// call in a test is picked with the background's cursor (BackgroundCursor),
// the one this gives the calls between tests.
func (m *MockManager) exactCursorKey(key string) (string, bool) {
	v := m.exactView()
	if v == nil {
		return key, true
	}
	if v.test != "" {
		return "", false
	}
	return v.poolKey + "\x00" + key, true
}

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
