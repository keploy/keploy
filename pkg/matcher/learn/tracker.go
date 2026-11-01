package learn

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Tracker keeps, for the test that is running, the mocks F has served in it
// (the earlier places Classify looks in) and the names of the variables
// learned in it. The agent owns one; it begins a new test whenever the replay
// opens the next test's window, and the parsers find it on their
// connection's context.
type Tracker struct {
	mu    sync.Mutex
	epoch uint64
	// setEpoch is the test number the running set began at (NewSet): its
	// start-up's. A request of a lower number is of an earlier set.
	setEpoch uint64
	served   []Earlier
	mocks    []*models.Mock
	// before are the mocks served before the running test began: in the
	// previous test, between tests, or while the next test's mocks were
	// being put in place. The test's boundary and the mock store's do not
	// move at one moment, so a call near it may be either test's, and its
	// earlier places may be there.
	before []*models.Mock
	// set are the mocks F served in the running set, each once, in the
	// order F first served it: at its start-up, in its tests and between
	// them. F gave the application every one of them before any later call,
	// as much as one served in the call's test: an application that
	// consumes a message in one test (or at the start-up) may build a value
	// on it in a later one. Append only, so a read hands out a prefix of it
	// without a copy (Placed.Set). serves are the numbers of every serve of
	// each (mockID), in one sequence across the set (1 the first; Order),
	// and seq the last number given: the order in which F gave the
	// application its recordings, wherever in the set it gave them. NewSet
	// forgets them: it replaces serves, never changes it, so an Order read
	// before keeps the numbers it read. set holds every mock F served in the
	// set, by pointer, until the set ends: the store keeps each of them for
	// the set as well, so it holds no text the store does not.
	set    []*models.Mock
	serves map[string][]int
	seq    int
	// testFrom is the number the running test's first serve takes (seq + 1
	// at its Begin), 0 before the set's first test, and outsideSeq the
	// numbers of the serves made while no test was open (at the start-up,
	// between tests): a mock every serve of which numbers below testFrom and
	// none of them outside a test F gave the application only in an older
	// test of the set (Order.ServedView, Raw.ServedInAnOlderTest). NewSet
	// forgets them with serves. A serve adds its number to outsideSeq under
	// the lock, past every number an Order read before holds (upTo), and an
	// Order reads it under the lock, so it reads what its read saw.
	testFrom   int
	outsideSeq map[int]bool
	// asRecorded are, of each mock F served in the set (by mockID), the
	// statements its adapters made that a serve of it gave the application
	// its recording byte for byte (ServedMockAsRecorded, Served of an
	// exchange served as recorded), each the last serve number given (seq)
	// when it was made: the view a later call reads it with
	// (Order.ServedView). Made with serves, and replaced with it, so an
	// Order read before keeps the statements it can count.
	asRecorded map[string][]int
	// lives are, of each mock F served in the set (by mockID), the
	// statements its adapters made that a serve of it gave the application
	// another view than its recording, which the adapter knows whole
	// (ServedMockLive, Served of an exchange with a View): each different
	// view, and each statement's serve number and view (liveViews). Made
	// with serves and replaced with them, as asRecorded.
	lives map[string]*liveViews
	// flights are the request each serve of the set was made in, by its
	// number (nil: none, or one keploy cannot tell; flightOfLocked). NewSet
	// replaces it, as serves. aloft are the requests in flight now,
	// landings the count of those that landed or ended untold
	// (Flight.EndUntold), and flying whether any
	// connection was ever lifted (Lift): F's order is then read by
	// flights (Order.told). lifts are the requests lifted in the set, in
	// the order they lifted (Flight.lift), for a serve one of them caused
	// (Flight.LiftCaused); NewSet replaces it.
	flights map[int]*Flight
	// handshakes are the set's serves told as handshakes
	// (ServedHandshakeIn), by number.
	handshakes map[int]bool
	aloft      map[*Flight]struct{}
	landings   uint64
	flying     bool
	lifts      []*Flight
	// places are the instants of each mock F served in the running set, as
	// the offset rule reads them (GivenPlaces), read from its recording once
	// for the set (Order.GivenPlaces): no text of it is kept. NewSet
	// replaces it.
	places *setPlaces
	// derived is what the learn step read of each recording's payload alone
	// in the running set (setDerived), shared by each test's drift cache
	// (newDrifts): none of its text is kept. NewSet replaces it.
	derived *setDerived
	// outside are the mocks served while no test was open (the start-up,
	// between tests), by every later test: each is an earlier place of every
	// test that follows, not of the next one alone. The exact view shows a
	// data-plane mock of the pool only while no test is open, so a value the
	// app sent in a start-up call is found here in the second test on, and
	// refused there as in the first (only a test's own calls carry its
	// variables), never learned as noise. outsideKept are their identities
	// (mockID). They are the running set's: NewSet forgets them.
	outside     []*models.Mock
	outsideKept map[string]bool
	// kept are the identities (MockID) in mocks: a mock served again is kept
	// once.
	kept map[string]bool
	// answered are the test's own mocks that answered a request of the
	// running test, by name: each answers one (design §5.2), whatever
	// lifetime the store gives it. They are kept, as they were recorded, to
	// compete still once the store let them go (a per-test mock is deleted
	// when consumed).
	answered map[string]*models.Mock
	// drifts keeps what each value drifted, for the running test's
	// requests.
	drifts *DriftCache
	// open is whether a test is running (Begin, until End), and began
	// whether one has begun in this set (Begin, until NewSet).
	open  bool
	began bool
	// names are the running test's variables, by where each is bound (its
	// mock and place), and taken the names given.
	names map[string]string
	taken map[string]bool
	// run are the exchanges the learn step served before the first test
	// began (the start-up's) that carried a value the run made (one that
	// drifted from its recording), and, under the proven switch, the calls
	// of each test of the set that did, kept as its next begins (ranCalls):
	// an earlier place of every later test of the set (Run). runNames are
	// the variables bound at them, by mock and place, and
	// runTaken their names, which no test's variable takes. setTaken are the
	// names every test of the set has taken so far, which no run's variable
	// takes: a run's variable is named when a test first uses it, after
	// earlier tests named their own. run is kept within its share of the
	// set's budget (keepRun): from the first place past it, the run keeps
	// no more, and its last place says so (Earlier.Unread).
	run      []Earlier
	runNames map[string]string
	runTaken map[string]bool
	setTaken map[string]bool
	// appClock reads the app's clock at a real instant (SetAppClock), nil
	// while none is set.
	appClock func(time.Time) (time.Time, bool)
	// appClockRun is the real instant from which the app's clock has run as
	// one to a real instant (SetAppClockRun), nil where the hooks tell none.
	appClockRun func(time.Time) (time.Time, bool)
	// servedAt are the store's serves of the running set's data-plane mocks,
	// in the order of the real instants it served them at (servedIn), each
	// with the instant its call reached keploy. NewSet forgets them.
	servedAt []servedInstant
	// serveAt is the real instant each of the running set's serves was made
	// at, by number (serveAt[n-1] of the n-th, ServedMockAt's at): what an
	// Order cut at a call's arrival (Order.ArrivedAt) reads. NewSet forgets
	// it.
	serveAt []time.Time
	// given is the running test case's own request (Give), an earlier place
	// the learn step reads as any other (Earlier.Given); nil until it is
	// given, and for a test no keploy test case opened (keploy mock's).
	// Begin forgets it.
	given *Earlier
	// gave are the values the running test gave keploy so far (GiveValue),
	// in the order it gave them: earlier places of its calls after its
	// request (Earlier.Gave). The agent bounds how many a test gives
	// (service/agent.MaxGivensPerTest) and how long each is; Begin and NewSet
	// forget them, so a test's givens live only while it runs.
	gave []Earlier
	// awaiting is open while the running test awaits its test case's request
	// (BeginAwaiting, Await): a call that reads the test's places then
	// (Places) waits (AwaitRequest) until Give brings the request, or the
	// test's window closes without it (End, the next Begin, NewSet), which
	// close it. nil while no test awaits one.
	awaiting chan struct{}
	// onAwait, when set (OnAwait), is called as a call starts to wait for
	// the running test's request, the tracker's lock not held.
	onAwait func()
	// onUnread, when set (OnUnreadClose), is called as a test's window
	// closes while it still awaited its request, with how many calls waited
	// for it then (waiting), the tracker's lock not held.
	onUnread func(waiting int)
	// waiting counts the calls waiting for the running test's request
	// (AwaitRequest).
	waiting int
	// clientTests: the agent serves keploy mock (SetClientTests), whose
	// tests a test harness runs and gives the application its inputs; every
	// DriftCache the tracker makes says so (DriftCache.ClientTests).
	clientTests bool
	// beganAt is the real instant the running test began at (BeginAt),
	// zero before the set's first test: in a client test whose begin moved
	// the app's clock, a call's window on F's clock starts there
	// (CallWindowFrom).
	beganAt time.Time
	// setBegan is the real instant the running set began at (NewSetAt),
	// zero before the first: a server test's call the store served nothing
	// before in the set has its span on the app's clock start there, or
	// where the clock's run began after it, where the hooks tell the run
	// (LiveSpan).
	setBegan time.Time
	// recBegan is the instant the running test began at in its recording
	// (RecordedBegin, taken by BeginAt from nextRecBegan), zero where none
	// was given: in a client test a call's window in the recording starts
	// there where no earlier recorded call bounds it (CallWindowFrom).
	recBegan, nextRecBegan time.Time
	// appZone is the zone the application writes a wall clock of no zone
	// in, beside UTC (SetAppZone); nil: UTC alone. Every DriftCache the
	// tracker makes reads it (DriftCache.zone).
	appZone *time.Location
	// learnEnded says the running test set's learn step has ended
	// (SetLearnEnded): TrackerFrom then gives no tracker.
	learnEnded atomic.Bool
	// learnHeld says the learn step is held (SetLearnHeld): every
	// DriftCache the tracker makes reads it (DriftCache.held), and while it
	// is set the classification refuses every changed value.
	learnHeld atomic.Bool
}

// NewTracker makes a tracker with no test open.
func NewTracker() *Tracker {
	t := &Tracker{places: newSetPlaces(), derived: newSetDerived()}
	t.drifts = t.newDrifts()
	return t
}

// NewSet starts the next test set's replay on the same agent: what was served
// before it (the previous set's tests, its start-up, between its tests) is no
// earlier place of the new set's, and no test is open until the next Begin.
// Mock names restart in every set (mock-0 again), so a mock served in one set
// must not stand for, or hide, another set's mock of its name: the set's
// start-up takes a test number no request of the previous set holds, so a
// request of that set answered after the set changed is nobody's, and names
// none of the new set's variables, its run's neither (Name, RunName).
func (t *Tracker) NewSet() { t.NewSetAt(time.Now()) }

