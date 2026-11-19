package manager

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// timerRule names an application's timer: an update of its jobs collection,
// made every few hundred milliseconds whatever the requests do.
var timerRule = models.WorkerMockRule{ID: "timer", Kind: "Mongo", Match: map[string]string{"command": "update", "collection": "jobs"}}

// lockRule names another of its own calls, on another collection.
var lockRule = models.WorkerMockRule{ID: "lock", Kind: "Mongo", Match: map[string]string{"command": "update", "collection": "locks"}}

// mongoUpdate is the mock of an update of coll made at at, as the Mongo
// recorder writes one.
func mongoUpdate(coll string, at time.Time) *models.Mock {
	m := &models.Mock{Kind: models.Mongo, Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "mongo:27017"}}}
	m.Spec.MongoRequests = []models.MongoRequest{{Message: &models.MongoOpMessage{Sections: []string{
		`{ SectionSingle msg: {"update":"` + coll + `","ordered":true,"$db":"app"} }`,
	}}}}
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(time.Millisecond)
	return m
}

// bgRig is a manager past its startup window (whose mocks every cleanup keeps
// anyway), its output and mappings bound, with rules set for its recording.
func bgRig(t *testing.T, rules ...models.WorkerMockRule) (*SyncMockManager, chan *models.Mock, chan models.TestMockMapping) {
	t.Helper()
	m := New(zap.NewNop())
	out := make(chan *models.Mock, 1000)
	maps := make(chan models.TestMockMapping, 100)
	m.SetOutputChannel(out)
	m.SetMappingChannel(context.Background(), maps)
	m.SetFirstRequestSignaled()
	m.resolvedTestCount = models.StartupMockTestCaseWindow
	m.SetBackgroundRules("", rules)
	return m, out, maps
}

// received is every mock the manager has sent so far.
func received(t *testing.T, m *SyncMockManager, out chan *models.Mock) []*models.Mock {
	t.Helper()
	waitOutQueue(t, m)
	var got []*models.Mock
	for {
		select {
		case mk := <-out:
			got = append(got, mk)
		default:
			return got
		}
	}
}

// mappedTo is the mock names the mappings sent so far give test.
func mappedTo(maps chan models.TestMockMapping, test string) []string {
	var ids []string
	for {
		select {
		case mp := <-maps:
			if mp.TestName == test {
				ids = append(ids, mp.MockIDs...)
			}
		default:
			return ids
		}
	}
}

func collections(mocks []*models.Mock) []string {
	var out []string
	for _, mk := range mocks {
		out = append(out, mk.Spec.MongoRequests[0].Message.(*models.MongoOpMessage).Sections[0][len(`{ SectionSingle msg: {"update":"`):][:4])
	}
	slices.Sort(out)
	return out
}

// A call a rule names, made while a static-dedup duplicate's request was in
// flight, is the application's own, not the duplicate's: the duplicate's
// resolve sends it with no owner, and drops the duplicate's own call as
// before.
func TestABackgroundCallInADuplicatesWindowIsKept(t *testing.T) {
	m, out, _ := bgRig(t, timerRule)
	start := time.Now().Add(-time.Minute)
	m.AddMock(mongoUpdate("jobs", start.Add(time.Millisecond)))
	m.AddMock(mongoUpdate("user", start.Add(2*time.Millisecond)))
	m.ResolveRange(start, start.Add(10*time.Millisecond), "test-0", false, false)

	got := received(t, m, out)
	if c := collections(got); !slices.Equal(c, []string{"jobs"}) {
		t.Fatalf("sent %v; want the timer's call alone (the duplicate's own update dropped)", c)
	}
	if got[0].Name != "" {
		t.Fatalf("the background call was sent named %q, as a test's mock is", got[0].Name)
	}
	if sent, _ := m.BackgroundCalls(); sent != 1 {
		t.Fatalf("background calls sent: %d, want 1", sent)
	}
}

