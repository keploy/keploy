package learn

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
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
	// drifted from its recording): an earlier place of every test of the set
	// (Run). runNames are the variables bound at them, by mock and place, and
	// runTaken their names, which no test's variable takes. setTaken are the
	// names every test of the set has taken so far, which no run's variable
	// takes: a run's variable is named when a test first uses it, after
	// earlier tests named their own.
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
	// servedAt are the real instants the store served the running set's
	// data-plane mocks at, in order (ServedMockAt): when the app's calls the
	// replay answered reached keploy. NewSet forgets them.
	servedAt []time.Time
	// given is the running test case's own request (Give), an earlier place
	// the learn step reads as any other (Earlier.Given); nil until it is
	// given, and for a test no keploy test case opened (keploy mock's).
	// Begin forgets it.
	given *Earlier
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
}

// NewTracker makes a tracker with no test open.
func NewTracker() *Tracker {
	return &Tracker{drifts: NewDriftCache(), places: newSetPlaces()}
}

// NewSet starts the next test set's replay on the same agent: what was served
// before it (the previous set's tests, its start-up, between its tests) is no
// earlier place of the new set's, and no test is open until the next Begin.
// Mock names restart in every set (mock-0 again), so a mock served in one set
// must not stand for, or hide, another set's mock of its name: the set's
// start-up takes a test number no request of the previous set holds, so a
// request of that set answered after the set changed is nobody's, and names
// none of the new set's variables, its run's neither (Name, RunName).
func (t *Tracker) NewSet() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.epoch++
	t.setEpoch = t.epoch
	t.served = nil
	t.mocks, t.kept, t.before = nil, nil, nil
	t.set, t.serves, t.seq = nil, nil, 0
	t.flights, t.lifts, t.handshakes = nil, nil, nil
	t.places = newSetPlaces()
	t.outside, t.outsideKept = nil, nil
	t.run, t.runNames, t.runTaken, t.setTaken = nil, nil, nil, nil
	t.names, t.taken = nil, nil
	t.answered, t.drifts = nil, t.newDrifts()
	t.servedAt = nil
	t.given = nil
	say := t.closeUnread()
	t.open, t.began = false, false
	t.beganAt, t.recBegan, t.nextRecBegan = time.Time{}, time.Time{}, time.Time{}
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
	dc.clientTests, dc.zone, dc.appClock = t.clientTests, t.appZone, t.appClock != nil
	dc.zoneName, dc.zoneNoName = t.drifts.zoneName, t.drifts.zoneNoName
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
	t.served = nil
	t.before, t.mocks, t.kept = t.mocks, nil, nil
	t.answered = nil
	t.drifts = t.newDrifts()
	t.names, t.taken = nil, nil
	t.given = nil
	t.open, t.began = true, true
	t.beganAt = at
	t.recBegan, t.nextRecBegan = t.nextRecBegan, time.Time{}
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
// own request first (Give), then what the learn step served in it so
// far, leaf by leaf; every mock any parser served in it so far
// (ServedMock), and the mocks served before it began: those served since the
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
	earlier := make([]Earlier, 0, len(t.run)+len(t.served)+1)
	if t.given != nil {
		earlier = append(earlier, *t.given)
	}
	earlier = append(append(earlier, t.run...), t.served...)
	return Placed{
		Test: t.epoch, Earlier: earlier, Served: append([]*models.Mock(nil), t.mocks...), Before: before, Awaiting: t.awaiting != nil,
		Open: t.open, Begun: t.began, SetAt: t.setEpoch,
		Set:   t.set[:len(t.set):len(t.set)],
		Order: Order{t: t, serves: t.serves, upTo: t.seq, places: t.places, flights: t.flights, handshakes: t.handshakes, set: t.setEpoch, landings: t.landings, flying: t.flying},
	}
}

// ServedMock notes a mock any parser served in the running test: an earlier
// place of the test's recording, whatever its kind, for Classify (Raw); and,
// a data-plane one, a call of the app's that reached keploy now
// (CallWindow).
func (t *Tracker) ServedMock(m *models.Mock) { t.ServedMockAt(m, time.Now()) }

// ServedMockAt is ServedMock of a mock served at the real instant at.
func (t *Tracker) ServedMockAt(m *models.Mock, at time.Time) { t.servedIn(m, at, nil, serveKind{}) }

// ServedMockIn is ServedMock of a mock served in the request in, as the
// parser that served it said (integrations.ServingIn): the serve is that
// request's whatever other requests are aloft (Flight); in NoRequest, no
// request's; in one a request caused (Flight.LiftCaused), that one's. A nil
// in is ServedMock.
func (t *Tracker) ServedMockIn(m *models.Mock, in *Flight) {
	t.servedIn(m, time.Now(), in, serveKind{})
}

// ServedMockAtIn is ServedMockIn of a mock served at the real instant at.
func (t *Tracker) ServedMockAtIn(m *models.Mock, at time.Time, in *Flight) {
	t.servedIn(m, at, in, serveKind{})
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
	t.servedIn(m, time.Now(), in, serveKind{handshake: true, placed: chosen == in})
}