// NewSetAt is NewSet of a set that began at the real instant at: a server
// test's call the store served nothing before in the set starts its span on
// the app's clock no earlier than at (LiveSpan).
func (t *Tracker) NewSetAt(at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.epoch++
	t.setEpoch = t.epoch
	t.served = nil
	t.mocks, t.kept, t.before = nil, nil, nil
	t.set, t.serves, t.seq, t.asRecorded, t.lives = nil, nil, 0, nil, nil
	t.testFrom, t.outsideSeq = 0, nil
	t.flights, t.lifts, t.handshakes = nil, nil, nil
	// The earlier set's derived data is let go now, not when its last
	// handler is done with it, so the agent holds one set's budget at a time.
	t.derived.drop()
	t.places, t.derived = newSetPlaces(), newSetDerived()
	t.outside, t.outsideKept = nil, nil
	t.run, t.runNames, t.runTaken, t.setTaken = nil, nil, nil, nil
	t.names, t.taken = nil, nil
	t.answered, t.drifts = nil, t.newDrifts()
	t.servedAt, t.serveAt = nil, nil
	t.given, t.gave = nil, nil
	say := t.closeUnread()
	t.open, t.began = false, false
	t.beganAt, t.recBegan, t.nextRecBegan = time.Time{}, time.Time{}, time.Time{}
	t.setBegan = at
	t.mu.Unlock()
	say()
}

// closeAwaiting ends the running test's wait for its request (awaiting):
// every call waiting reads the test's places as they now stand. It reports
// whether the test still awaited it. t.mu is held.
func (t *Tracker) closeAwaiting() bool {
	if t.awaiting == nil {
		return false
	}
	close(t.awaiting)
	t.awaiting = nil
	return true
}

// closeUnread ends the running test's wait for its request as its window
// closes without it (End, the next Begin, NewSet), not because Give brought
// it (closeAwaiting). It returns what to call once t.mu is released: the
// OnUnreadClose hook, with how many calls waited, where the test still
// awaited its request; otherwise nothing. t.mu is held.
func (t *Tracker) closeUnread() func() {
	if !t.closeAwaiting() || t.onUnread == nil {
		return func() {}
	}
	f, n := t.onUnread, t.waiting
	return func() { f(n) }
}

// waitGiven waits while the test running as it is called awaits its test
// case's request (awaiting), t.mu held on entry and on return, released
// while it waits: until Give brings it, or that test's window closes without
// it, or done is closed. false where done closed while that test still
// awaited it.
//
// It waits for that one test's request alone: a call that wakes once the
// window closed returns, though the next test began awaiting its own
// request (BeginAwaiting) before the call ran again. Its caller reads the
// round as it then stands, which moved under the call: a call made in one
// test is never left to wait for, and be decided by, the next test's
// request.
//
// What it answers is read from the tracker as the call wakes, never from
// which of the two the scheduler showed it first: where the request came
// (or the window closed) by then, true, as for a call that read the places
// after it; where the test still awaits it and done is closed, false.
func (t *Tracker) waitGiven(done <-chan struct{}) bool {
	// awaiting is never replaced while open: a test's wait ends only by
	// closing it (closeAwaiting), and a new one is made only once it is
	// nil (await). Another channel than ch is the next test's wait.
	ch := t.awaiting
	for ch != nil && t.awaiting == ch {
		if closed(done) {
			return false
		}
		hook := t.onAwait
		t.waiting++
		t.mu.Unlock()
		if hook != nil {
			hook()
		}
		select {
		case <-ch:
		case <-done:
		}
		t.mu.Lock()
		t.waiting--
	}
	return true
}

// closed reports whether done is closed.
func closed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// OnUnreadClose makes the tracker call f as a test's window closes (End, the
// next Begin, NewSet) while the test still awaited its test case's request,
// which the filter that opens it never brought, with how many calls waited
// for it then, without the tracker's lock: the agent says so in its log at
// the default level, for every value that changed in a call of the test is
// then refused. nil calls nothing.
func (t *Tracker) OnUnreadClose(f func(waiting int)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.onUnread = f
	t.mu.Unlock()
}

// OnAwait makes the tracker call f as a call starts to wait for the running
// test's request (AwaitRequest), without the tracker's lock: the agent says
// so in its log, and a test orders the request after a call that waits for
// it through no clock. nil calls nothing.
func (t *Tracker) OnAwait(f func()) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.onAwait = f
	t.mu.Unlock()
}

// AwaitRequest waits while the running test awaits its test case's request
// (BeginAwaiting): until Give brings it, or the test's window closes without
// it (End, the next Begin, NewSet), or ctx ends, whose error it returns. It
// waits for the request of the test running as it is called alone, never
// for the next test's, though that test began awaiting its own before the
// call woke (waitGiven): the call's caller then reads a round that moved
// under it, and decides nothing of it there. A
// call that read the test's places while it awaited (Places) reads them
// again only then: the replay sends the request only after the filter that
// brings it is answered, so a call made in between is the test's background
// (a pool call, a call of the previous test's flow), and it is learned from
// as one made after the request came, whichever way the scheduler ordered
// the two. A tracker no test awaits a request of returns at once. It holds
// no lock of the tracker's while it waits; its caller must hold none that
// Give, End or a Begin takes.
//
// A known nondeterminism, accepted as fail-closed: a call woken by Give but
// not scheduled until its test's window closed (End, the next Begin) reads a
// round that moved, and its caller decides nothing of it (a miss naming the
// move), where a call scheduled at once is decided in its test. Whether such
// a call is kept or dropped then depends on the scheduler under load. Either
// way nothing is learned from another round, so it costs yield, never a
// false pass.
func (t *Tracker) AwaitRequest(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	ok := t.waitGiven(ctx.Done())
	t.mu.Unlock()
	if !ok {
		return ctx.Err()
	}
	return nil
}

// newDrifts is an empty DriftCache of the tracker's tests: one of keploy
// mock's client tests where the tracker's are (SetClientTests), in the
// application's zone (SetAppZone), with an app clock where the tracker has
// one (SetAppClock). t.mu is held.
func (t *Tracker) newDrifts() *DriftCache {
	dc := NewDriftCache()
	dc.clientTests, dc.zone, dc.appClock, dc.set = t.clientTests, t.appZone, t.appClock != nil, t.derived
	dc.held = &t.learnHeld
	if t.drifts != nil { // none yet as NewTracker makes the first
		dc.zoneName, dc.zoneNoName = t.drifts.zoneName, t.drifts.zoneNoName
	}
	return dc
}

// SetClientTests says whether the agent serves keploy mock: its tests are a
// test harness's (a Playwright or CLI test, any harness), which gives the
// application its inputs and never hands keploy a request of its own, so
// the learn step has no request place (Earlier.Given) for them. A value the
// harness gave the application can still change between the recording and
// record's full round, and move by about F's lead (a slot a minute on), so
// there a changed instant is learned as noise only where the call's window
// on the application's clock shows it a read of that clock in both runs
// (DriftCache.ClientTests). The agent sets it once, before any test.
func (t *Tracker) SetClientTests(on bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.clientTests = on
	t.drifts.clientTests = on
	t.mu.Unlock()
}

// ClientTests reports whether the tracker's tests are keploy mock's client
// tests (SetClientTests): an adapter then reads the window (CallWindowFrom)
// of every call, a test's own too, since only the window shows a read of
// the application's clock there.
func (t *Tracker) ClientTests() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.clientTests
}

// MockID is a served mock's identity, as the tracker files it (each once): its
// name, which is unique within one set's recording only, with its kind and
// recorded times. The mock store files its open test's account by it too
// (proxy MockManager.ServedInOpenTest), so the two accounts agree on which
// serves are one.
func MockID(m *models.Mock) string {
	return m.Name + "\x00" + string(m.Kind) + "\x00" + strconv.FormatInt(m.Spec.ReqTimestampMock.UnixNano(), 10) +
		"\x00" + strconv.FormatInt(m.Spec.ResTimestampMock.UnixNano(), 10)
}

// SetAppZone says in which zone, beside UTC, the application writes a wall
// clock of no zone (a "2026-10-22 16:44:16" its text names no zone of): its
// own local zone, which the agent, running beside it, reads as its own
// (nil: UTC alone). In a client test only a wall clock read in one of those
// two zones, in both runs, can be a read of the application's clock within
// its call's window (Window.holdsRead): a value any other zone would read so
// is no read keploy can tell from a value the test harness gave it (the
// clock's reading plus two hours). An application whose zone is neither has
// such a value refused, a false red that fails closed. A wall clock of no
// zone its protocol types (WallClockLeaf) is read in those two zones too
// where the learn step looks for a read of the clock (instantClock), as a
// text's is. The agent sets it once, before any test. A learning that
// depends on it names it (models.ClockOffset.AppZone, models.ZoneNameOf).
// It is the agent's own zone: a call whose process's zone the agent read
// (WithProcZone) is learned and checked in that one instead (DriftsOf,
// ClockSpanFrom).
func (t *Tracker) SetAppZone(loc *time.Location) { t.setAppZone(loc, true) }

// SetAppZoneUnnamed is SetAppZone of a zone keploy could not read by name
// (Go's own reading of a TZ it cannot read, which Go reads as UTC): no
// learning names it, so a replay reads the zone its own tracker knows, as
// before the name was stored, never a name the application's zone is not.
func (t *Tracker) SetAppZoneUnnamed(loc *time.Location) { t.setAppZone(loc, false) }

func (t *Tracker) setAppZone(loc *time.Location, named bool) {
	if t == nil {
		return
	}
	name := ""
	if named {
		name = models.ZoneNameOf(loc, time.Now())
	}
	t.mu.Lock()
	t.appZone = loc
	t.drifts.zone, t.drifts.zoneName, t.drifts.zoneNoName = loc, name, !named
	t.mu.Unlock()
}

// RecordedBegin gives the instant the next test (the one the next Begin
// starts) began at in its recording: keploy mock's tests, whose begin moves
// the app's clock there (the agent's test begins). In a client test a call's
// window in the recording then starts no earlier than it (CallWindowFrom),
// as F's starts no earlier than the test's begin in this run: the app reads
// its clock for a call of the test after the test began. A test begun with
// none given has none.
func (t *Tracker) RecordedBegin(at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.nextRecBegan = at
	t.mu.Unlock()
}

// Begin starts the next test now (BeginAt).
func (t *Tracker) Begin() { t.BeginAt(time.Now()) }

// BeginAt starts the next test at the real instant at: nothing is earlier
// than its first call. The agent begins it once the test's begin has moved
// the app's clock (keploy mock's tests, at their recorded begin, given first
// by RecordedBegin): in a client test, a call of the test reads its window
// on F's clock from then on where none runs from the previous call, and in
// the recording from the recorded begin where no earlier recorded call
// bounds it (CallWindowFrom).
func (t *Tracker) BeginAt(at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	say := t.beginAt(at)
	t.mu.Unlock()
	say()
}