// Without a rule, the same call is the duplicate's debris, as it always was.
func TestWithoutARuleACallInADuplicatesWindowIsDropped(t *testing.T) {
	m, out, _ := bgRig(t)
	start := time.Now().Add(-time.Minute)
	timer := mongoUpdate("jobs", start.Add(time.Millisecond))
	m.AddMock(timer)
	if timer.TestModeInfo.Background {
		t.Fatal("a mock was tagged with no rule set")
	}
	m.ResolveRange(start, start.Add(10*time.Millisecond), "test-0", false, false)
	if got := received(t, m, out); len(got) != 0 {
		t.Fatalf("sent %d mocks of a duplicate's window, want none", len(got))
	}
}

// A call a rule names that no request owns, older than the stale cutoff, is
// sent with no owner by a kept request's resolve; one no rule names is cut.
func TestABackgroundCallPastTheStaleCutoffIsKept(t *testing.T) {
	m, out, _ := bgRig(t, timerRule)
	old := time.Now().Add(-30 * time.Second)
	m.AddMock(mongoUpdate("jobs", old))
	m.AddMock(mongoUpdate("user", old.Add(time.Millisecond)))
	now := time.Now()
	m.ResolveRange(now.Add(-time.Second), now, "test-6", true, false)

	if c := collections(received(t, m, out)); !slices.Equal(c, []string{"jobs"}) {
		t.Fatalf("sent %v; want the timer's call alone", c)
	}
}

// A duplicate's prune (DeleteMocksStrictlyBefore, the queue's path) keeps a
// call a rule names that it would drop.
func TestADuplicatesPruneKeepsABackgroundCall(t *testing.T) {
	m, out, _ := bgRig(t, timerRule)
	now := time.Now()
	m.AddMock(mongoUpdate("jobs", now.Add(-2*time.Second)))
	m.AddMock(mongoUpdate("user", now.Add(-time.Second)))
	m.DeleteMocksStrictlyBefore(now)

	if c := collections(received(t, m, out)); !slices.Equal(c, []string{"jobs"}) {
		t.Fatalf("sent %v; want the timer's call alone", c)
	}
}

// Of the copies of one call the cleanups keep, one is sent: a timer run every
// second would otherwise grow the recording by a mock a second, and the
// worker scope serves one copy to every test. Each call the rules name is
// sent once.
func TestOneCopyOfEachBackgroundCallIsSent(t *testing.T) {
	m, out, _ := bgRig(t, timerRule, lockRule)
	start := time.Now().Add(-time.Minute)
	for i := 0; i < 3; i++ {
		m.AddMock(mongoUpdate("jobs", start.Add(time.Duration(i+1)*time.Millisecond)))
	}
	for i := 0; i < 2; i++ {
		m.AddMock(mongoUpdate("locks", start.Add(time.Duration(i+5)*time.Millisecond)))
	}
	m.ResolveRange(start, start.Add(10*time.Millisecond), "test-0", false, false)
	// And more copies later, in another duplicate's window.
	later := start.Add(time.Second)
	m.AddMock(mongoUpdate("jobs", later.Add(time.Millisecond)))
	m.ResolveRange(later, later.Add(10*time.Millisecond), "test-0", false, false)

	if c := collections(received(t, m, out)); !slices.Equal(c, []string{"jobs", "lock"}) {
		t.Fatalf("sent %v; want one copy of each call", c)
	}
	if sent, repeats := m.BackgroundCalls(); sent != 2 || repeats != 4 {
		t.Fatalf("background calls sent %d, repeats let go %d; want 2 and 4", sent, repeats)
	}
}

// A kept test whose window holds a call a rule names owns it, as it owns any
// other: sent named, in its mapping, every copy. A rule keeps a call from a
// cleanup; it takes nothing from a test.
func TestAKeptTestOwnsTheBackgroundCallsInItsWindow(t *testing.T) {
	m, out, maps := bgRig(t, timerRule)
	for i := 0; i < 2; i++ {
		start := time.Now().Add(-time.Minute + time.Duration(i)*time.Second)
		m.AddMock(mongoUpdate("jobs", start.Add(time.Millisecond)))
		m.ResolveRange(start, start.Add(10*time.Millisecond), "test-7", true, true)
	}
	got := received(t, m, out)
	if len(got) != 2 {
		t.Fatalf("sent %d mocks, want both copies, each its test's", len(got))
	}
	ids := mappedTo(maps, "test-7")
	for _, mk := range got {
		if mk.Name == "" || !slices.Contains(ids, mk.Name) {
			t.Fatalf("mock %q is not in its test's mapping %v", mk.Name, ids)
		}
	}
	if sent, _ := m.BackgroundCalls(); sent != 0 {
		t.Fatalf("%d of a test's own mocks were sent as background calls", sent)
	}
}

