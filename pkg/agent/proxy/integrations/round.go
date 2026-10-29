package integrations

import (
	"context"
	"sort"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// ONE READ OF EXACT SERVING'S ROUND (the proven switch, record's full round).
//
// A learn step decides a call from what is open, owned, consumed, claimed and
// spent. Read from several of the store's accessors and the learn tracker one
// after another, those reads can each be of another instant: between two of
// them the store installs a view (a test's begin or end, a pool view after a
// pool view), the tracker begins or ends a test, or another connection
// consumes, spends or claims a recording. The reads then disagree, and a call
// is paired with a farther recording, its difference learned as noise: a
// false green. A RoundReader answers all of them in one read, under the one
// lock every such change takes (RoundSnapshot), and takes a claim, a spend
// or a consume only while nothing a snapshot decided on moved (WithinRound,
// ClaimPairingIn, SpendByKindIn), saying what moved when it refuses.

// RoundAt is the instant a RoundSnapshot was read at.
type RoundAt struct {
	// Epoch is the staging (MockManager.StagingEpoch).
	Epoch uint64
	// Gen is the round's generation. It moves at every view install (a
	// test's, a pool view's, a pool view after a pool view whatever it
	// shows) and at every boundary of the learn tracker (a test's begin or
	// end on it, a new set), before what moved is visible to any reader. It
	// never moves at a consume, a spend or a claim.
	Gen uint64
	// Bounds is the round's boundary count. It moves with Gen at every
	// view install and every boundary of the learn tracker, and at nothing
	// else: a test process's claim of its key again
	// (MockManager.RestartExactKey), which gives back what its key spent,
	// moves Gen alone. A call's mark is moved by it (CallRound.MovedSince).
	Bounds uint64
	// Taken is the length of the staging's take log (RoundSnapshot.Taken).
	Taken int
}

// RoundPhase is where the round is, told from the store's view and the learn
// tracker read together.
type RoundPhase uint8

const (
	// RoundStartUp: no test view was installed since exact serving began
	// (the application's start-up), and the tracker has no test open.
	RoundStartUp RoundPhase = iota + 1
	// RoundInTest: the store shows a test's view, and the tracker's open
	// test is that view's.
	RoundInTest
	// RoundBetween: the store shows a pool view and the tracker has no test
	// open (between tests, after the last).
	RoundBetween
	// RoundBoundary: the two moved apart. The store shows the pool view and
	// the tracker a test (a test's start before the store opened it, a
	// test's end before the tracker ended it), or the store shows a test's
	// view and the tracker none, or another test than the view's (a test
	// opened before the tracker began it, or after a test whose end never
	// reached the tracker). No recording is a test's own then: the tracker
	// cannot say which test a call is of. Where the store ended the test the
	// tracker still has open (RoundSnapshot.Ended), for its pool view or
	// another test's view, the round is no one's (RoundSnapshot.NoOneRound):
	// a learn step decides nothing of a call read there. Where the tracker
	// began a test the store has not opened (RoundSnapshot.TrackerAhead), a
	// call read on the store's pool view is the pool's
	// (RoundSnapshot.PoolsCall), and one read on the view of the test before
	// waits for the store to open the tracker's test. Where the store shows a
	// test's view and the tracker has no test open, or another test than the
	// one it ended, the call is of no one round to learn in (ViewAlone):
	// the view's test's, or the round between tests', and nothing tells
	// which. It is answered only by a recording it matches exactly, and a
	// learn step learns nothing from it, under either rule set: on a set
	// without the background rules the view's in-flight gap recordings and
	// its reached worker scope are the pool's there, and a call that is
	// exactly one of the view's own recordings, paired with them, would
	// learn what it differs in onto them as noise.
	RoundBoundary
)

// RoundOwner is whose a recording the view shows is, as the view holds it.
type RoundOwner uint8

const (
	// OwnerNone: no test's the view knows of (a protocol-phase recording,
	// the pool's, the worker scope's, another test's).
	OwnerNone RoundOwner = iota
	// OwnerTest: one of the open test's own (its mapping).
	OwnerTest
	// OwnerInFlight: one of the open test's own that was in flight when it
	// began, which the pool view before it served as such (its ahead).
	OwnerInFlight
	// OwnerShared: a recording the open test shares with the tests recorded
	// beside it (models.ExactServing.Concurrent), no other test took.
	OwnerShared
	// OwnerSharedTaken: a shared recording another test answered a call with
	// in the staging (RoundSnapshot.Claimant names it).
	OwnerSharedTaken
	// OwnerAhead: one of a pool view's ahead, the next test's calls in
	// flight when it began.
	OwnerAhead
	// OwnerSetAside: a recording of a test the replay set aside, served to
	// the open test's call that is it exactly (models.Mock.TestModeInfo.
	// SetAsideFrom): never the open test's own, and never learned against.
	OwnerSetAside
)

// TakeHow is what a take log entry records.
type TakeHow uint8

const (
	// TakeConsumed: one of the view's own answered (a test's own, or a pool
	// view's ahead: Take.Ahead).
	TakeConsumed TakeHow = iota + 1
	// TakeSpent: a delete door or a by-kind spend took a staged recording
	// for the staging (a call outside every test, the worker scope, the
	// background pool in a test), or, ForView, a protocol-phase one until the view
	// changes.
	TakeSpent
	// TakeClaimed: a pairing claimed another test's recording. A claim is
	// no consumption: the recording stays its owner's, whose view shows it
	// unconsumed until the owner's own call takes it; no pairing takes it
	// again in the staging.
	TakeClaimed
	// TakeCarried: a test view's install carried a pool view's ahead answer
	// into the test (Of: that answer's entry).
	TakeCarried
	// TakeVoided: a view install carried a pool view's ahead answer into no
	// test: the test it was answered for did not open next (Of: that
	// answer's entry). It answered a call of the round all the same.
	TakeVoided
	// TakeRestored: a take given back, its recording answering again (Of:
	// the entry it gives back): a spend for the staging, when a test
	// process claimed its pool slice's key again
	// (MockManager.RestartExactKey), a new scope; or what a test's earlier
	// view answered (its own, a shared recording, an ahead answer carried
	// into it), when a view of the same test is installed again in the
	// staging (a retry), which starts them afresh.
	TakeRestored
)

// Take is one entry of a staging's take log, written under the store's
// round lock in the step that takes the recording out of what a snapshot
// shows. Never written again: a later carry or void is an entry of its own.
type Take struct {
	// Mock is the recording taken: the view's copy of one of its own, or
	// the store's staged recording. Read-only.
	Mock *models.Mock
	How  TakeHow
	// Gen and Test are the view's the take was made in (for TakeCarried
	// and TakeVoided, the view that carried or voided it): its round
	// generation, and its test ("" for a pool view).
	Gen  uint64
	Test string
	// Ahead: a TakeConsumed of a pool view's ahead.
	Ahead bool
	// ForView: a TakeSpent that holds while the view it was made in (Gen)
	// is in force, not for the staging (a protocol-phase recording a
	// delete door evicted in a test): the next view shows it again.
	ForView bool
	// Of is, for TakeCarried and TakeVoided, the index of the ahead's
	// TakeConsumed entry, for TakeRestored the index of the TakeSpent or
	// TakeConsumed entry it gives back; -1 otherwise.
	Of int
}

// LearnRead is the learn tracker as a RoundSnapshot read it, under the same
// lock as the store (learn.Tracker.State and Test).
type LearnRead struct {
	// Bound: a tracker was read.
	Bound  bool
	Number uint64
	Open   bool
	Begun  bool
	// SetAhead: the tracker began a test set (learn.Tracker.NewSet) the
	// store has not staged yet, so the store still holds the set before's
	// staging, its pool view or a test's (RoundSnapshot.SetUnstaged).
	SetAhead bool
	// Earlier, Served and Before are learn.Tracker.Test's.
	Earlier        []learn.Earlier
	Served, Before []*models.Mock
	// Set and Order are learn.Tracker.Placed's: F's serving of the running
	// set, as the same read saw it.
	Set   []*models.Mock
	Order learn.Order
	// Awaiting: the running test still awaits its test case's request
	// (learn.Tracker.Places): a call that reads the round then learns
	// nothing from it until the request came (learn.Tracker.AwaitRequest).
	Awaiting bool
	// Answered is learn.Tracker.AnsweredMocks.
	Answered []*models.Mock
}

// RoundQuery is what a RoundSnapshot reads beside the round's own state.
type RoundQuery struct {
	// Kind is the kind of the background and the pairing's candidates read
	// ("" for none of either).
	Kind models.Kind
	// Tracker is the learn tracker read in the same lock (nil: none).
	Tracker *learn.Tracker
	// Pairing reads the pairing's candidates of Kind.
	Pairing bool
}

// DiskCandidate is a pairing candidate the agent keeps on disk, by name, not
// decoded under the lock: Load decodes it.
type DiskCandidate struct {
	Name string
	Load func() (*models.Mock, error)
}

// RoundSnapshot is one read of exact serving's round, made under the store's
// round lock (RoundReader.RoundSnapshot): every field is of one instant.
type RoundSnapshot struct {
	At RoundAt
	// Test is the store's open test, "" for a pool view; Ahead is a pool
	// view that answers the next test's calls in flight.
	Test  string
	Ahead bool
	// Fired: a test view was installed since exact serving began
	// (models.WindowSnapshot.FirstTestFired).
	Fired bool
	Phase RoundPhase
	// Boundary says, for RoundBoundary, which side moved.
	Boundary string
	// Ended is, for RoundBoundary where the store no longer shows the view
	// bound to the test the learn tracker still has open, that view's test
	// (its name): the store showed its pool view, or another test's view,
	// after it, and the tracker has not ended it. "" otherwise. A call read
	// there is of no one round (NoOneRound): the test's late call or the
	// next round's early one, and nothing tells which. It is the view last
	// bound to the tracker's test: where a view was opened and ended
	// without its test's begin reaching the tracker, an older test's view,
	// and one that survives a staging of the set again until the tracker's
	// next test. Read as the call's own (CallRound), its recordings are
	// held so either way, which only names fewer recordings made elsewhere.
	Ended string
	// Open is Phase == RoundInTest: the one answer a learn step reads to
	// whether a test is open, and whose recordings are its own.
	Open bool
	// Background: the set is served by the background rules.
	Background bool
	// Staging: the store's view is one the replay serves while it stages a
	// set's mocks, where no recording answers a call (CallRound.Staging).
	Staging bool
	// PerTest, Session, Startup and SessionScoped are what
	// GetPerTestMocksInWindow (and GetFilteredMocks), GetSessionMocks,
	// GetStartupMocks and GetSessionScopedMocks show, of every kind.
	PerTest, Session, Startup, SessionScoped []*models.Mock
	// Consumed are the view's own that answered, in the order they did: in
	// a test's view the ahead answers carried into it first
	// (ConsumedTestMocks); in a pool view its ahead that answered.
	Consumed []*models.Mock
	// Taken is the staging's take log, [0, At.Taken): shared, never written
	// again.
	Taken []Take
	Learn LearnRead
	// BackgroundUnused and BackgroundSpent are KindBackground(Kind).
	BackgroundUnused, BackgroundSpent []*models.Mock
	// Candidates are the pairing's candidates of Kind the store holds in
	// memory, in recorded order; OnDisk those the agent keeps on disk, by
	// name (LoadCandidates decodes them).
	Candidates []*models.Mock
	OnDisk     []DiskCandidate
	// OnDiskErr is what the store's read of the names it keeps on disk
	// returned: the candidates it did not read are in neither list.
	OnDiskErr error

	owners    map[string]RoundOwner
	claimants map[string]string
	worker    map[string]struct{}
	pool      map[string]struct{}
	bgPool    map[string]struct{}
	windows   *models.WindowSchedule
	poolNames *map[string]struct{}
	winStart  time.Time
	winEnd    time.Time
	viewGen   uint64
	endedOwn  map[string]struct{}
}

// RoundParts is what a store fills a RoundSnapshot's unexported answers
// from (NewRoundSnapshot): read under the same lock as the rest.
type RoundParts struct {
	Owners    map[string]RoundOwner
	Claimants map[string]string
	// Worker, Pool and BackgroundPool are the view's worker scope, its pool
	// slice and the background pool, by name; never written once read.
	Worker, Pool, BackgroundPool map[string]struct{}
	// Windows is the staging's recorded test windows; PoolNames the
	// replay's scope table (nil until sent); WindowStart and WindowEnd the
	// current test window.
	Windows                *models.WindowSchedule
	PoolNames              *map[string]struct{}
	WindowStart, WindowEnd time.Time
	// ViewGen is the generation the view in force was installed at (the
	// Gen of the takes made in it).
	ViewGen uint64
	// EndedOwn are, by name, the recordings the view of the test the store
	// ended (RoundSnapshot.Ended) held as its own; nil when Ended is "".
	EndedOwn map[string]struct{}
}

// WithParts is s answering from p (a store's, read with s).
func (s RoundSnapshot) WithParts(p RoundParts) RoundSnapshot {
	s.owners, s.claimants = p.Owners, p.Claimants
	s.worker, s.pool, s.bgPool = p.Worker, p.Pool, p.BackgroundPool
	s.windows, s.poolNames = p.Windows, p.PoolNames
	s.winStart, s.winEnd = p.WindowStart, p.WindowEnd
	s.viewGen = p.ViewGen
	s.endedOwn = p.EndedOwn
	return s
}

// NoOneRound says why a call read in s is of no one round, "" where it is of
// one: the store ended the test the learn tracker still has open (Ended),
// for its pool view or for another test's view (a test's end that never
// reached the tracker, and a begin that reached the store first). A call
// read there is the test's late call or the next round's early one, and
// nothing tells which. Read as either, a call that is exactly one test's own
// recording was paired with another round's, and what it differs in learned
// onto it as noise. A learn step decides nothing of a call read where
// NoOneRound is not "": the call misses, with no reason, saying why, and
// nothing is learned or answered from that read, whatever the replay does
// next. The tracker ahead of the store is no such round (TrackerAhead): on
// the store's pool view the call is the pool's (PoolsCall), and on the view
// of the test before it is of the tracker's test, whose view the store has
// not installed yet, and waits for it. Where the store shows a test's view
// and the tracker has no test open (the tracker ended a test whose end did
// not install the pool view), "" is returned: the call is answered from the
// store's view by a recording it matches exactly, and learns nothing
// (ViewAlone). A call
// read in a test set the store has not staged (SetUnstaged) is of no round
// of either set: decided at once in none.
func (s RoundSnapshot) NoOneRound() string {
	if s.SetUnstaged() {
		return "as keploy's learn tracker began a test set its store had not staged, so the call is of no round of either test set"
	}
	if s.Phase != RoundBoundary || !s.Learn.Bound || !s.Learn.Open || s.Ended == "" {
		return ""
	}
	if s.Test == "" {
		return "after keploy's store ended " + s.Ended + " and before its learn tracker did, so the call is of neither " + s.Ended + " nor the round between tests"
	}
	return "after keploy's store ended " + s.Ended + " for " + s.Test + "'s view and before its learn tracker ended " + s.Ended + ", so the call is of neither " + s.Ended + " nor " + s.Test
}

// ViewAlone says why a call read in s is of no one round to learn in, "" where
// it is of one: the store shows a test's view and the learn tracker has no
// test open. The agent's flows begin the tracker's test before they install
// its view (keploy test's loops, and keploy mock's scope flow too:
// ProvenBegin opens the test's window before BeginScopeAt), so the store
// never opens a test first; the state arises where a test's end did not
// install the pool view and the tracker then ended the test (the scope
// flow's end, warned as it could not stop serving the test, or keploy test's
// end, failing with serveExactPool's error). The call is the view's test's
// (made before the tracker's end) or the round between tests', and nothing
// tells which. It is still served from the
// store's view where it matches one of its recordings exactly, but a learn
// step learns nothing from it, on its first pass or any later one (a pass
// read again once a take of it was refused, the tracker caught up with the
// store's test): it misses, with no reason, saying why, and never waits for
// the tracker, whose begin the agent's settle may itself wait for. Read as
// the pool's, a call that is exactly one of the view's own recordings was
// paired with the pool's (on a set without the background rules, the view's
// in-flight gap recordings and its reached worker scope), and what it
// differs in learned onto them as noise: a false green. A call read once the
// tracker began the view's test is of that test, and learns as any other.
// The store ahead of the tracker mirrors NoOneRound, the store behind it
// (it ended the test the tracker still has open); a test set the store has
// not staged is NoOneRound's, not this.
func (s RoundSnapshot) ViewAlone() string {
	if s.Phase != RoundBoundary || !s.Learn.Bound || s.Learn.Open || s.Learn.SetAhead || s.Test == "" {
		return ""
	}
	return "while keploy's store showed " + s.Test + "'s view and its learn tracker had no test open, so the call is of neither " + s.Test + " nor the round between tests to learn in"
}

// TrackerAhead reports whether s found the learn tracker ahead of the
// store: the tracker began a test (it is open, at its number) whose view the
// store has not installed, and the store shows its pool view or the view of
// the test before, bound to the tracker's test before. Its mark (CallRound)
// holds every recording as its own: the store cannot tell which the
// tracker's test owns, so a miss of it names none made elsewhere. What the
// call is then depends on the view in force:
//   - the store's pool view (PoolsCall): keploy test's begin of every test
//     under exact serving, from the tracker's begin to the install of the
//     test's view. The call is the pool's, decided at once in the round
//     between tests, and never waits for the store;
//   - the view of the test before, which keploy test's flow never leaves in
//     force once the tracker began the next test (a test's end installs the
//     pool view first): the call is of the tracker's test, made before the
//     store caught up, and a learn step decides nothing of it until the
//     store opens that test (then in that test's round, its mark read again
//     there) or the test's window closes (the tracker's End, its next
//     Begin, a new set: the moved miss), waiting for it through no clock
//     (RoundWaiter).
//
// The tracker's begin of a test set the store has not staged is no such
// state (SetUnstaged): no call read there waits.
func (s RoundSnapshot) TrackerAhead() bool {
	return s.Phase == RoundBoundary && s.Learn.Bound && s.Learn.Open && s.Ended == "" && !s.Learn.SetAhead
}

// SetUnstaged reports whether s found the learn tracker in a test set the
// store has not staged (LearnRead.SetAhead): keploy test's Mock of the next
// set began it (learn.Tracker.NewSet) and the store still holds the set
// before's staging, its pool view (the set's last test ended) or a test's
// view. No recording of either set answers a call read there: the set
// before's are not the next set's, and the next set's are not staged. It is
// the call the staging note describes (a call the application makes between
// test sets), decided at once, never waiting for the staging: its mark is a
// staging's, outside every test, holding every recording as its own
// (CallRound), so its miss says it was refused as the set's mocks were
// staged and names no recording made elsewhere, and a learn step decides
// nothing of it (NoOneRound). A wait would end at the staging, whose cut of
// the misses made before it (the proxy's set start) the moved miss would
// race; a miss decided at once is counted the same whichever way its report
// and the cut fall (its mark's staging precedes the set's).
func (s RoundSnapshot) SetUnstaged() bool {
	return s.Phase == RoundBoundary && s.Learn.Bound && s.Learn.SetAhead
}

// PoolsCall reports whether s found the learn tracker ahead of the store
// (TrackerAhead) while the store shows its pool view: under exact serving,
// keploy test's begin of every test, from the tracker's begin (the test's
// capture window opens before its mock filter) to the install of the test's
// view, which the agent makes only once every call made before is answered
// (its settle), and only then gives the application the test's request. No
// call read there is the tracker's test's: it is the pool's, decided at once
// in the round between tests. It never waits for the store to open the
// test: the install it would wait for waits for it, as the settle waits for
// every call in the proxy's hands (a deadlock: the settle's watchdog fails
// the test's open). It waits only for its test case's request, as any call
// read before the request was given does (learn.Tracker.AwaitRequest), which
// the agent gives before it settles: the call reads the round with the
// request either way.
func (s RoundSnapshot) PoolsCall() bool {
	return s.TrackerAhead() && s.Test == ""
}

// Owner is whose m is, as the view held it at the snapshot (by name).
func (s RoundSnapshot) Owner(m *models.Mock) RoundOwner {
	if m == nil {
		return OwnerNone
	}
	return s.owners[m.Name]
}

// CallRound is s, a store's snapshot of the round, as a call's first read
// of it (CallRound): its staging, and every recording its view held as its
// own (Owner is not OwnerNone), whether that view was a staging's, a
// test's (Test), and one of a set served by proven routing (Background).
// Read where the store ended a test the learn tracker still has open
// (Ended), the call may be that test's late one: the recordings that test's
// view held are held as the call's own too, so no lookup names one as made
// elsewhere, nor pairs the call with one as another test's.
//
// Read in a test set the store has not staged (SetUnstaged), the mark is a
// staging's, outside every test, and holds every recording: the call the
// staging note describes, whose miss names no recording made elsewhere.
//
// Read where the learn tracker is ahead of the store (TrackerAhead), every
// recording is held as the call's own: the tracker's test's view is not
// installed, so nothing tells which recordings are that test's own, the
// pool's call (PoolsCall) among them, and a lookup that named one made elsewhere would let
// record's judging bake a test's own recording into the worker scope. It
// names none, and pairs none, which only fails closed.
func (s RoundSnapshot) CallRound() CallRound {
	if s.SetUnstaged() {
		return NewCallRound(s.At.Epoch, HoldsEvery).ReadAt(s.At).InStaging(true).InView(false, s.Background)
	}
	if s.TrackerAhead() {
		return NewCallRound(s.At.Epoch, HoldsEvery).ReadAt(s.At).InStaging(s.Staging).InView(s.Test != "", s.Background)
	}
	owners, ended := s.owners, s.endedOwn
	return NewCallRound(s.At.Epoch, func(name string) bool {
		_, own := ended[name]
		return own || owners[name] != OwnerNone
	}).ReadAt(s.At).InStaging(s.Staging).InView(s.Test != "", s.Background)
}

// HoldsEvery holds every recording as a call's own (NewCallRound): the mark
// of a call whose round's own recordings the store cannot tell
// (RoundSnapshot.TrackerAhead). Its miss names no recording made elsewhere.
func HoldsEvery(string) bool { return true }

// Claimant is the test that took the shared recording m (OwnerSharedTaken).
func (s RoundSnapshot) Claimant(m *models.Mock) (string, bool) {
	if m == nil {
		return "", false
	}
	t, ok := s.claimants[m.Name]
	return t, ok
}

// OwnedByOpenTest reports whether m is the open test's own at the snapshot
// (TestOwner): a test is open (Open) and m is of its mapping, in flight
// when it began or not.
func (s RoundSnapshot) OwnedByOpenTest(m *models.Mock) bool {
	o := s.Owner(m)
	return s.Open && (o == OwnerTest || o == OwnerInFlight)
}

// InFlightIntoOpenTest reports whether m was in flight when the open test
// began, at the snapshot (InFlightOwner).
func (s RoundSnapshot) InFlightIntoOpenTest(m *models.Mock) bool {
	return s.Open && s.Owner(m) == OwnerInFlight
}

// InTestWindow, InRecordedTestWindow and SameRecordedTestWindow are the
// store's TestWindows, of the staging and the test window the snapshot read.
func (s RoundSnapshot) InTestWindow(t time.Time) bool {
	if s.winStart.IsZero() || s.winEnd.IsZero() || t.IsZero() {
		return false
	}
	return !t.Before(s.winStart) && !t.After(s.winEnd)
}

// RecordedTestAt is the test case whose recorded window t is inside, of the
// staging's recorded test windows (the earliest-starting one), "" where t is
// inside none, the replay sent none, or the window carries no name.
func (s RoundSnapshot) RecordedTestAt(t time.Time) string {
	if s.windows.Len() == 0 || t.IsZero() {
		return ""
	}
	if i := s.windows.IndexOf(t); i >= 0 {
		return s.windows.Window(i).TestCase
	}
	return ""
}

// BeforeTestWindow implements RecordedTests: t is before the start of the
// recorded window of the view's test (the staging's recorded windows, by test
// case), else of the test window the snapshot read.
func (s RoundSnapshot) BeforeTestWindow(t time.Time) (before, known bool) {
	if t.IsZero() {
		return false, false
	}
	if s.Test != "" {
		for i := 0; i < s.windows.Len(); i++ {
			if w := s.windows.Window(i); w.TestCase == s.Test && !w.Start.IsZero() {
				return t.Before(w.Start), true
			}
		}
	}
	if s.winStart.IsZero() {
		return false, false
	}
	return t.Before(s.winStart), true
}

var _ RecordedTests = RoundSnapshot{}

func (s RoundSnapshot) InRecordedTestWindow(t time.Time) (inside, known bool) {
	if s.windows.Len() == 0 || t.IsZero() {
		return false, false
	}
	return s.windows.Contains(t), true
}

func (s RoundSnapshot) SameRecordedTestWindow(a, b time.Time) (same, known bool) {
	if s.windows.Len() == 0 || a.IsZero() || b.IsZero() {
		return false, false
	}
	i := s.windows.IndexOf(a)
	return i >= 0 && i == s.windows.IndexOf(b), true
}

// InPool is the store's PoolMembership (the replay's scope table) at the
// snapshot.
func (s RoundSnapshot) InPool(m *models.Mock) (pool, known bool) {
	if s.poolNames == nil || m == nil {
		return false, false
	}
	_, pool = (*s.poolNames)[m.Name]
	return pool, true
}

// IsWorker reports whether name is one of the worker scope's recordings the
// view shows; InPoolSlice whether it is in a pool view's pool slice;
// InBackgroundPool whether it is the background pool's.
func (s RoundSnapshot) IsWorker(name string) bool {
	_, ok := s.worker[name]
	return ok
}

func (s RoundSnapshot) InPoolSlice(name string) bool {
	_, ok := s.pool[name]
	return ok
}

func (s RoundSnapshot) InBackgroundPool(name string) bool {
	_, ok := s.bgPool[name]
	return ok
}

var (
	_ TestWindows    = RoundSnapshot{}
	_ InFlightOwner  = RoundSnapshot{}
	_ TestOwner      = RoundSnapshot{}
	_ PoolMembership = RoundSnapshot{}
)

// ExactBackground reports whether, at the snapshot, the background
// answers a call exactly or not at all, and no drift is learned onto it: the
// set is served by the background rules and the store shows a test's view,
// whether the learn tracker has that test open (RoundInTest) or not
// (RoundBoundary). The pool is learned onto between tests only.
func (s RoundSnapshot) ExactBackground() bool {
	return s.Background && s.Test != ""
}

// ViewGen is the generation the view in force at the snapshot was installed
// at (RoundParts.ViewGen): it tells one view from the next, a pool view
// after a pool view too, where At.Gen moves at the learn tracker's
// boundaries as well.
func (s RoundSnapshot) ViewGen() uint64 { return s.viewGen }

// TakenInView is what the open view's own answered, carried ahead answers
// included, in the order they did (Consumed): in a test, what competes as
// spent. None in a pool view.
func (s RoundSnapshot) TakenInView() []*models.Mock {
	if s.Test == "" {
		return nil
	}
	return s.Consumed
}

// Voided are the ahead answers of the staging that no test view carried
// (TakeVoided), in the order they were voided: each answered a call of the
// round.
func (s RoundSnapshot) Voided() []*models.Mock {
	var out []*models.Mock
	for _, t := range s.Taken {
		if t.How == TakeVoided {
			out = append(out, t.Mock)
		}
	}
	return out
}

// AnsweredInRound reports whether name answered a call of the round in the
// staging and answers none again: by a consume (an ahead answer included,
// carried or voided) or a spend that no TakeRestored gave back (a retried
// test's, a restarted key's: each gives back the one entry it names). A
// spend for the view alone (ForView) counts while that view is in force. A
// pairing's claim is no answer of the recording's own (ClaimedInRound).
//
// With it, every data-plane recording of the staging is, at one snapshot,
// in at most one of: shown unconsumed (PerTest, Session, ...), the
// pairing's candidates (Candidates, OnDisk), the background's unused
// (BackgroundUnused), or answered. Two kinds are in none: a claimed
// recording its owner's view does not show, and a retried test's own its
// retry did not answer again (the replay's account of its first answer
// keeps it from the candidates). One is in two, by design: a voided ahead
// answer (Voided) a later view of the test it named shows, which the store
// serves again as before, and a learn step counts as spent.
func (s RoundSnapshot) AnsweredInRound(name string) bool {
	// The entries of name that answered, and are not given back.
	live := map[int]bool{}
	for i, t := range s.Taken {
		if t.Mock == nil || t.Mock.Name != name {
			continue
		}
		switch t.How {
		case TakeConsumed:
			live[i] = true
		case TakeSpent:
			if !t.ForView || t.Gen == s.viewGen {
				live[i] = true
			}
		case TakeRestored:
			delete(live, t.Of)
		}
	}
	return len(live) > 0
}

// ClaimedInRound reports whether a pairing claimed name in the staging
// (TakeClaimed): no pairing takes it again. The claim is no consumption: a
// view of its owner shows it unconsumed, until the owner's call takes it.
func (s RoundSnapshot) ClaimedInRound(name string) bool {
	for _, t := range s.Taken {
		if t.How == TakeClaimed && t.Mock != nil && t.Mock.Name == name {
			return true
		}
	}
	return false
}

// CandidateError is a pairing candidate the store keeps on disk that could
// not be read (RoundSnapshot.LoadCandidates): Name is the recording's, ""
// for the store's read of the names it keeps (OnDiskErr).
type CandidateError struct {
	Name string
	Err  error
}

func (e CandidateError) Error() string {
	if e.Name == "" {
		return "the recordings kept on disk: " + e.Err.Error()
	}
	return "recording " + e.Name + ": " + e.Err.Error()
}

func (e CandidateError) Unwrap() error { return e.Err }

// LoadCandidates is the pairing's candidates of the snapshot's kind, those
// the store holds and those decoded from disk (OnDisk), in recorded order,
// decoded outside the store's lock. A recording a load fails on is left out
// and its error returned with its name, as is the store's own read of the
// names (OnDiskErr): a call that is one pairs with none. A claim of any of
// them (ClaimPairingIn) checks again that it may still be taken.
func (s RoundSnapshot) LoadCandidates(kind models.Kind) ([]*models.Mock, []CandidateError) {
	out := append([]*models.Mock(nil), s.Candidates...)
	var errs []CandidateError
	if s.OnDiskErr != nil {
		errs = append(errs, CandidateError{Err: s.OnDiskErr})
	}
	for _, d := range s.OnDisk {
		mk, err := d.Load()
		if err != nil {
			errs = append(errs, CandidateError{Name: d.Name, Err: err})
			continue
		}
		if mk != nil && mk.Kind == kind && !mk.IsProtocolPhase() && !mk.IsAsync() {
			out = append(out, mk)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Spec.ReqTimestampMock.Before(out[j].Spec.ReqTimestampMock)
	})
	return out, errs
}

// RoundScope is a store whose consume doors settle only while the store is
// at the RoundAt it was made for (RoundReader.WithinRound). Every other
// method is the store's.
type RoundScope interface {
	MockMemDb
	// Moved says why the last refused consume was refused for a move
	// (the staging, the view, the tracker's test), "" when none was.
	Moved() string
}

// RoundReader is an optional MockMemDb capability under exact serving: one
// read of the round, and takes checked against it. The agent's MockManager
// implements it, and every MockMemDb wrapper that forwards its store must
// forward it.
type RoundReader interface {
	// RoundSnapshot reads the round now; false while exact serving is off.
	RoundSnapshot(q RoundQuery) (RoundSnapshot, bool)
	// WithinRound is the store as a RoundScope for at.
	WithinRound(at RoundAt) RoundScope
	// ClaimPairingIn is ClaimPairing that claims m only while the store is
	// still at at's staging and generation: moved, and nothing claimed, when
	// either moved. why says what refused it.
	ClaimPairingIn(m *models.Mock, at RoundAt) (claimed, moved bool, why string)
	// SpendByKindIn is SpendByKind that spends m only while the store is
	// still at at's staging and generation: moved, and nothing spent, when
	// either moved. why says what refused it.
	SpendByKindIn(m *models.Mock, at RoundAt) (spent, moved bool, why string)
	// TakenLog is entries [from, to) of staging epoch's take log, nil for a
	// staging the store left.
	TakenLog(epoch uint64, from, to int) []Take
}

// RoundWaiter is an optional RoundReader capability: a wait, through no
// clock, for the round to move. A learn step whose call it read where the
// learn tracker is ahead of a store still showing the view of the test
// before (RoundSnapshot.TrackerAhead, not PoolsCall) waits on it for the
// store to open the tracker's test, or for that test's window to close. A
// call of the pool's (PoolsCall) never waits on it: in keploy test's flow
// the install it would wait for waits for the call. Whether a waiter is
// scheduled before the store ends the test it waited for is the scheduler's:
// one woken only after that end reads the round moved on and gets the moved
// miss, never a verdict of another round (accepted as fail-closed, as
// learn.Tracker.AwaitRequest's). The agent's MockManager implements it, and every MockMemDb wrapper
// that forwards RoundReader must forward it.
type RoundWaiter interface {
	// AwaitRoundMove waits until the round moves from at: its generation
	// (RoundAt.Gen: a view's install, a boundary of the learn tracker) or
	// its staging. moved is true once it moved, at once where it moved
	// already; false, at once, where the store cannot tell the move (no
	// exact serving, a tracker whose boundaries never reach it, a store
	// closed); err is ctx's where it ended first.
	AwaitRoundMove(ctx context.Context, at RoundAt) (moved bool, err error)
}

// AwaitRoundMove is db's AwaitRoundMove; false, at once, when db does not
// implement RoundWaiter.
func AwaitRoundMove(ctx context.Context, db MockMemDb, at RoundAt) (moved bool, err error) {
	if w, ok := db.(RoundWaiter); ok {
		return w.AwaitRoundMove(ctx, at)
	}
	return false, nil
}

// SnapshotRound is db's RoundSnapshot; false when db does not implement
// RoundReader, or exact serving is off.
func SnapshotRound(db MockMemDb, q RoundQuery) (RoundSnapshot, bool) {
	if r, ok := db.(RoundReader); ok {
		return r.RoundSnapshot(q)
	}
	return RoundSnapshot{}, false
}

// WithinRound is db's RoundScope for at; nil when db does not implement
// RoundReader.
func WithinRound(db MockMemDb, at RoundAt) RoundScope {
	if r, ok := db.(RoundReader); ok {
		return r.WithinRound(at)
	}
	return nil
}

// ClaimPairingIn is db's ClaimPairingIn; nothing claimed, with why, when db
// does not implement RoundReader.
func ClaimPairingIn(db MockMemDb, m *models.Mock, at RoundAt) (claimed, moved bool, why string) {
	if r, ok := db.(RoundReader); ok {
		return r.ClaimPairingIn(m, at)
	}
	return false, false, "the mock store reads no round"
}

// SpendByKindIn is db's SpendByKindIn; nothing spent, with why, when db
// does not implement RoundReader.
func SpendByKindIn(db MockMemDb, m *models.Mock, at RoundAt) (spent, moved bool, why string) {
	if r, ok := db.(RoundReader); ok {
		return r.SpendByKindIn(m, at)
	}
	return false, false, "the mock store reads no round"
}

// RoundMoved is why a consume through scope (WithinRound) was refused for a
// move of the round, "" when none was (and for a nil scope).
func RoundMoved(scope MockMemDb) string {
	if s, ok := scope.(RoundScope); ok && s != nil {
		return s.Moved()
	}
	return ""
}