// BeginAwaiting starts the next test now as one keploy test opened
// (BeginAwaitingAt).
func (t *Tracker) BeginAwaiting() { t.BeginAwaitingAt(time.Now()) }

// BeginAwaitingAt is BeginAt and Await in one step: the next test, which
// keploy test opened, begins at the real instant at awaiting its test case's
// request, which the filter that opens it brings (Give). No call reads the
// test's places between the two without being told the test awaits its
// request (Places), and waiting for it (AwaitRequest). The agent begins so
// as keploy test opens a test's capture window.
func (t *Tracker) BeginAwaitingAt(at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	say := t.beginAt(at)
	t.await()
	t.mu.Unlock()
	say()
}

// beginAt is BeginAt, t.mu held. It returns what to call once t.mu is
// released (closeUnread).
func (t *Tracker) beginAt(at time.Time) func() {
	say := t.closeUnread()
	t.epoch++
	if provenswitch.On() {
		t.keepRun(ranCalls(t.served, t.orderLocked()))
	}
	t.served = nil
	t.before, t.mocks, t.kept = t.mocks, nil, nil
	t.answered = nil
	t.drifts = t.newDrifts()
	t.names, t.taken = nil, nil
	t.given, t.gave = nil, nil
	t.open, t.began = true, true
	t.beganAt = at
	t.recBegan, t.nextRecBegan = t.nextRecBegan, time.Time{}
	t.testFrom = t.seq + 1
	return say
}

// Await marks the running test, begun already, as a keploy test case's,
// whose own request the filter that opens it carries (Give), where none was
// given yet: until it is given, a call that reads the test's places is told
// so (Places), and waits for it (AwaitRequest). A test that ends without it
// (End) leaves its earliest place a request the learn step could not read
// (Earlier.Unread): every value that changed in a call that read the test's
// places after then is refused. At the next Begin a call still waiting
// stops waiting (AwaitRequest), though the next test awaits a request of its
// own: it was made in the test whose window closed, and reads a round that
// moved under it. BeginAwaitingAt
// marks a test as it begins, in one step; a test keploy test did not open
// (keploy mock's) is never marked: it has no keploy test case.
func (t *Tracker) Await() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.open && t.given == nil {
		t.await()
	}
	t.mu.Unlock()
}

// await marks the running test as awaiting its request (Await). t.mu is
// held.
func (t *Tracker) await() {
	t.given = &Earlier{Mock: givenPlace(""), Given: true, Unread: unopenedRequest}
	if t.awaiting == nil {
		t.awaiting = make(chan struct{})
	}
}

// Give makes e (GivenRequest: the running keploy test case's own request)
// the running test's earliest place, read as any earlier place
// (Earlier.Given), until the next test begins. The agent gives it as the
// test case opens (its mock filter), after Begin and before the replay sends
// the request: a call the test's request led to finds it. One given while no
// test is open (the test's begin never reached the agent) is tied to no
// test's calls: the calls made until the next test begins read a request
// that could not be read, never the previous test's.
func (t *Tracker) Give(e Earlier) {
	if t == nil {
		return
	}
	e.Given = true
	e.Seen = append([]Seen(nil), e.Seen...)
	t.mu.Lock()
	if t.open {
		t.given = &e
	} else {
		t.given = &Earlier{Mock: e.Mock, Given: true, Unread: strayRequest}
	}
	t.closeAwaiting()
	t.mu.Unlock()
}

// GiveValue makes e (TestGiven: a value the running test gave keploy) an
// earlier place of the test's calls from now until the next test begins,
// after its test case's request (Earlier.Gave): a call that carries a value
// made from it (a part of its text, it in another reading) finds it there,
// and the learn step refuses it naming the given, never learns it as noise.
// No moment it names is a place (withoutGave): a read of the application's
// own clock learns as it does with no given. A value carried whole is the
// given's variable, which the test's variable table binds
// (variables.Binding.Given). One given while no test is open is tied to no
// test's calls, and dropped.
func (t *Tracker) GiveValue(e Earlier) {
	if t == nil || len(e.Seen) == 0 {
		return
	}
	e.Given, e.Gave = true, true
	e.Seen = append([]Seen(nil), e.Seen...)
	t.mu.Lock()
	if t.open {
		t.gave = append(t.gave, e)
	}
	t.mu.Unlock()
}

// End ends the running test: until the next Begin no test is open, and a
// request belongs to no test's own mocks. A test that still awaited its
// request closes without it: the calls waiting for it read the test's places
// with a request that could not be read (Await).
func (t *Tracker) End() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.open = false
	say := t.closeUnread()
	t.mu.Unlock()
	say()
}

// Open reports whether a test is open. A nil tracker, which knows nothing of
// tests, answers true.
func (t *Tracker) Open() bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.open
}

// Begun reports whether a test has begun in this set (Begin, until NewSet):
// what is served before is the start-up's. A nil tracker answers true.
func (t *Tracker) Begun() bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.began
}

// Test is the running test's number, its earlier places: its test case's
// own request first (Give), the values it gave keploy (GiveValue), then
// what the learn step served in it so far, leaf by leaf; every mock any
// parser served in it so far (ServedMock), and the mocks served before it began: those served since the
// previous test began (see Tracker.before), and every one served while no
// test was open (see Tracker.outside), each once. A request reads it once
// its scope is read, and hands the number back to Served. It never waits:
// while the running test awaits its test case's request its earliest place
// is a request the learn step could not read, and a caller that learns from
// a call reads Places instead, which says so.
func (t *Tracker) Test() (uint64, []Earlier, []*models.Mock, []*models.Mock) {
	n, earlier, served, before, _ := t.Places()
	return n, earlier, served, before
}

// Places is Test, and, in the same read, whether the running test still
// awaits its test case's request (BeginAwaiting, Await): a call that reads
// its places then learns nothing from them, waits for the request
// (AwaitRequest), and reads its scope and places again, so that the learn
// step's verdict on it does not hang on whether it fell before the filter
// that opens the test or after. It never waits itself: a caller may read it
// holding a lock of its own.
func (t *Tracker) Places() (uint64, []Earlier, []*models.Mock, []*models.Mock, bool) {
	p := t.Placed()
	return p.Test, p.Earlier, p.Served, p.Before, p.Awaiting
}

// Placed is one read of the tracker's places (Places), with F's serving of
// the running set as that read saw it: every mock F served in the set so far
// (Set) and the order it served them in (Order).
type Placed struct {
	Test           uint64
	Earlier        []Earlier
	Served, Before []*models.Mock
	Awaiting       bool
	// Open and Begun are State's, in the same read: whether a test is open
	// (Begin, until End), and whether one has begun in this set. A call's
	// learn step holds each later pass to the test and the openness its
	// first pass read (the round it was made in).
	Open, Begun bool
	// SetAt is SetAt's, in the same read: the test number the running set
	// began at.
	SetAt uint64
	// Set is every mock F served in the running set before the read, each
	// once, in the order F first served it (Tracker.set): a prefix of the
	// tracker's own list, which only grows, so no later serve shows in it.
	Set []*models.Mock
	// Order is where in the set F served each of them, as of the read.
	Order Order
}