// A flush sends a call a rule names that no request owns once no request may
// claim it any more, with no request resolving: the application's timer
// between requests, or with none at all, reaches the recording. A young one
// waits, as a request opened late may still own it, unless the recording is
// stopping.
func TestAFlushSendsAnIdleBackgroundCall(t *testing.T) {
	m, out, _ := bgRig(t, timerRule, lockRule)
	now := time.Now()
	m.AddMock(mongoUpdate("jobs", now.Add(-30*time.Second)))
	m.AddMock(mongoUpdate("locks", now.Add(-time.Second)))
	m.FlushOwnedWindows()
	if c := collections(received(t, m, out)); !slices.Equal(c, []string{"jobs"}) {
		t.Fatalf("the flush sent %v; want the old call alone", c)
	}
	m.FlushOwnedWindows()
	if got := received(t, m, out); len(got) != 0 {
		t.Fatalf("a second flush sent %d, want the young call still waiting", len(got))
	}
	m.NoteStop(time.Now())
	m.FlushOwnedWindows()
	if c := collections(received(t, m, out)); !slices.Equal(c, []string{"lock"}) {
		t.Fatalf("the flush of a stopping recording sent %v; want the young call", c)
	}
}

// A call a rule names that a request in flight may own is held for it, as any
// other: kept, that request's test owns it; ended without it, it is sent with
// no owner.
func TestABackgroundCallARequestInFlightMayClaimIsHeldForIt(t *testing.T) {
	t.Run("kept", func(t *testing.T) {
		m, out, maps := bgRig(t, timerRule)
		start := time.Now().Add(-time.Minute)
		w := m.OpenWindow(start, nil)
		m.AddMock(mongoUpdate("jobs", start.Add(time.Millisecond)))
		m.FlushOwnedWindows() // old enough to be due, but a request in flight may claim it
		if got := received(t, m, out); len(got) != 0 {
			t.Fatalf("the flush sent %d, want the call left for the request in flight", len(got))
		}
		m.DeleteMocksStrictlyBefore(start.Add(5 * time.Millisecond)) // a duplicate's prune
		if got := received(t, m, out); len(got) != 0 {
			t.Fatalf("the prune sent %d, want the call held for the request in flight", len(got))
		}
		m.ResolveKept(w, start, start.Add(10*time.Millisecond), "test-8", true)
		got := received(t, m, out)
		if len(got) != 1 || !slices.Contains(mappedTo(maps, "test-8"), got[0].Name) {
			t.Fatalf("sent %d; want the call in test-8's mapping", len(got))
		}
	})
	t.Run("ended without it", func(t *testing.T) {
		m, out, _ := bgRig(t, timerRule)
		start := time.Now().Add(-time.Minute)
		w := m.OpenWindow(start, nil)
		m.AddMock(mongoUpdate("jobs", start.Add(time.Millisecond)))
		m.AddMock(mongoUpdate("user", start.Add(2*time.Millisecond)))
		m.DeleteMocksStrictlyBefore(start.Add(5 * time.Millisecond))
		w.Close()
		m.FlushOwnedWindows()
		got := received(t, m, out)
		if c := collections(got); !slices.Equal(c, []string{"jobs"}) || got[0].Name != "" {
			t.Fatalf("sent %v; want the timer's call alone, with no owner", c)
		}
	})
}

