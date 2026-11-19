package manager

import (
	"context"
	"slices"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
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

// A copy of a call sent already before the first request is let go too, and
// is no loss: AddMock takes it.
func TestBeforeTheFirstRequestOneCopyOfABackgroundCallIsSent(t *testing.T) {
	m := New(zap.NewNop())
	out := make(chan *models.Mock, 100)
	m.SetOutputChannel(out)
	m.SetBackgroundRules("", []models.WorkerMockRule{timerRule})
	now := time.Now()
	for i := 0; i < 3; i++ {
		if got := m.AddMock(mongoUpdate("jobs", now.Add(time.Duration(i)*time.Millisecond))); got != MockTaken {
			t.Fatalf("copy %d: %q, want it taken", i, got)
		}
	}
	m.AddMock(mongoUpdate("user", now))
	m.AddMock(mongoUpdate("user", now.Add(time.Millisecond)))
	if c := collections(received(t, m, out)); !slices.Equal(c, []string{"jobs", "user", "user"}) {
		t.Fatalf("sent %v; want one copy of the timer's call and every other call", c)
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

// A call a rule names is never left in the settled prefix, which a flush steps
// over: the next flush must look at it again, to send it once it is due.
func TestABackgroundCallIsNeverSettled(t *testing.T) {
	m, out, _ := bgRig(t, timerRule)
	now := time.Now()
	m.AddMock(mongoUpdate("user", now.Add(-2*time.Second)))
	timer := mongoUpdate("jobs", now.Add(-time.Second))
	m.AddMock(timer)
	m.FlushOwnedWindows()
	m.mu.Lock()
	settled := m.settledLocked()
	m.mu.Unlock()
	if settled != 1 {
		t.Fatalf("settled prefix %d; want the call before the background call alone", settled)
	}
	// Due now: the next flush finds it.
	m.mu.Lock()
	timer.Spec.ReqTimestampMock = now.Add(-time.Minute)
	m.mu.Unlock()
	m.FlushOwnedWindows()
	if c := collections(received(t, m, out)); !slices.Equal(c, []string{"jobs"}) {
		t.Fatalf("sent %v; want the background call once due", c)
	}
}