// Placed is Places with the set's serving (Placed.Set, Placed.Order), read
// at one instant. It copies the running test's mocks, those served before it
// began and those served while no test was open, never the whole set.
func (t *Tracker) Placed() Placed {
	if t == nil {
		return Placed{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	before := append(make([]*models.Mock, 0, len(t.before)+len(t.outside)), t.before...)
	seen := make(map[string]bool, len(t.before))
	for _, m := range t.before {
		seen[MockID(m)] = true
	}
	for _, m := range t.outside {
		if !seen[MockID(m)] {
			before = append(before, m)
		}
	}
	earlier := make([]Earlier, 0, len(t.gave)+len(t.run)+len(t.served)+1)
	if t.given != nil {
		earlier = append(earlier, *t.given)
	}
	earlier = append(append(append(earlier, t.gave...), t.run...), t.served...)
	return Placed{
		Test: t.epoch, Earlier: earlier, Served: append([]*models.Mock(nil), t.mocks...), Before: before, Awaiting: t.awaiting != nil,
		Open: t.open, Begun: t.began, SetAt: t.setEpoch,
		Set:   t.set[:len(t.set):len(t.set)],
		Order: t.orderLocked(),
	}
}

// orderLocked is the order of the set's serves as a read now sees it
// (Placed.Order). t.mu is held.
func (t *Tracker) orderLocked() Order {
	return Order{t: t, serves: t.serves, asRecorded: t.asRecorded, lives: t.lives, upTo: t.seq, testFrom: t.testFrom, outside: t.outsideSeq, places: t.places, flights: t.flights, handshakes: t.handshakes, set: t.setEpoch, landings: t.landings, flying: t.flying, serveAt: t.serveAt[:len(t.serveAt):len(t.serveAt)]}
}

// ServedMock notes a mock any parser served in the running test: an earlier
// place of the test's recording, whatever its kind, for Classify (Raw); and,
// a data-plane one, a call of the app's that reached keploy now and was
// answered now (CallWindow).
func (t *Tracker) ServedMock(m *models.Mock) { t.ServedMockAt(m, time.Now()) }

// ServedMockAt is ServedMock of a mock served, to a call that reached keploy,
// at the real instant at.
func (t *Tracker) ServedMockAt(m *models.Mock, at time.Time) { t.servedIn(m, at, at, nil, serveKind{}) }

// ServedMockIn is ServedMock of a mock served in the request in, as the
// parser that served it said (integrations.ServingIn): the serve is that
// request's whatever other requests are aloft (Flight); in NoRequest, no
// request's; in one a request caused (Flight.LiftCaused), that one's. A nil
// in is ServedMock. The serve is made now, and its call reached keploy when
// in lifted (Flight.Lift; now where in is no request of the application's):
// a store's consume door tells it after the parser's learn step decided the
// call, so the call is a later one's previous call from its arrival, as
// record's is from its request (PreviousCall), not from the consume.
func (t *Tracker) ServedMockIn(m *models.Mock, in *Flight) {
	now := time.Now()
	t.servedIn(m, now, in.arrivedBy(t, now), in, serveKind{})
}

// ServedMockAtIn is ServedMockIn of a mock served, to a call that reached
// keploy, at the real instant at.
func (t *Tracker) ServedMockAtIn(m *models.Mock, at time.Time, in *Flight) {
	t.servedIn(m, at, at, in, serveKind{})
}

// ServedHandshakeIn is ServedMock of a handshake m, a protocol phase, that
// answers the request in, its connection's recorded handshake having been
// chosen in the request chosen, as its parser says: the answer a server
// gives the request a client opens its connection with (a Postgres start-up
// answers the StartupMessage, and is picked as the connection's first
// request arrives: its StartupMessage, or the SSLRequest before it; an
// SSLRequest's answer, the SSLRequest). A protocol phase told with
// ServedMockIn (a driver's heartbeat) has no place in F's order and makes no
// request a serve; a handshake told here has in's place (Flight) where it
// was chosen in in: told once in landed, unless in was aloft beside a
// request that was a serve or got a handshake. Chosen in another request, it
// has no place, as two connections whose choices were aloft together may
// have got each other's handshakes whatever came after. Either way it makes
// in one that got a handshake, and no other request's serve untold. The
// parser tells it while in is aloft, before its answer's first write lands
// in: told after, it has no place (fail closed), as the application may have
// read it and sent a request whose serve F numbered before it. A nil in, one
// of another tracker's, or NoRequest gives it no place once a connection
// lifted (Order.Of: 0, 0). A data-plane m is numbered and placed so too, and
// makes no request a serve: tell those with ServedMockIn.
func (t *Tracker) ServedHandshakeIn(m *models.Mock, chosen, in *Flight) {
	now := time.Now()
	t.servedIn(m, now, in.arrivedBy(t, now), in, serveKind{handshake: true, placed: chosen == in})
}

// servedInstant is a data-plane serve of the store's (servedIn): made at the
// real instant at, to a call that reached keploy at from (no later than at).
// upTo is the latest from of it and of every serve made before it.
type servedInstant struct {
	at, from, upTo time.Time
}

// servedIn is ServedMockAt of a serve made at the real instant at, to a call
// that reached keploy at from, in the request in (nil: not said), told as k
// says (serveKind).
func (t *Tracker) servedIn(m *models.Mock, at, from time.Time, in *Flight, k serveKind) {
	if t == nil || m == nil {
		return
	}
	t.mu.Lock()
	if !m.IsProtocolPhase() {
		i := sort.Search(len(t.servedAt), func(i int) bool { return t.servedAt[i].at.After(at) })
		t.servedAt = append(t.servedAt, servedInstant{})
		copy(t.servedAt[i+1:], t.servedAt[i:])
		t.servedAt[i] = servedInstant{at: at, from: from}
		for j := i; j < len(t.servedAt); j++ {
			up := t.servedAt[j].from
			if j > 0 && t.servedAt[j-1].upTo.After(up) {
				up = t.servedAt[j-1].upTo
			}
			t.servedAt[j].upTo = up
		}
	}
	id := MockID(m)
	if t.serves == nil {
		t.serves, t.asRecorded, t.lives = map[string][]int{}, map[string][]int{}, map[string]*liveViews{}
	}
	if len(t.serves[id]) == 0 {
		t.set = append(t.set, m)
	}
	t.seq++
	t.serves[id] = append(t.serves[id], t.seq)
	t.serveAt = append(t.serveAt, at)
	if !t.open {
		if t.outsideSeq == nil {
			t.outsideSeq = map[int]bool{}
		}
		t.outsideSeq[t.seq] = true
	}
	if k.handshake {
		if t.handshakes == nil {
			t.handshakes = map[int]bool{}
		}
		t.handshakes[t.seq] = true
	}
	if f := t.flightOfLocked(m.IsProtocolPhase(), in, k); f != nil {
		if t.flights == nil {
			t.flights = map[int]*Flight{}
		}
		t.flights[t.seq] = f
	}
	// Kept between tests too: the next Begin moves it to before.
	if !t.kept[id] {
		if t.kept == nil {
			t.kept = map[string]bool{}
		}
		t.kept[id] = true
		t.mocks = append(t.mocks, m)
	}
	// Served while no test is open: an earlier place of every later test of
	// the set.
	if !t.open && !t.outsideKept[id] {
		if t.outsideKept == nil {
			t.outsideKept = map[string]bool{}
		}
		t.outsideKept[id] = true
		t.outside = append(t.outside, m)
	}
	t.mu.Unlock()
}

// Order is F's serving of the running set as one read of the tracker saw it
// (Placed): the serves numbered up to the read's last, of the set the read
// was of (serves, which NewSet replaces and never changes, so a read keeps
// its set's numbers after the next set begins), and that set's places
// (GivenPlaces). The zero Order knows no serve.
type Order struct {
	t      *Tracker
	serves map[string][]int
	// asRecorded are the set's statements that a serve gave the application
	// its recording byte for byte (Tracker.asRecorded): ServedView counts
	// them, and lives the views stated of serves (Tracker.lives).
	asRecorded map[string][]int
	lives      map[string]*liveViews
	upTo       int
	// testFrom is the number the running test's first serve took or takes,
	// as the read saw it (Tracker.testFrom): 0 where no test had begun in
	// the set; outside the numbers of the set's serves made while no test
	// was open (Tracker.outsideSeq).
	testFrom int
	outside  map[int]bool
	places   *setPlaces
	// flights, landings and flying are the set's requests of each serve,
	// the landings the read saw and whether F's order is read by flights
	// (Order.told).
	flights map[int]*Flight
	// handshakes are the set's serves told as handshakes
	// (ServedHandshakeIn), by number.
	handshakes map[int]bool
	// set is the epoch of the set the read was of (Tracker.setEpoch).
	set      uint64
	landings uint64
	flying   bool
	// serveAt is the real instant of each serve numbered up to upTo
	// (Tracker.serveAt), and arrival the instant the call the read is for
	// reached keploy (ArrivedAt; zero: not said).
	serveAt []time.Time
	arrival time.Time
}

// ArrivedAt is o for the call that reached keploy at the real instant
// arrival: ServedView marks a recording every serve of which o saw was made
// after arrival (Raw.servedAfter), which F had not served when the call
// reached keploy, however soon after it F served it and the call's learn
// step read the tracker. The rule a recording F had not served before the
// call reads it so (follows.notServed), so that its verdict does not hang
// on how long the call waited for its learn step: a sibling served in that
// wait, which grows with load, leaves it as it was. A zero arrival is o.
func (o Order) ArrivedAt(arrival time.Time) Order {
	o.arrival = arrival
	return o
}

// servedAfterLocked reports whether every serve of the mock of identity id
// that o saw was made after o's arrival (ArrivedAt), and there was one: no
// arrival said, a serve of an instant not kept, or one made at or before the
// arrival, says it was served before the call. o.t.mu is held.
func (o Order) servedAfterLocked(id string) bool {
	if o.arrival.IsZero() {
		return false
	}
	n := o.serves[id]
	k := sort.SearchInts(n, o.upTo+1)
	if k == 0 {
		return false
	}
	for _, s := range n[:k] {
		if s < 1 || s > len(o.serveAt) || !o.serveAt[s-1].After(o.arrival) {
			return false
		}
	}
	return true
}

// Of is where F served m in the set, in one sequence across its start-up,
// its tests and the gaps between them (1 the first serve): the first and the
// last of its serves the read saw. A mock F served once has one number for
// both; a mock F served again (a pool's reusable recording) spans every
// other serve in between. 0, 0 where the read saw no serve of m, or where
// one of its serves is in an order keploy cannot tell (Order.told: made
// while another request of the application's was in flight on a parallel
// connection, or whose request was still in flight at the read; Flight),
// which the offset rule fails closed on (learn.Raw.ServedIn). The same for
// every call of the same read, whatever F served or the application sent
// since, in this set or after the next began.
func (o Order) Of(m *models.Mock) (first, last int) {
	if o.t == nil || m == nil {
		return 0, 0
	}
	o.t.mu.Lock()
	defer o.t.mu.Unlock()
	return o.ofLocked(MockID(m))
}

// Spans is Of of each of ms, in one hold of the tracker's lock.
func (o Order) Spans(ms []*models.Mock) [][2]int {
	out := make([][2]int, len(ms))
	if o.t == nil {
		return out
	}
	o.t.mu.Lock()
	defer o.t.mu.Unlock()
	for i, m := range ms {
		if m != nil {
			out[i][0], out[i][1] = o.ofLocked(MockID(m))
		}
	}
	return out
}

// ServedView is r, a recording of m that F served in the running set
// (Raw.ServedIn) and whose view no adapter stated yet, with the view F's
// serves of it gave the application, as their adapters told it, where every
// serve of m the read saw was told: served as recorded (Raw.ServedAsRecorded)
// where each was told so (Tracker.ServedMockAsRecorded, Tracker.Served of an
// exchange served as recorded); the view they gave (Raw.ServedLive) where
// each was told with one view of m, the same (Tracker.ServedMockLive,
// Tracker.Served of an exchange with a View); and, where they gave more than
// one (two views, or one and the recording), each view (Raw.ServedChanged: a
// word a value holds now that stands in any of them and not in the recorded
// text is refused). A view that could not be kept or read stands for any
// text (Raw.ServedChanged with none). Anything else stays r: a serve no
// adapter told of, or one keploy cannot tell from another set's. F's view of
// r is then one no adapter stated, and a word a value holds now that may
// stand there is refused (Raw.viewUnstated: fail closed). A Raw with a view
// already stated (ServedLive, ServedChanged) keeps it. Every adapter's learn
// step reads the recordings F served through it, so a statement one adapter
// made holds for every other's call.
//
// Where F made every serve of m the read saw in an older test of the set
// (olderTestLocked), r is also marked so (Raw.ServedInAnOlderTest): F gave
// the application m's answer there, never at the start-up, between tests or
// in the call's own test. One serve there, or one in an order keploy cannot
// tell, leaves it unmarked: the answer then stays where a value may have
// come from, whatever F gave there (fail closed). A Raw not served, or of
// another mock, is returned as it is.
func (o Order) ServedView(m *models.Mock, r Raw) Raw {
	if o.t == nil || m == nil || r.mock != m.Name || !r.served {
		return r
	}
	id := MockID(m)
	unstated := r.viewUnstated()
	var recorded, unread, ok bool
	var loads []func() (*models.Mock, error)
	o.t.mu.Lock()
	older, before, after := o.olderTestLocked(id), o.beforeTestLocked(id), o.servedAfterLocked(id)
	if unstated {
		recorded, _, loads, unread, ok = o.viewsLocked(id)
	}
	o.t.mu.Unlock()
	if older {
		r = r.ServedInAnOlderTest()
	}
	if before {
		r = r.ServedBeforeTheTest()
	}
	r.servedAfter = after
	switch {
	case !unstated, !ok:
		return r
	case unread:
		return r.ServedChanged()
	case len(loads) == 0:
		return r.ServedAsRecorded()
	}
	views := make([]Raw, len(loads))
	for i, load := range loads {
		views[i] = viewRaw(m, load)
	}
	if !recorded && len(views) == 1 {
		return r.ServedLive(views[0])
	}
	// A serve told as recorded gave the recording itself: one of the
	// views, which holds no word the recorded text does not.
	if recorded {
		views = append(views, r)
	}
	return r.ServedChanged(views...)
}

// viewsLocked is what the statements say of the serves of the mock of
// identity id the read saw (those numbered up to upTo): ok where every one
// was told, as recorded or with a view, and there was one (as many
// statements numbered up to upTo as serves: a statement follows its serve,
// so one numbered up to upTo is of a serve the read saw); recorded where one
// was told as recorded; the loader of each different view told
// (ServedMockLive), in the order first told; unread where one was told with
// a view that could not be kept; and where each loader's view stands among
// the recording's views the set keeps (keys: liveViews.views), which names
// it for the set (Order.GivenPlaces). o.t.mu held.
func (o Order) viewsLocked(id string) (recorded bool, keys []int, loads []func() (*models.Mock, error), unread, ok bool) {
	n := o.serves[id]
	k := sort.SearchInts(n, o.upTo+1)
	if k == 0 {
		return false, nil, nil, false, false
	}
	told := sort.SearchInts(o.asRecorded[id], o.upTo+1)
	recorded = told > 0
	if lv := o.lives[id]; lv != nil {
		var seen map[int]bool
		for _, s := range lv.stated {
			if s.seq > o.upTo {
				break
			}
			told++
			switch {
			case s.view < 0:
				unread = true
			case !seen[s.view]:
				if seen == nil {
					seen = map[int]bool{}
				}
				seen[s.view] = true
				keys = append(keys, s.view)
				loads = append(loads, lv.views[s.view].load)
			}
		}
	}
	return recorded, keys, loads, unread, told == k
}

// viewRaw is the view load gives of m, which F served (Tracker.ServedMockLive),
// as Raw.ServedLive and Raw.ServedChanged take one: read through load where
// they read it, and kept by none of the set's caches (Raw.viewOf).
func viewRaw(m *models.Mock, load func() (*models.Mock, error)) Raw {
	return Raw{mock: m.Name, at: m.Spec.ReqTimestampMock, answered: m.Spec.ResTimestampMock, served: true, call: callOf(m),
		src: &rawSource{name: m.Name, load: load}}
}

// beforeTestLocked reports whether every serve of the mock of identity id the
// read saw was made before the running test began (numbered below testFrom:
// at the set's start-up, between tests, or in an older test), each in an
// order keploy can tell (Order.told), and there was one. false where no test
// had begun in the set (testFrom 0). The caller holds t.mu.
func (o Order) beforeTestLocked(id string) bool {
	n := o.serves[id]
	k := sort.SearchInts(n, o.upTo+1)
	if k == 0 || o.testFrom == 0 || n[k-1] >= o.testFrom {
		return false
	}
	for _, s := range n[:k] {
		if !o.told(s) {
			return false
		}
	}
	return true
}

// olderTestLocked reports whether F made every serve of the mock of
// identity id the read saw (those numbered up to upTo), and one, in an older
// test of the set than the running one: while a test was open (none
// outside), numbered below the running test's first (testFrom), each in an
// order keploy can tell (told). o.t.mu held.
func (o Order) olderTestLocked(id string) bool {
	n := o.serves[id]
	k := sort.SearchInts(n, o.upTo+1)
	if k == 0 || n[k-1] >= o.testFrom {
		return false
	}
	for _, s := range n[:k] {
		if o.outside[s] || !o.told(s) {
			return false
		}
	}
	return true
}

// ofLocked is Of of the mock of identity id, o.t.mu held (the running set's
// serves still grow under it).
func (o Order) ofLocked(id string) (first, last int) {
	n := o.serves[id]
	k := sort.SearchInts(n, o.upTo+1)
	if k == 0 {
		return 0, 0
	}
	for _, s := range n[:k] {
		if !o.told(s) {
			return 0, 0
		}
	}
	return n[0], n[k-1]
}

// Epoch is the running test's number, the one Test returns, read without
// copying anything the tracker keeps: Begin and NewSet move it, and nothing
// else does (End keeps it, as does every serve and answer). A learn step that
// reads it as a call is matched and again as it decides is of one test of one
// set exactly when the two are equal: a test's end and the next one's begin,
// or a new set, landing between the reads move it. It is the tracker's own
// statement of the test's identity, for a reader that held the drift cache's
// address for it (Drifts), which moves with it today but says nothing of it.
func (t *Tracker) Epoch() uint64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.epoch
}

// OpenEpoch is the open test's number (Epoch) while a test is open (Begin,
// until End or NewSet), and 0 while none is: before the first Begin, after a
// test's End, and after NewSet until the next Begin. The mock store stamps a
// test's view with it as it installs the view (proxy MockManager.
// SetOpenTestEpoch): a view installed while the tracker had no test open
// carries 0, which no reader takes for a test of the tracker's.
func (t *Tracker) OpenEpoch() uint64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.open {
		return 0
	}
	return t.epoch
}