// Before the first request every copy of a call a rule names is sent: no
// cleanup would drop it there. A recording whose requests the manager never
// learns of (no --sync, no static dedup) sends every mock there for its whole
// run, its tests' own calls among them: a test that makes the call a rule
// names keeps its copy. Only a copy a cleanup would have dropped is one of a
// call.
func TestBeforeTheFirstRequestEveryCopyOfACallARuleNamesIsSent(t *testing.T) {
	m := New(zap.NewNop())
	out := make(chan *models.Mock, 100)
	m.SetOutputChannel(out)
	m.SetBackgroundRules("", []models.WorkerMockRule{timerRule})
	now := time.Now()
	// A boot copy, then 20 tests each making the call itself, the first
	// request never signalled.
	for i := 0; i <= 20; i++ {
		if got := m.AddMock(mongoUpdate("jobs", now.Add(time.Duration(i)*time.Second))); got != MockTaken {
			t.Fatalf("copy %d: %q, want it taken", i, got)
		}
		m.AddMock(mongoUpdate("user", now.Add(time.Duration(i)*time.Second+time.Millisecond)))
	}
	got := collections(received(t, m, out))
	jobs := 0
	for _, c := range got {
		if c == "jobs" {
			jobs++
		}
	}
	if jobs != 21 || len(got) != 42 {
		t.Fatalf("sent %d copies of the call a rule names (of %d mocks); want all 21 of 42", jobs, len(got))
	}
	if sent, repeats := m.BackgroundCalls(); sent != 0 || repeats != 0 {
		t.Fatalf("background calls sent %d, repeats let go %d; want none: no cleanup ran", sent, repeats)
	}
}

// A copy whose send was dropped is forgotten: the call's next copy is sent.
func TestABackgroundCallWhoseSendWasDroppedIsSentAgain(t *testing.T) {
	m, _, _ := bgRig(t, timerRule)
	first, again := mongoUpdate("jobs", time.Now()), mongoUpdate("jobs", time.Now())
	call, repeat := m.backgroundRepeat(first)
	if repeat || !call.ok {
		t.Fatal("the first copy of a call is a repeat")
	}
	if _, repeat := m.backgroundRepeat(again); !repeat {
		t.Fatal("a second copy is sent")
	}
	m.bg.forget(call)
	if _, repeat := m.backgroundRepeat(again); repeat {
		t.Fatal("the copy after a dropped send is let go: the call would never reach the recording")
	}
}

// A manager whose mocks go to several recordings applies each recording's
// rules to its own mocks only, and a recording's rules set again start its
// calls afresh.
func TestBackgroundRulesAreEachRecordingsOwn(t *testing.T) {
	m := New(zap.NewNop())
	m.SetDestinationOf(func(mk *models.Mock) string { return mk.Spec.Metadata["recording"] }, func() string { return "" })
	m.SetBackgroundRules("a", []models.WorkerMockRule{timerRule})
	of := func(rec string) *models.Mock {
		mk := mongoUpdate("jobs", time.Now())
		mk.Spec.Metadata["recording"] = rec
		return mk
	}
	for rec, want := range map[string]bool{"a": true, "b": false, "": false} {
		mk := of(rec)
		m.markBackground(mk, rec, rec != "")
		if mk.TestModeInfo.Background != want {
			t.Errorf("recording %q: tagged %v, want %v", rec, mk.TestModeInfo.Background, want)
		}
	}
	if _, repeat := m.backgroundRepeat(of("a")); repeat {
		t.Fatal("the first copy is a repeat")
	}
	if _, repeat := m.backgroundRepeat(of("b")); repeat {
		t.Fatal("another recording's copy of the call is a repeat")
	}
	m.SetBackgroundRules("a", []models.WorkerMockRule{timerRule})
	if _, repeat := m.backgroundRepeat(of("a")); repeat {
		t.Fatal("a new recording's first copy is a repeat of the last one's")
	}
	m.SetBackgroundRules("a", nil)
	mk := of("a")
	m.markBackground(mk, "a", true)
	if mk.TestModeInfo.Background || m.bg.any.Load() {
		t.Fatal("rules cleared still tag")
	}
}