// servedIn is ServedMockAt of a serve made in the request in (nil: not
// said), told as k says (serveKind).
func (t *Tracker) servedIn(m *models.Mock, at time.Time, in *Flight, k serveKind) {
	if t == nil || m == nil {
		return
	}
	t.mu.Lock()
	if !m.IsProtocolPhase() {
		i := sort.Search(len(t.servedAt), func(i int) bool { return t.servedAt[i].After(at) })
		t.servedAt = append(t.servedAt, time.Time{})
		copy(t.servedAt[i+1:], t.servedAt[i:])
		t.servedAt[i] = at
	}
	id := MockID(m)
	if t.serves == nil {
		t.serves = map[string][]int{}
	}
	if len(t.serves[id]) == 0 {
		t.set = append(t.set, m)
	}
	t.seq++
	t.serves[id] = append(t.serves[id], t.seq)
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
	upTo   int
	places *setPlaces
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
// the next, lands there too, and the next Begin forgets it. Served before the
// set's first test began (the start-up's), it is the run's when it carried a
// value the run made (Earlier.Run): an earlier place of every test of the
// set. Either way it is the start-up's (Earlier.StartUp). A set has no
// test's own exchange (Own) before its first test: one served then took its
// scope from the previous set, and is nobody's.
func (t *Tracker) Served(n uint64, e Earlier) {
	if t == nil {
		return
	}
	t.mu.Lock()
	e.StartUp = !t.open && !t.began
	switch {
	case t.epoch != n:
	case e.StartUp && e.Own:
	case e.StartUp && drifted(e):
		e.Run = true
		t.run = append(t.run, e)
	default:
		t.served = append(t.served, e)
	}
	t.mu.Unlock()
}

// drifted reports whether an exchange's request carried a value other than
// its recording's at some place a variable can hold.
func drifted(e Earlier) bool {
	for _, s := range e.Seen {
		if s.Side == Request && s.Place != "" && s.Live != "" && s.Live != s.Leaf.Value {
			return true
		}
	}
	return false
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

// runPrefix begins the name of a run's variable: "run-session". A test's
// variable is named by its place alone, so a run's name, given once for the
// whole set when a test first uses the value, collides with none in any
// later test. A test case's template keys are the test's own, and hold no
// '-' (templates.IsKey): none is a run's name. One given from elsewhere that
// is cannot be renamed in its test, since the run's name is the set's (its
// start-up's bind carries it): the place is refused there (RunName).
const runPrefix = "run-"

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
// two. nil: none, and a span is read only where the clock ran as one from
// its bound, at its two ends, as before.
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
// store served (ServedMockAt) and no later than arrival. nil when the
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
// previous serve, or where the clock's run began after it where the hooks
// tell that (SetAppClockRun).
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
	return &Window{Rec: Span{From: recFrom, To: paired.Spec.ReqTimestampMock}, F: f}
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
// that, so a test's first call after its begin moved the clock has one. ok
// is false on a nil tracker, where no app
// clock is set (SetAppClock: keploy cannot tell which clock the application
// reads), where it cannot tell the app's clock at either bound, where no
// bound is known, or where the app's clock did not run as one between the
// bound and arrival: a replay then cannot check a read of the clock, and
// refuses it (CheckClockRead). The span carries the tracker's zone
// (SetAppZone, the agent's own; Span.Zone), which ClockSpanFrom replaces
// by the zone of the call's own process where the agent read it.
func (t *Tracker) LiveSpan(arrival time.Time) (Span, bool) {
	if t == nil {
		return Span{}, false
	}
	lc, _ := t.liveClockAt(arrival)
	s, ok := lc.span(arrival)
	if ok {
		s.Zone = lc.zone
	}
	return s, ok
}

// liveClock is what the tracker knows, at one moment, of this run's clock
// before a call that reached keploy at a real instant: how to read the
// app's clock, the previous data-plane serve before the call, and in a
// client test the running test's begin.
type liveClock struct {
	clock       func(time.Time) (time.Time, bool)
	run         func(time.Time) (time.Time, bool)
	prev, began time.Time
	client      bool
	zone        *time.Location
}

// liveClockAt is the tracker's liveClock before arrival, and the running
// test's recorded begin (RecordedBegin), read under one lock.
func (t *Tracker) liveClockAt(arrival time.Time) (liveClock, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	lc := liveClock{clock: t.appClock, run: t.appClockRun, client: t.clientTests, began: t.beganAt, zone: t.appZone}
	i := sort.Search(len(t.servedAt), func(i int) bool { return !t.servedAt[i].Before(arrival) })
	if i > 0 {
		lc.prev = t.servedAt[i-1]
	}
	return lc, t.recBegan
}

// span is this run's span to arrival (LiveSpan): from the previous serve,
// or where the app's clock's run to arrival began after it (runFrom); in a
// client test from the test's begin where that came after that start and
// the span from it is none or the clock moved after the serve.
func (lc liveClock) span(arrival time.Time) (Span, bool) {
	if lc.clock == nil {
		return Span{}, false
	}
	start := lc.runFrom(lc.prev, arrival)
	f, ok := lc.from(start, arrival)
	if (!ok || !start.Equal(lc.prev)) && lc.client && lc.began.After(start) {
		if g, gok := lc.from(lc.runFrom(lc.began, arrival), arrival); gok || !ok {
			f, ok = g, gok
		}
	}
	return f, ok
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
// only with a tracker, so one that reads it per request decides no request of
// a cycle the learn step does not learn in, and the request is served as
// keploy test serves it.
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
// closed on. A copy: earlier is the round's. Nothing is numbered where no
// payload was read. Every learn step numbers them by it, so a payload reads
// the same in each.
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
		if m, ok := byID[e.Payload.Mock()+"\x00"+e.Payload.At().String()]; ok && span[m][0] > 0 {
			out[i].Payload = e.Payload.ServedIn(span[m][0], span[m][1])
		}
	}
	return out
}