// Drifts is what each value drifted, kept for the running test (Classify),
// and before the set's first test for its start-up, so that the start-up's
// calls read each earlier recording's text once.
func (t *Tracker) Drifts() *DriftCache {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.drifts
}

// DriftsOf is Drifts read in the zone of the process that made the call ctx
// serves (WithProcZone; DriftCache.InZone): the application's zone a call's
// learning depends on is its own process's, never the agent's. Drifts
// itself where ctx carries none.
func (t *Tracker) DriftsOf(ctx context.Context) *DriftCache {
	dc := t.Drifts()
	if z, ok := procZoneFrom(ctx); ok {
		return dc.InZone(z)
	}
	return dc
}

// Answered reports whether the running test's own mock name has answered a
// request of the test: it answers no other. A nil tracker, which knows of no
// test, answers false.
func (t *Tracker) Answered(name string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.answered[name] != nil
}

// Answer takes the test's own mock m to answer a request of test number n:
// false when it answered one already, or that test has ended, or no test is
// open, and the request looks again. Two requests never take one mock. A nil
// tracker takes every mock.
func (t *Tracker) Answer(n uint64, m *models.Mock) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.open || t.epoch != n || t.answered[m.Name] != nil {
		return false
	}
	if t.answered == nil {
		t.answered = map[string]*models.Mock{}
	}
	t.answered[m.Name] = m
	return true
}

// AnsweredMocks are the running test's own mocks that answered a request of
// it, in no order.
func (t *Tracker) AnsweredMocks() []*models.Mock {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*models.Mock, 0, len(t.answered))
	for _, m := range t.answered {
		out = append(out, m)
	}
	return out
}

// Unanswer gives back a mock Answer took for a request that was not answered
// from it after all.
func (t *Tracker) Unanswer(n uint64, name string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.epoch == n {
		delete(t.answered, name)
	}
	t.mu.Unlock()
}

// Served adds a mock F served to test number n's history, until the next test
// begins: a request of that test answered after it ended, or between it and
// the next, lands there too, and the next Begin forgets it, but for the
// test's own calls that carried a value the run made, which it keeps as the
// run's under the proven switch (ranCalls), one told after the next test
// began among them. Served before the
// set's first test began (the start-up's), it is the run's when it carried a
// value the run made (Earlier.Run): an earlier place of every test of the
// set. Either way it is the start-up's (Earlier.StartUp). A set has no
// test's own exchange (Own) before its first test: one served then took its
// scope from the previous set, and is nobody's.
//
// Of an exchange its adapter says F served as recorded (Earlier.AsRecorded,
// every Seen as recorded), Served is also ServedMockAsRecorded of its
// recording, told in test n of the running set: the view a later call reads
// it with, in any test of the set (Order.ServedView). Of one it says F
// gave the application another view of (Earlier.View), it is
// ServedMockLive of that view.
func (t *Tracker) Served(n uint64, e Earlier) {
	if t == nil {
		return
	}
	t.mu.Lock()
	switch {
	case n < t.setEpoch:
	case e.servedAsRecorded():
		t.statedLocked(e.Payload.id)
	case e.View != nil && !e.Given:
		t.livedLocked(e.Payload.id, e.ViewKey, e.View)
	}
	e.StartUp = !t.open && !t.began
	switch {
	case t.epoch != n:
		// An earlier test's call of the set told after the next began is
		// the run's as one told in time is (ranCalls); the start-up's and a
		// previous set's are nobody's. It keeps no number of F's serves
		// (an Order of no tracker numbers none): the order now numbers the
		// serves of the tests after its own too, and F's serve of it may
		// itself be numbered after its test ended, so no count read now is
		// the one its test's end saw. A later call's copy rule then cannot
		// tell where F served it among other served sources, and refuses
		// (servedOrder).
		if n > t.setEpoch && provenswitch.On() {
			t.keepRun(ranCalls([]Earlier{e}, Order{}))
		}
	case e.StartUp && e.Own:
	case e.StartUp && drifted(e):
		e.Run = true
		t.keepRun([]Earlier{e})
	default:
		t.served = append(t.served, e)
	}
	t.mu.Unlock()
}

// ServedMockAsRecorded says that F's serve of m just told (ServedMock and
// its forms) gave the application m's recording byte for byte: its request
// as recorded (no part of it learned as noise or bound as a variable) and
// the recorded answer (no variable, template value or clock stamp of F's
// own in it), as Raw.ServedAsRecorded says it. An adapter tells it once for
// each such serve, after the serve, and only where it knows both. A later
// call of the set reads m as served as recorded where every serve of it
// the call's read saw was told so (Order.ServedView); a serve no adapter
// told so is one whose view no adapter stated, and a word a value holds
// now that may stand there is refused (fail closed).
func (t *Tracker) ServedMockAsRecorded(m *models.Mock) {
	if t == nil || m == nil {
		return
	}
	t.mu.Lock()
	t.statedLocked(MockID(m))
	t.mu.Unlock()
}

// ServedMockLive says that F's serve of m just told (ServedMock and its
// forms) gave the application another view of m than its recording, which
// its adapter knows whole: view gives m as F's application sent its request
// and F answered it (Raw.ServedLive's live), read where a later call's rule
// reads it, never kept by the set's caches; key names that view among m's,
// the same for two serves that gave the same view and another for each
// other. An adapter tells it once for each such serve, after the serve, in
// place of ServedMockAsRecorded, and only where it knows the whole view. A
// later call of the set reads m with the view (Order.ServedView) where every
// serve of it the call's read saw was told; with each where they differ. A
// nil view, a key "", or a view past the maxLiveViews different ones a set
// keeps of m, says F gave a view keploy cannot read: a later call refuses
// every word a value holds now that may stand there (Raw.ServedChanged with
// none: fail closed).
func (t *Tracker) ServedMockLive(m *models.Mock, key string, view func() (*models.Mock, error)) {
	if t == nil || m == nil {
		return
	}
	t.mu.Lock()
	t.livedLocked(MockID(m), key, view)
	t.mu.Unlock()
}