// A call a rule names waiting in the buffer is settled as any other, so a
// resolve still steps over the buffer it has decided: a timer's calls, always
// waiting, would otherwise have every resolve walk the whole buffer under the
// manager's lock. A flush still sends it once it is due, and the prefix keeps
// the rest, in their order.
func TestAWaitingBackgroundCallStaysSettledAndIsSentOnceDue(t *testing.T) {
	m, out, _ := bgRig(t, timerRule)
	now := time.Now()
	timer := mongoUpdate("jobs", now.Add(-time.Second)) // the oldest, at the front
	m.AddMock(timer)
	for i := 0; i < 50; i++ {
		m.AddMock(mongoUpdate("user", now.Add(time.Duration(i)*time.Millisecond)))
	}
	// A kept test elsewhere decides the buffer; then it is settled whole.
	far := now.Add(time.Hour)
	m.ResolveRange(far, far.Add(time.Millisecond), "test-9", true, false)
	m.FlushOwnedWindows()
	m.mu.Lock()
	settled, buffered := m.settledLocked(), len(m.buffer)
	m.mu.Unlock()
	if buffered != 51 || settled != buffered {
		t.Fatalf("settled prefix %d of %d buffered; want the whole buffer settled with a background call waiting", settled, buffered)
	}
	if got := received(t, m, out); len(got) != 0 {
		t.Fatalf("sent %d before the call was due, want none", len(got))
	}
	// Due now (its time has passed): the next flush finds it in the prefix.
	m.mu.Lock()
	timer.Spec.ReqTimestampMock = now.Add(-time.Minute)
	m.bgOldest = timer.Spec.ReqTimestampMock
	m.mu.Unlock()
	m.FlushOwnedWindows()
	if c := collections(received(t, m, out)); !slices.Equal(c, []string{"jobs"}) {
		t.Fatalf("sent %v; want the background call once due", c)
	}
	m.mu.Lock()
	settled, buffered = m.settledLocked(), len(m.buffer)
	first := m.buffer[0]
	m.mu.Unlock()
	if buffered != 50 || settled != 50 || first.TestModeInfo.Background {
		t.Fatalf("after the send: settled %d of %d buffered; want the other 50 still settled, in order", settled, buffered)
	}
}

// A young call a rule names in the settled prefix is sent when the recording
// stops: no request will claim it then.
func TestASettledBackgroundCallIsSentAtTheStop(t *testing.T) {
	m, out, _ := bgRig(t, timerRule)
	now := time.Now()
	m.AddMock(mongoUpdate("user", now.Add(-time.Second)))
	m.AddMock(mongoUpdate("jobs", now))
	m.FlushOwnedWindows()
	if got := received(t, m, out); len(got) != 0 {
		t.Fatalf("sent %d young calls, want none", len(got))
	}
	m.NoteStop(time.Now())
	m.FlushOwnedWindows()
	if c := collections(received(t, m, out)); !slices.Equal(c, []string{"jobs"}) {
		t.Fatalf("the stopping recording's flush sent %v; want the background call", c)
	}
}

// A background call whose send is dropped (no output wired, so the send
// fails at once) is forgotten by that send, so the call's next copy is sent:
// the call would otherwise never reach the recording.
func TestADroppedBackgroundSendForgetsTheCall(t *testing.T) {
	m := New(zap.NewNop())
	m.SetBackgroundRules("", []models.WorkerMockRule{timerRule})
	first := mongoUpdate("jobs", time.Now())
	first.TestModeInfo.Background = true
	m.sendTaken([]ownedMock{{mock: first, background: true}})
	if _, repeat := m.backgroundRepeat(mongoUpdate("jobs", time.Now())); repeat {
		t.Fatal("the copy after a dropped send is a repeat: the call would never reach the recording")
	}
}

// A background call waiting to be sent is a mock the stop's drain waits for
// (handOnPendingBefore): it was requested before the stop and is not on the
// stream yet.
func TestAKeptBackgroundCallIsPendingUntilSent(t *testing.T) {
	m := New(zap.NewNop())
	at := time.Now().Add(-time.Second)
	m.mu.Lock()
	m.background = append(m.background, mongoUpdate("jobs", at))
	m.mu.Unlock()
	if why := m.handOnPendingBefore(time.Now()); why == "" {
		t.Fatal("a kept background call requested before the stop is not pending")
	}
	if why := m.handOnPendingBefore(at.Add(-time.Second)); why != "" {
		t.Fatalf("a background call requested after the moment asked is pending: %q", why)
	}
}

