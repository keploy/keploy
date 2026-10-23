package learn

import (
	"context"
	"sort"
	"strconv"
	"sync"
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
	// kept are the identities (mockID) in mocks: a mock served again is kept
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
}

// NewTracker makes a tracker with no test open.
func NewTracker() *Tracker {
	return &Tracker{drifts: NewDriftCache()}
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

// waitGiven waits while the running test awaits its test case's request
// (awaiting), t.mu held on entry and on return, released while it waits:
// until Give brings it, or the test's window closes without it, or done is
// closed. false where done closed while the test still awaited it.
//
// What it answers is read from the tracker as the call wakes, never from
// which of the two the scheduler showed it first: where the request came
// (or the window closed) by then, true, as for a call that read the places
// after it; where the test still awaits it and done is closed, false.
func (t *Tracker) waitGiven(done <-chan struct{}) bool {
	for t.awaiting != nil {
		if closed(done) {
			return false
		}
		ch, hook := t.awaiting, t.onAwait
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
// it (End, the next Begin, NewSet), or ctx ends, whose error it returns. A
// call that read the test's places while it awaited (Places) reads them
// again only then: the replay sends the request only after the filter that
// brings it is answered, so a call made in between is the test's background
// (a pool call, a call of the previous test's flow), and it is learned from
// as one made after the request came, whichever way the scheduler ordered
// the two. A tracker no test awaits a request of returns at once. It holds
// no lock of the tracker's while it waits; its caller must hold none that
// Give, End or a Begin takes.
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
// mock's client tests where the tracker's are (SetClientTests). t.mu is held.
func (t *Tracker) newDrifts() *DriftCache {
	dc := NewDriftCache()
	dc.clientTests, dc.zone = t.clientTests, t.appZone
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

// mockID is a served mock's identity: its name, which is unique within one
// set's recording only, with its kind and recorded times.
func mockID(m *models.Mock) string {
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
// such a value refused, a false red that fails closed. The agent sets it
// once, before any test.
func (t *Tracker) SetAppZone(loc *time.Location) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.appZone = loc
	t.drifts.zone = loc
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
// places after then is refused. At the next Begin a call still waiting reads
// the next test's places instead, waiting again where that test awaits its
// own request. BeginAwaitingAt
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
	if t == nil {
		return 0, nil, nil, nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	before := append(make([]*models.Mock, 0, len(t.before)+len(t.outside)), t.before...)
	seen := make(map[string]bool, len(t.before))
	for _, m := range t.before {
		seen[mockID(m)] = true
	}
	for _, m := range t.outside {
		if !seen[mockID(m)] {
			before = append(before, m)
		}
	}
	earlier := make([]Earlier, 0, len(t.run)+len(t.served)+1)
	if t.given != nil {
		earlier = append(earlier, *t.given)
	}
	earlier = append(append(earlier, t.run...), t.served...)
	return t.epoch, earlier, append([]*models.Mock(nil), t.mocks...), before, t.awaiting != nil
}

// ServedMock notes a mock any parser served in the running test: an earlier
// place of the test's recording, whatever its kind, for Classify (Raw); and,
// a data-plane one, a call of the app's that reached keploy now
// (CallWindow).
func (t *Tracker) ServedMock(m *models.Mock) { t.ServedMockAt(m, time.Now()) }

// ServedMockAt is ServedMock of a mock served at the real instant at.
func (t *Tracker) ServedMockAt(m *models.Mock, at time.Time) {
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
	id := mockID(m)
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
// set. A set has no test's own exchange (Own) before its first test: one
// served then took its scope from the previous set, and is nobody's.
func (t *Tracker) Served(n uint64, e Earlier) {
	if t == nil {
		return
	}
	t.mu.Lock()
	switch {
	case t.epoch != n:
	case !t.open && !t.began && e.Own:
	case !t.open && !t.began && drifted(e):
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
// call's window is read).
func (t *Tracker) SetAppClock(f func(real time.Time) (time.Time, bool)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.appClock = f
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
	return &PreviousCall{id: mockID(paired), to: paired.Spec.ReqTimestampMock}
}

// See considers m as paired's previous call.
func (p *PreviousCall) See(m *models.Mock) {
	if p == nil || m == nil || p.to.IsZero() || m.IsProtocolPhase() || mockID(m) == p.id {
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
// one clock.
//
// In a client test (SetClientTests), whose begin moves the app's clock, a
// span also starts where the running test began, when that came after the
// previous call: in this run at the test's begin (BeginAt), where the app's
// clock did not run as one from the previous serve to the arrival, or the
// store served none before it; in the recording at the test's recorded
// begin (RecordedBegin), where it came after prevRec or none is recorded.
// The app reads its clock for a call of the test after the test began, so
// a later start only narrows a span, and a test's first call has a window
// where no earlier call bounds it. keploy test's windows are as they were.
func (t *Tracker) CallWindowFrom(prevRec time.Time, paired *models.Mock, arrival time.Time) *Window {
	if t == nil || paired == nil || paired.Spec.ReqTimestampMock.IsZero() {
		return nil
	}
	t.mu.Lock()
	clock := t.appClock
	i := sort.Search(len(t.servedAt), func(i int) bool { return !t.servedAt[i].Before(arrival) })
	var prev time.Time
	if i > 0 {
		prev = t.servedAt[i-1]
	}
	client, began, recBegan := t.clientTests, t.beganAt, t.recBegan
	t.mu.Unlock()
	recFrom := prevRec
	if client && recBegan.After(recFrom) {
		recFrom = recBegan
	}
	if recFrom.IsZero() || !recFrom.Before(paired.Spec.ReqTimestampMock) || clock == nil {
		return nil
	}
	// span is F's span from the real instant from to the arrival, ok where
	// the app's clock ran as one between them.
	span := func(from time.Time) (Span, bool) {
		if from.IsZero() || from.After(arrival) {
			return Span{}, false
		}
		fFrom, ok := clock(from)
		if !ok {
			return Span{}, false
		}
		fTo, ok := clock(arrival)
		if !ok || fTo.Sub(fFrom) != arrival.Sub(from) {
			return Span{}, false
		}
		return Span{From: fFrom, To: fTo}, true
	}
	f, ok := span(prev)
	if !ok && client && began.After(prev) {
		f, ok = span(began)
	}
	if !ok {
		return nil
	}
	return &Window{Rec: Span{From: recFrom, To: paired.Spec.ReqTimestampMock}, F: f}
}

type trackerKey struct{}

// WithTracker puts t on ctx, for the parsers of a connection.
func WithTracker(ctx context.Context, t *Tracker) context.Context {
	return context.WithValue(ctx, trackerKey{}, t)
}

// TrackerFrom is the tracker on ctx, nil when there is none.
func TrackerFrom(ctx context.Context) *Tracker {
	t, _ := ctx.Value(trackerKey{}).(*Tracker)
	return t
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