// maxLiveViews is how many different views of one recording a set keeps
// (ServedMockLive): a serve that gave another one is read as one whose view
// could not be read, which fails closed, so a set keeps at most
// maxLiveViews loaders of each recording, and a later call reads at most
// as many views of it.
const maxLiveViews = 4

// liveViews are the views stated of one mock's serves (ServedMockLive): each
// different one, in the order first stated, and each statement: the last
// serve number given when it was made and its view (an index in views; -1:
// one that could not be kept).
type liveViews struct {
	views  []liveView
	stated []liveStated
}

// liveView is one view stated of a mock's serves: its key and its loader.
type liveView struct {
	key  string
	load func() (*models.Mock, error)
}

// liveStated is one statement of a view (liveViews): seq the last serve
// number given when it was made, view its index (-1: could not be kept).
type liveStated struct{ seq, view int }

// livedLocked notes a statement that a serve of the mock of identity id gave
// the application the view load gives, named key (ServedMockLive), at the
// last serve number given, as statedLocked notes one as recorded. Of a mock
// the set never served, or of an id "", it notes nothing. t.mu is held.
func (t *Tracker) livedLocked(id, key string, load func() (*models.Mock, error)) {
	if id == "" || len(t.serves[id]) == 0 {
		return
	}
	lv := t.lives[id]
	if lv == nil {
		lv = &liveViews{}
		t.lives[id] = lv
	}
	k := -1
	if load != nil && key != "" {
		for i, v := range lv.views {
			if v.key == key {
				k = i
				break
			}
		}
		if k < 0 && len(lv.views) < maxLiveViews {
			lv.views = append(lv.views, liveView{key: key, load: load})
			k = len(lv.views) - 1
		}
	}
	lv.stated = append(lv.stated, liveStated{seq: t.seq, view: k})
}

// statedLocked notes a statement that a serve of the mock of identity id
// gave the application its recording byte for byte (ServedMockAsRecorded),
// at the last serve number given: a statement follows its serve, so one
// numbered at or below a read's last serve is of a serve that read saw
// (Order.ServedView). Of a mock the set never served, or of an id "" (a
// payload not read from its mock), it notes nothing. t.mu is held.
func (t *Tracker) statedLocked(id string) {
	if id == "" || len(t.serves[id]) == 0 {
		return
	}
	t.asRecorded[id] = append(t.asRecorded[id], t.seq)
}

// drifted reports whether an exchange's request carried a value other than
// its recording's at some place a variable can hold.
func drifted(e Earlier) bool {
	for _, s := range e.Seen {
		if driftedLeaf(s) {
			return true
		}
	}
	return false
}

// driftedLeaf: s is a request leaf the application sent with another value
// than its recording's, a value the run made.
func driftedLeaf(s Seen) bool {
	return s.Side == Request && s.Place != "" && s.Live != "" && s.Live != s.Leaf.Value
}

// ranCalls are the run's exchanges the ended test's own calls (served)
// leave to the tests after it in the set: of each call whose request carried
// a value the run made, those request leaves alone (driftedLeaf), as an
// exchange of the run's (Earlier.Run, no longer Own). A later test's call
// that sends such a value is read with the same rule as one its own earlier
// call sent, and binds it by the run's name (RunName) where the earlier
// test's call sent it: a value one test made and a later one reuses is
// followed, never learned as noise. Of the rest of the call only where its
// recording stands in the orders the rules read is kept (Raw.orderOf: when
// the recording asked and answered, where it carried the request, F's serves
// of it), which a later call's copy rule reads to tell whether the recording
// gave the application that answer before the call, and where F served it
// among the other earlier answers (recordedGave, servedOrder): without them,
// every value a later call holds that one of those leaves held in the
// recording would be refused as one keploy cannot tell from a copy. Nothing
// else (its other leaves, its answer's text, its view) is kept, so a word of
// a leaf that did not drift is no earlier place of a later test, and the set
// holds the values its tests made, not their calls. A call of the pool's (not Own)
// carries no test's variable, and leaves none; nor does one that carried no
// value the run made, which the later test reads as another test's recording
// (Raw).
func ranCalls(served []Earlier, o Order) []Earlier {
	var out []Earlier
	for _, e := range served {
		var seen []Seen
		for _, s := range e.Seen {
			if e.Own && driftedLeaf(s) {
				seen = append(seen, s)
			}
		}
		if seen != nil {
			out = append(out, Earlier{Mock: e.Mock, At: e.At, Run: true, Seen: seen, noTime: e.noTime, Payload: o.orderOf(e.Payload)})
		}
	}
	return out
}

// orderOf is x as the place an earlier test's call left the run keeps it
// (ranCalls): where its recording stands in the orders the rules read, its
// mock, when its request and its answer were recorded, where its request was
// carried, and F's serves of it as o numbers them (Order.ofLocked: those
// told by the next test's begin, where the call was told in its own test;
// none where o is of no tracker, a call told after its test ended), with no
// word of its text, its identity or a view (Raw.ordered). A number x carries
// is not kept: only o numbers it. A payload of no mock stays the zero Raw: an
// exchange that came with none gives no order either. Its text is never
// loaded here. o's tracker's lock is held.
func (o Order) orderOf(x Raw) Raw {
	if x.mock == "" {
		return Raw{}
	}
	r := Raw{mock: x.mock, at: x.at, served: x.served, answered: x.answered, conn: x.conn, pos: x.pos, noTime: x.noTime, ordered: true}
	if o.t == nil {
		return r
	}
	// The name first: the set's names are its mocks' (one each), so the
	// scan builds no key and allocates nothing for a mock of another name.
	for _, m := range o.t.set {
		if m == nil || m.Name != x.mock || !m.Spec.ReqTimestampMock.Equal(x.at) {
			continue
		}
		if first, last := o.ofLocked(MockID(m)); first > 0 {
			r = r.ServedIn(first, last)
		}
		break
	}
	return r
}

// keepRun adds places to the run's (run), each within the run's share of
// the set's budget (setDerived.keepRun), so a set whose tests send large
// values (a generated document, an upload) holds a bounded part of them, not
// all of them. From the first place past it the run keeps no more: in its
// stead one place of no leaves says so (Earlier.Unread, runFullWhy), and
// every value that changed in a later call of the set is then refused
// rather than learned as noise or an echo (requestUnread): it may have come
// from a place the run no longer keeps, and keploy cannot tell. That place
// is an earlier test's (not StartUp), which no recorded time moves past a
// call (alwaysEarlier). A value a kept place holds is followed as before.
// t.mu is held.
func (t *Tracker) keepRun(places []Earlier) {
	for _, e := range places {
		if n := len(t.run); n > 0 && t.run[n-1].Unread != "" {
			return
		}
		if !t.derived.keepRun(runPlaceBytes(e)) {
			e = Earlier{Mock: e.Mock, At: e.At, Run: true, Unread: runFullWhy(e.Mock, t.derived.runCap())}
		}
		t.run = append(t.run, e)
	}
}

// runPlaceBytes is what the run counts of a place it keeps (keepRun): the
// exchange and its leaves, with their text. The start-up's exchange, kept
// whole, keeps its recording too (Payload): its text where it holds it
// (RawOf), none where the set's store does, which lets it go (LazyRawOf).
func runPlaceBytes(e Earlier) int64 {
	n := int64(unsafe.Sizeof(e)) + int64(len(e.Mock)+len(e.Payload.rText))
	for _, s := range e.Seen {
		n += int64(unsafe.Sizeof(s)) + int64(len(s.Leaf.Path)+len(s.Leaf.Noise)+len(s.Leaf.Value)+len(s.Live)+len(s.Place)+len(s.Record)+len(s.Name))
	}
	return n
}

// runFullWhy is why a value that changed is refused once the run keeps no
// more places (keepRun), from mock's call on, past limit bytes: it names the
// call and the bound, never a value, and how to correct it.
func runFullWhy(mock string, limit int64) string {
	return "keploy keeps the values each test of a set sends, for the tests after it, within " + strconv.FormatInt(limit>>10, 10) + " KiB, and the set's tests sent more from " + mock + "'s call on: the value may have come from one it no longer keeps, and keploy cannot tell. Split the test set, or give the keploy agent more memory"
}