// A recording's background calls kept but not sent when it ends (its output
// unwired) are let go with it, and a new recording starts with none: the next
// recording's stream is no place for them.
func TestARecordingsKeptBackgroundCallsEndWithIt(t *testing.T) {
	m := New(zap.NewNop())
	keep := func() {
		m.mu.Lock()
		m.background = append(m.background, mongoUpdate("jobs", time.Now()))
		m.noteWaitingBackgroundLocked(time.Now())
		m.mu.Unlock()
	}
	held := func() (int, bool) {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.background), m.bgWaiting
	}
	keep()
	m.NoteStop(time.Now())
	m.EndStoppedRecording()
	if n, _ := held(); n != 0 {
		t.Fatalf("%d background calls kept past their recording's end", n)
	}
	keep()
	m.StartRecordSession()
	if n, waiting := held(); n != 0 || waiting {
		t.Fatalf("a new recording starts with %d background calls kept (waiting %v)", n, waiting)
	}
}

// A recording remembers at most maxBackgroundCalls calls it sent: past it,
// every copy of a new call is sent, so a rule that names a call per request
// cannot grow the set without end.
func TestTheCallsARecordingRemembersAreBounded(t *testing.T) {
	m, _, _ := bgRig(t, timerRule)
	for i := 0; i < maxBackgroundCalls; i++ {
		if _, repeat := m.backgroundRepeat(mongoUpdate(fmt.Sprintf("c%d", i), time.Now())); repeat {
			t.Fatalf("call %d: a new call is a repeat", i)
		}
	}
	for i := 0; i < 2; i++ {
		if _, repeat := m.backgroundRepeat(mongoUpdate("past-the-bound", time.Now())); repeat {
			t.Fatalf("copy %d of a call past the bound is let go; want every copy sent", i)
		}
	}
	m.bg.mu.Lock()
	n := len(m.bg.sent[""])
	m.bg.mu.Unlock()
	if n != maxBackgroundCalls {
		t.Fatalf("the recording remembers %d calls, want %d", n, maxBackgroundCalls)
	}
}

// Under the proven switch a recording's rules are not set, so no mock is
// tagged: the proven model places the worker scope by its own rules.
func TestNoRuleIsSetUnderTheProvenSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := New(zap.NewNop())
	m.SetBackgroundRules("", []models.WorkerMockRule{timerRule})
	if m.bg.any.Load() {
		t.Fatal("rules set under the proven switch")
	}
	mk := mongoUpdate("jobs", time.Now())
	m.markBackground(mk, "", true)
	if mk.TestModeInfo.Background {
		t.Fatal("a mock was tagged under the proven switch")
	}
}

// BenchmarkResolveWithAWaitingBackgroundCall is
// BenchmarkResolveOverABufferAndAFullRing's resolve with a worker-mock rule set
// and one call it names waiting at the front of the buffer, as a timer's
// always is: a resolve must still step over the buffer it settled, not walk it
// all again. Run it with a fixed count (-benchtime=200x).
func BenchmarkResolveWithAWaitingBackgroundCall(b *testing.B) {
	for _, buffered := range []int{10000, 100000} {
		b.Run(fmt.Sprintf("buffered=%d/ring=%d", buffered, maxRecentWindows), func(b *testing.B) {
			rb := newResolveBench(maxRecentWindows, 0)
			rb.mgr.SetBackgroundRules("", []models.WorkerMockRule{timerRule})
			rb.mgr.AddMock(mongoUpdate("jobs", rb.gap(0, 0))) // young: it waits
			gaps := max(rb.ring, 1)
			for i := range buffered {
				rb.mgr.AddMock(httpMockAt(rb.gap(i%gaps, 1+i/gaps)))
			}
			far := rb.s0.Add(24 * time.Hour)
			rb.mgr.ResolveRange(far, far, "warm", true, false)
			sentMocks(rb.out)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				at := rb.s0.Add(-time.Duration(i+1) * time.Millisecond)
				rb.mgr.AddMock(httpMockAt(at))
				rb.mgr.ResolveRange(at, at.Add(time.Microsecond), "t", true, false)
				rb.drain()
			}
			b.StopTimer()
			rb.mgr.mu.Lock()
			settled, n := rb.mgr.settledLocked(), len(rb.mgr.buffer)
			rb.mgr.mu.Unlock()
			if n != buffered+1 || settled != n {
				b.Fatalf("settled prefix %d of %d buffered; want the whole buffer settled with the background call waiting", settled, n)
			}
		})
	}
}
