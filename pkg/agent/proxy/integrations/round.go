package integrations

import (
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
	// cannot say which test a call is of. Where the store shows the pool
	// view, a learn step reads the round as between tests; where it shows a
	// test's view, the background answers exactly or not at all, as in a
	// test (ExactBackground), so nothing is learned onto it.
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
)

// TakeHow is what a take log entry records.
type TakeHow uint8

const (
	// TakeConsumed: one of the view's own answered (a test's own, or a pool
	// view's ahead: Take.Ahead).
	TakeConsumed TakeHow = iota + 1
	// TakeSpent: a delete door or a by-kind spend took a staged recording
	// for the staging (a call outside every test, the worker scope, S12's
	// pool in a test), or, ForView, a protocol-phase one until the view
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
	// Earlier, Served and Before are learn.Tracker.Test's.
	Earlier        []learn.Earlier
	Served, Before []*models.Mock
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
	// Open is Phase == RoundInTest: the one answer a learn step reads to
	// whether a test is open, and whose recordings are its own.
	Open bool
	// Background: the set is served by S12's background rules.
	Background bool
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
}

// RoundParts is what a store fills a RoundSnapshot's unexported answers
// from (NewRoundSnapshot): read under the same lock as the rest.
type RoundParts struct {
	Owners    map[string]RoundOwner
	Claimants map[string]string
	// Worker, Pool and BackgroundPool are the view's worker scope, its pool
	// slice and S12's background pool, by name; never written once read.
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
}

// WithParts is s answering from p (a store's, read with s).
func (s RoundSnapshot) WithParts(p RoundParts) RoundSnapshot {
	s.owners, s.claimants = p.Owners, p.Claimants
	s.worker, s.pool, s.bgPool = p.Worker, p.Pool, p.BackgroundPool
	s.windows, s.poolNames = p.Windows, p.PoolNames
	s.winStart, s.winEnd = p.WindowStart, p.WindowEnd
	s.viewGen = p.ViewGen
	return s
}

// Owner is whose m is, as the view held it at the snapshot (by name).
func (s RoundSnapshot) Owner(m *models.Mock) RoundOwner {
	if m == nil {
		return OwnerNone
	}
	return s.owners[m.Name]
}

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
// InBackgroundPool whether it is S12's background pool's.
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

// ExactBackground reports whether, at the snapshot, S12's background
// answers a call exactly or not at all, and no drift is learned onto it: the
// set is served by S12's background rules and the store shows a test's view,
// whether the learn tracker has that test open (RoundInTest) or not
// (RoundBoundary). The pool is learned onto between tests only.
func (s RoundSnapshot) ExactBackground() bool {
	return s.Background && s.Test != ""
}

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