// Name is the name of test number n's variable bound at place of mock: the
// same for every call that uses it, no other variable's in the test
// (variables are named per test), and none of reserved (the test case's
// template keys). ok is false when that test has ended. A place of the run's
// (RunName) is named the run's variable's name, whatever test of the set n
// is; ok is false when reserved holds that name (RunName), or n is of an
// earlier set: the set's run is not its. A nil tracker, which knows of no
// test, names a variable by its place alone.
func (t *Tracker) Name(n uint64, mock, place string, reserved map[string]bool) (name string, ok bool) {
	if t == nil {
		name = varName(place)
		for i := 2; reserved[name]; i++ {
			name = varName(place) + "-" + strconv.Itoa(i)
		}
		return name, true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n < t.setEpoch {
		return "", false
	}
	key := mock + "\x00" + place
	if name, ok := t.runNames[key]; ok {
		return name, !reserved[name]
	}
	if t.epoch != n {
		return "", false
	}
	if name, ok := t.names[key]; ok {
		return name, true
	}
	if t.names == nil {
		t.names, t.taken = map[string]string{}, map[string]bool{}
	}
	if t.setTaken == nil {
		t.setTaken = map[string]bool{}
	}
	name = t.free(varName(place), reserved, t.taken)
	t.names[key], t.taken[name], t.setTaken[name] = name, true, true
	return name, true
}

// runPrefix begins the name of a run's variable: "run-session"
// (models.RunVariablePrefix). A test's variable is named by its place alone,
// never with it (varName), so a run's name, given once for the whole set
// when a test first uses the value, collides with none in any later test.
// A test case's template keys are the test's own, and hold no
// '-' (templates.IsKey): none is a run's name. One given from elsewhere that
// is cannot be renamed in its test, since the run's name is the set's (its
// start-up's bind carries it): the place is refused there (RunName).
const runPrefix = models.RunVariablePrefix

// RunName names, for a request of test number n (its start-up's included),
// the variable bound at place of mock, an exchange of the run (Earlier.Run):
// one name for the whole set, which no test's variable takes, and none a test
// of the set took before it. reserved are the running test's template keys:
// the name takes none of them when it is first given, and ok is false when
// it was given earlier and reserved holds it, so that test refuses the place
// rather than read its own key as the run's variable. ok is false, and no
// name is given, when n is of an earlier set: the set's mock names are that
// set's too, and its run is not this one. A nil tracker, which keeps no set,
// names it by its place alone, apart from reserved.
func (t *Tracker) RunName(n uint64, mock, place string, reserved map[string]bool) (string, bool) {
	if t == nil {
		name := runPrefix + varName(place)
		for i := 2; reserved[name]; i++ {
			name = runPrefix + varName(place) + "-" + strconv.Itoa(i)
		}
		return name, true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if n < t.setEpoch {
		return "", false
	}
	key := mock + "\x00" + place
	if name, ok := t.runNames[key]; ok {
		return name, !reserved[name]
	}
	if t.runNames == nil {
		t.runNames, t.runTaken = map[string]string{}, map[string]bool{}
	}
	name := t.free(runPrefix+varName(place), reserved, t.setTaken)
	t.runNames[key], t.runTaken[name] = name, true
	return name, true
}

// PlaceName names, for a request of test number n, the variable bound at
// p, the earlier place a value came from: the run's variable (RunName) at
// an exchange of the run (Place.Run), the test's own (Name) at any other.
func (t *Tracker) PlaceName(n uint64, p Place, reserved map[string]bool) (string, bool) {
	if p.Run {
		return t.RunName(n, p.Mock, p.Var, reserved)
	}
	return t.Name(n, p.Mock, p.Var, reserved)
}

// free is a name beginning with base that none of reserved, the run's names
// and taken holds. t.mu is held.
func (t *Tracker) free(base string, reserved, taken map[string]bool) string {
	name := base
	for i := 2; taken[name] || t.runTaken[name] || reserved[name]; i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

// SetAppClock makes the tracker read the app's clock through f: the instant
// the app's clock showed at a real one, false where f cannot tell (the
// agent's time-freeze hooks, which move the app's clock; nil: none, and no
// call's window is read, and an instant only a window or a replay's check
// could show a read of the clock is refused at learn: DriftCache.appClock).
func (t *Tracker) SetAppClock(f func(real time.Time) (time.Time, bool)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.appClock = f
	t.drifts.appClock = f != nil
	t.mu.Unlock()
}

// SetAppClockRun makes the tracker read, through f, the real instant from
// which the app's clock has run as one to a real instant: the end of the
// last move the hooks wrote of it by then (the enterprise run clock, which
// a server test's begin moves after the previous test's last serve), false
// where they cannot tell (a move being written). A call's span then starts
// no earlier than that instant (LiveSpan): the app reads its clock for a
// call of a test after the test's request reached it, after the move. A
// call that does not (a background poller that read the clock before the
// move and reaches keploy after it) is checked against the moved clock's
// span, as a read made before the previous serve is checked against that
// serve's: refused where the move is larger than the read's cut, and
// passing or not by when it read only where the move is within a cut or
// two. A server test's call the store served nothing before in the set
// starts its span at the later of the set's begin (NewSetAt) and that
// instant. nil: none, and a span is read only where the clock ran as one
// from its bound, at its two ends, as before; such a call has none.
func (t *Tracker) SetAppClockRun(f func(real time.Time) (time.Time, bool)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.appClockRun = f
	t.mu.Unlock()
}

// PreviousCall finds, of the recordings it sees (See), the recording's
// previous data-plane call before paired: the latest by request time, not
// paired itself. A protocol-phase call (a handshake, a driver's heartbeat)
// is no previous call: a driver makes one whenever it opens a connection,
// after the app read its clock for the call it opens it for. Nor is a call
// still unanswered when paired was sent (its recorded answer came after
// paired's request): the call before a clock read is one the app made and
// had answered before it read, so a call pending across paired's send is
// another of the app's flows' (a handler's write, blocked, sent between the
// consumer's read and its insert), and bounds nothing of paired's read.
// What decides is the recording's own stamps, never a margin. A span the
// skip widens can hold a pick the application made from served data by its
// clock, as a read of the clock would fall there: the classifier's echo
// reads every earlier place for the moment F saw before any window excuses
// it (echoedIn), so such a pick is followed, never noise. It keeps only that
// instant, so a caller can feed it every recording of the set as a store
// streams them, holding none.
type PreviousCall struct {
	id     string
	to, at time.Time
}

// NewPreviousCall starts the search for paired's previous call.
func NewPreviousCall(paired *models.Mock) *PreviousCall {
	if paired == nil {
		return &PreviousCall{}
	}
	return &PreviousCall{id: MockID(paired), to: paired.Spec.ReqTimestampMock}
}

// See considers m as paired's previous call.
func (p *PreviousCall) See(m *models.Mock) {
	if p == nil || m == nil || p.to.IsZero() || m.IsProtocolPhase() || MockID(m) == p.id {
		return
	}
	// Answered after paired was sent: another flow's call, pending across
	// its send. One whose answer time is not known still counts: it may be
	// the previous call, and the later bound only narrows the window.
	if res := m.Spec.ResTimestampMock; !res.IsZero() && res.After(p.to) {
		return
	}
	if at := m.Spec.ReqTimestampMock; !at.IsZero() && at.Before(p.to) && at.After(p.at) {
		p.at = at
	}
}

// At is the previous call's request time, zero when it saw none.
func (p *PreviousCall) At() time.Time {
	if p == nil {
		return time.Time{}
	}
	return p.at
}

// CallWindow is CallWindowFrom with the previous call found among recorded,
// the set's recordings.
func (t *Tracker) CallWindow(paired *models.Mock, recorded []*models.Mock, arrival time.Time) *Window {
	prev := NewPreviousCall(paired)
	for _, m := range recorded {
		prev.See(m)
	}
	return t.CallWindowFrom(prev.At(), paired, arrival)
}

// CallWindowFrom is the window (Window) in which the call that reached
// keploy at the real instant arrival, paired with the recording paired, made
// its clock reads: in the recording, after the recording's previous
// data-plane call (recorded at prevRec, PreviousCall: every recording of the
// set seen, the ones the agent keeps on disk too) and no later than paired;
// in this run, on the app's clock, after the previous data-plane call the
// store served (ServedMockAt), or in a server test the set's begin where it
// served none before it and the hooks tell the clock's run (LiveSpan), and
// no later than arrival. nil when the
// recording has no previous call (the app's start is not recorded), paired
// has no recorded time, no app clock reads arrival and the previous serve,
// or the app's clock did not run as one between them (a test's begin moved
// it): the span F's clock reads from its start to the arrival must be the
// real time between them, exactly, or what the window bounds is no run of
// one clock. Where the hooks tell where the clock's run began
// (SetAppClockRun), this run's half starts no earlier than that: a test's
// first call after its begin moved the clock has a window from the move.
//
// In a client test (SetClientTests), whose begin moves the app's clock, a
// span also starts where the running test began, when that came after the
// previous call: in this run at the test's begin (BeginAt), where the app's
// clock did not run as one from the previous serve to the arrival, or the
// store served none before it; in the recording at the test's recorded
// begin (RecordedBegin), where it came after prevRec or none is recorded.
// The app reads its clock for a call of the test after the test began, so
// a later start only narrows a span, and a test's first call has a window
// where no earlier call bounds it. keploy test's windows start at the
// previous serve, or the set's begin where that came later (setBound), or
// where the clock's run began after it where the hooks tell that
// (SetAppClockRun).
func (t *Tracker) CallWindowFrom(prevRec time.Time, paired *models.Mock, arrival time.Time) *Window {
	if t == nil || paired == nil || paired.Spec.ReqTimestampMock.IsZero() {
		return nil
	}
	lc, recBegan := t.liveClockAt(arrival)
	recFrom := prevRec
	if lc.client && recBegan.After(recFrom) {
		recFrom = recBegan
	}
	if recFrom.IsZero() || !recFrom.Before(paired.Spec.ReqTimestampMock) || lc.clock == nil {
		return nil
	}
	f, ok := lc.span(arrival)
	if !ok {
		return nil
	}
	rec := Span{From: recFrom, To: paired.Spec.ReqTimestampMock}
	if lc.client {
		rec.Began = recBegan
	}
	return &Window{Rec: rec, F: f}
}

// LiveSpan is the span on the app's clock in which the call that reached
// keploy at the real instant arrival made its clock reads in this run, as
// CallWindowFrom reads this run's half of a window: after the previous
// data-plane call the store served (ServedMockAt) and no later than
// arrival, and in a client test from the running test's begin where the
// app's clock did not run as one from that serve, or the store served none
// before it. It reads no recording, so a replay reads it for any call, in
// any cycle (ClockSpanFrom). Where the hooks tell where the app's clock's
// run to arrival began (SetAppClockRun), the span starts no earlier than
// that, so a test's first call after its begin moved the clock has one;
// and a server test's call the store served nothing before in the set
// (the set's first test, where its app made no start-up call) starts at
// the later of the set's begin (NewSetAt) and the run's; nor does a
// server test's span start before the set's begin where a serve of a call
// that reached keploy before it was filed after it. ok is false on a nil
// tracker, where no app clock is set (SetAppClock: keploy cannot tell
// which clock the application reads), where it cannot tell the app's clock
// at either bound, where no bound is known (a server test's call with no
// serve before it in the set, where the hooks tell no run), or where the
// app's clock did not run as one between the bound and arrival: a replay
// then cannot check a read of the clock, and refuses it (CheckClockRead).
// The span carries the tracker's zone (SetAppZone, the agent's own;
// Span.Zone), which ClockSpanFrom replaces by the zone of the call's own
// process where the agent read it.
//
// Where a serve made after the running test began starts the span, the
// span also carries where the test began on the app's clock (Span.TestFrom,
// liveClock.testFrom): another goroutine's call can be served between a
// read of the clock and the call that carries it, so the replay's check
// passes a value that holds from the test's begin too (CheckClockRead).
func (t *Tracker) LiveSpan(arrival time.Time) (Span, bool) {
	if t == nil {
		return Span{}, false
	}
	lc, _ := t.liveClockAt(arrival)
	s, ok := lc.span(arrival)
	if ok {
		s.Zone = lc.zone
		s.TestFrom = lc.testFrom(s.From, arrival)
	}
	return s, ok
}

// liveClock is what the tracker knows, at one moment, of this run's clock
// before a call that reached keploy at a real instant: how to read the
// app's clock, the previous data-plane call before it (prev: the latest
// instant a call the store served before it reached keploy, as record's
// previous call is the latest one answered before the call was sent,
// PreviousCall), the running set's begin, and in a client test the running
// test's begin.
type liveClock struct {
	clock            func(time.Time) (time.Time, bool)
	run              func(time.Time) (time.Time, bool)
	prev, set, began time.Time
	client, open     bool
	zone             *time.Location
}

// liveClockAt is the tracker's liveClock before arrival, and the running
// test's recorded begin (RecordedBegin), read under one lock.
func (t *Tracker) liveClockAt(arrival time.Time) (liveClock, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	lc := liveClock{clock: t.appClock, run: t.appClockRun, client: t.clientTests, open: t.open, set: t.setBegan, began: t.beganAt, zone: t.appZone}
	i := sort.Search(len(t.servedAt), func(i int) bool { return !t.servedAt[i].at.Before(arrival) })
	if i > 0 {
		lc.prev = t.servedAt[i-1].upTo
	}
	return lc, t.recBegan
}

// span is this run's span to arrival (LiveSpan): from the previous serve,
// or where the app's clock's run to arrival began after it (runFrom); in a
// server test with no serve before arrival in the set, from the set's
// begin, or where the run began after it, where the hooks tell the run
// (setBound); in a client test from the test's begin where that came after
// that start and the span from it is none or the clock moved after the
// serve. In a client test it carries the test's begin on the app's clock
// (Span.Began), or where the app's clock's run to arrival began after it
// (runFrom), where the app's clock ran as one from there to arrival.
func (lc liveClock) span(arrival time.Time) (Span, bool) {
	if lc.clock == nil {
		return Span{}, false
	}
	start := lc.runFrom(lc.setBound(), arrival)
	f, ok := lc.from(start, arrival)
	if (!ok || !start.Equal(lc.prev)) && lc.client && lc.began.After(start) {
		if g, gok := lc.from(lc.runFrom(lc.began, arrival), arrival); gok || !ok {
			f, ok = g, gok
		}
	}
	if ok && lc.client {
		if b, bok := lc.from(lc.runFrom(lc.began, arrival), arrival); bok {
			f.Began = b.From
		}
	}
	return f, ok
}

// testFrom is where the running test began on the app's clock (Span.
// TestFrom), or where the app's clock's run to arrival began after it
// (runFrom), where that is before from, the start of the call's span to
// arrival: a serve made after the test began started it. Zero where no test
// is open, where the test began after arrival (a call of the previous test
// checked late), where the app's clock did not run as one from there to
// arrival, and in a server test where the hooks tell no run (the clock may
// have left and come back to the same offset since the test began, as
// setBound reads a set's begin). The app reads its clock for a call of the
// test after the test began: a server test's request reaches the app after
// keploy opened the test, and a client test's harness acts after its
// begin. Whether the read came after another call keploy served since, it
// cannot tell: the application's other goroutines (a queue's consumer)
// make calls of their own beside the test's, whose serves fall between a
// read and the call that carries it.
func (lc liveClock) testFrom(from, arrival time.Time) time.Time {
	if !lc.open || lc.began.IsZero() || (!lc.client && lc.run == nil) {
		return time.Time{}
	}
	b, ok := lc.from(lc.runFrom(lc.began, arrival), arrival)
	if !ok || !b.From.Before(from) {
		return time.Time{}
	}
	return b.From
}

// setBound is the bound a span to arrival starts from: the previous serve;
// in a server test where the store served nothing before arrival in the
// set, the set's begin, where the hooks tell where the app's clock's run
// began (runFrom then starts the span at the later of the two). The app
// reads its clock for a call of the set after the set began. Without the
// run told, the clock may have moved between the set's begin and the call,
// so no bound is known, as before. A serve before the set's begin (a call
// of the set before, which reached keploy before the set began and was
// served after: its serve is filed at its arrival) bounds a server test's
// span at the set's begin instead, which only narrows it. A client test's
// span starts at its own begin instead (span).
func (lc liveClock) setBound() time.Time {
	if lc.client || lc.set.IsZero() {
		return lc.prev
	}
	if lc.prev.IsZero() {
		if lc.run == nil {
			return time.Time{}
		}
		return lc.set
	}
	if lc.set.After(lc.prev) {
		return lc.set
	}
	return lc.prev
}

// runFrom is the start of a span to arrival bounded by bound: where the
// hooks tell where the app's clock's run to arrival began (lc.run), the
// later of the two, and zero (no span) where they cannot tell it; bound
// itself where they tell no runs, or bound is zero (no bound is known).
func (lc liveClock) runFrom(bound, arrival time.Time) time.Time {
	if lc.run == nil || bound.IsZero() {
		return bound
	}
	began, ok := lc.run(arrival)
	if !ok {
		return time.Time{}
	}
	if began.After(bound) {
		return began
	}
	return bound
}

// from is the app's clock's span from the real instant from to arrival, ok
// where the app's clock ran as one between them: the span its clock reads
// must be the real time between them, exactly, or what it bounds is no run
// of one clock.
func (lc liveClock) from(from, arrival time.Time) (Span, bool) {
	if from.IsZero() || from.After(arrival) {
		return Span{}, false
	}
	fFrom, ok := lc.clock(from)
	if !ok {
		return Span{}, false
	}
	fTo, ok := lc.clock(arrival)
	if !ok || fTo.Sub(fFrom) != arrival.Sub(from) {
		return Span{}, false
	}
	return Span{From: fFrom, To: fTo}, true
}

type trackerKey struct{}

// WithTracker puts t on ctx, for the parsers of a connection.
func WithTracker(ctx context.Context, t *Tracker) context.Context {
	return context.WithValue(ctx, trackerKey{}, t)
}

// TrackerFrom is the tracker on ctx, nil when there is none, and nil once the
// test set's learn step has ended (SetLearnEnded): a parser's learn step runs
// only with a tracker, so one that reads it per request decides no request
// of a cycle the learn step does not learn in, and the request is served as
// keploy test serves it. A held learn step (SetLearnHeld) still gives it.
func TrackerFrom(ctx context.Context) *Tracker {
	t, _ := ctx.Value(trackerKey{}).(*Tracker)
	if t.LearnEnded() {
		return nil
	}
	return t
}

// SetLearnEnded says whether the running test set's learn step has ended
// (models.MockFilterParams.LearnEnded): keploy replay's retry cycles of
// record's full round run the passing test cases again out of the order they
// were recorded in, and are judged without learning. While it has ended,
// TrackerFrom gives no tracker. A parser that holds the tracker from an
// earlier read (one kept for a connection's life) reads LearnEnded on every
// request itself, and learns nothing while it is true. The agent sets it with
// the mock store's (integrations.LearnEnded), and only a set's staging or a
// new session takes it back up. A no-op on a nil tracker.
func (t *Tracker) SetLearnEnded(ended bool) {
	if t != nil {
		t.learnEnded.Store(ended)
	}
}

// LearnEnded reports whether the running test set's learn step has ended
// (SetLearnEnded); false on a nil tracker.
func (t *Tracker) LearnEnded() bool {
	return t != nil && t.learnEnded.Load()
}

// SetLearnHeld holds the learn step, or releases it. The agent holds it while
// a test of record's full round that keeps its recording from an earlier
// record is open (the proxy's MockManager.SetLearnHeld): what that test's
// calls change in must be learned onto no recording, its own or the pool's,
// as the record keeps none of it. Unlike an ended learn step it still runs:
// every parser's learn step decides the test's requests, serves what is
// exactly a recording, and states what it served (Served, ServedMockAsRecorded,
// ServedMockLive), as for any test, so a later test of the round reads what
// F gave the application there; only the classification refuses every
// changed value while it is held (DriftCache.held, heldWhy), so a call that
// changed misses. Releasing it takes the learn step back up; a new session
// releases it. A no-op on a nil tracker.
func (t *Tracker) SetLearnHeld(held bool) {
	if t != nil {
		t.learnHeld.Store(held)
	}
}

// LearnHeld reports whether the learn step is held (SetLearnHeld); false on
// a nil tracker.
func (t *Tracker) LearnHeld() bool {
	return t != nil && t.learnHeld.Load()
}

// SetAt is the test number the running set began at (NewSet): it moves at
// every new set, and only there, so a store that keeps the number its
// staging was made under tells whether the tracker began a set the store has
// not staged yet.
func (t *Tracker) SetAt() uint64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.setEpoch
}

// State is the running test's number, whether a test is open (Begin, until
// End) and whether one has begun in this set (Begin, until NewSet), in one
// read: Test, Open and Begun without the copies Test makes. A nil tracker
// answers 0, true, true, as Open and Begun do.
func (t *Tracker) State() (number uint64, open, began bool) {
	if t == nil {
		return 0, true, true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.epoch, t.open, t.began
}

// InServeOrder is earlier with the payload of each exchange a learn step
// served leaf by leaf (Earlier.Payload: keploy's HTTP learn step serves them)
// read as F served it (Raw.ServedIn: the first and the last of its serves in
// the one order F served the set's mocks in, by order), where mocks (those F
// served in the test or before it, as the learn step reads them as earlier
// places) holds its recording; one they do not hold, or order does not
// number, keeps an order keploy cannot tell, which the offset rule fails
// closed on. Of an exchange its adapter did not say F served as recorded
// (Earlier.AsRecorded), what the read's statements say of the serves of
// that recording comes with it (Earlier.told): an adapter that tells it
// after the serve (Tracker.ServedMockAsRecorded, Tracker.ServedMockLive)
// tells it of the recording, not of the exchange, and a later call reads
// the exchange as a recording F gave the application with what was told
// (lateOf), never as one whose view no adapter stated. One said as recorded
// that its leaves do not bear out stays one whose view no adapter stated
// (fail closed). A copy: earlier is the round's. Nothing is numbered where
// no payload was read: the place an earlier test's call told in its own
// test left the run keeps the numbers its serves had when the next test
// began, and one told later keeps none (ranCalls, Raw.ordered).
// Every learn step numbers them by it, so a payload reads the same in each.
func InServeOrder(earlier []Earlier, mocks []*models.Mock, order Order) []Earlier {
	read := false
	for _, e := range earlier {
		read = read || e.Payload.Read()
	}
	if !read {
		return earlier
	}
	byID := make(map[string]*models.Mock, len(mocks))
	var list []*models.Mock
	for _, m := range mocks {
		if m == nil {
			continue
		}
		id := m.Name + "\x00" + m.Spec.ReqTimestampMock.String()
		if _, ok := byID[id]; !ok {
			byID[id] = m
			list = append(list, m)
		}
	}
	spans := order.Spans(list)
	span := make(map[*models.Mock][2]int, len(list))
	for k, m := range list {
		span[m] = spans[k]
	}
	out := append([]Earlier(nil), earlier...)
	for i, e := range out {
		if !e.Payload.Read() {
			continue
		}
		m, ok := byID[e.Payload.Mock()+"\x00"+e.Payload.At().String()]
		if !ok {
			continue
		}
		if span[m][0] > 0 {
			out[i].Payload = e.Payload.ServedIn(span[m][0], span[m][1])
		}
		if !e.AsRecorded && MockID(m) == e.Payload.id {
			out[i].told = &toldOf{order: order, mock: m}
		}
	}
	return out
}
