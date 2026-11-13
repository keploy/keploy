package manager

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// staleCutoffFixture is a manager past its start-up window, its output wired,
// with every mock it tells OnDrop of collected.
func staleCutoffFixture(t *testing.T) (*SyncMockManager, chan *models.Mock, func() []*models.Mock) {
	t.Helper()
	ch := make(chan *models.Mock, 16)
	mgr := &SyncMockManager{}
	mgr.SetOutputChannel(ch)
	mgr.SetFirstRequestSignaled()
	mgr.resolvedTestCount = models.StartupMockTestCaseWindow
	var mu sync.Mutex
	var told []*models.Mock
	mgr.OnDrop(func(m *models.Mock) {
		mu.Lock()
		told = append(told, m)
		mu.Unlock()
	})
	return mgr, ch, func() []*models.Mock {
		mu.Lock()
		defer mu.Unlock()
		return append([]*models.Mock(nil), told...)
	}
}

// cutLogs has mgr log to the returned observer, at Debug.
func cutLogs(mgr *SyncMockManager) *observer.ObservedLogs {
	core, logs := observer.New(zapcore.DebugLevel)
	mgr.SetLogger(zap.New(core))
	return logs
}

// drained returns what ch holds now.
func drained(ch chan *models.Mock) []*models.Mock {
	var out []*models.Mock
	for {
		select {
		case m := <-ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

// staleAt is a per-test mock whose call starts at req and lasts d.
func staleAt(req time.Time, d time.Duration) *models.Mock {
	return &models.Mock{Kind: models.Mongo, Spec: models.MockSpec{
		ReqTimestampMock: req,
		ResTimestampMock: req.Add(d),
	}}
}

// cutLines are the lines logs holds that start with msg's prefix of a stale
// cutoff's summary or left-out line.
func cutLines(logs *observer.ObservedLogs, prefix string) []observer.LoggedEntry {
	return logs.FilterMessageSnippet(prefix).AllUntimed()
}

const (
	cutSummary = "were dropped at the stale cutoff"
	cutLeft    = "syncMock: a test case was left out: a mock requested inside its window was dropped at the stale cutoff"
)

// TestAWindowlessRequestsMockPastTheStaleCutoffIsNeverSavedWithout is the
// proxyless shape: the ingress opens no window (OpenWindow), so nothing tells
// the manager that request A is in flight. A's egress call is buffered, and
// another request's resolve, more than StaleHorizon later, comes before A's
// response is complete. A's test case must then either carry its mock, or be
// left out (SendingTestCase false) with the drop told to OnDrop and A named
// at WARN: never sent without it.
func TestAWindowlessRequestsMockPastTheStaleCutoffIsNeverSavedWithout(t *testing.T) {
	mgr, ch, told := staleCutoffFixture(t)
	logs := cutLogs(mgr)
	now := time.Now()
	reqA := now.Add(-10 * time.Second)
	mock := staleAt(reqA.Add(time.Millisecond), time.Millisecond)
	mgr.AddMock(mock)

	// B, a short request, is decided while A is still in flight.
	mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
	// Then A's response completes and A is decided.
	endA := time.Now()
	mgr.ResolveRange(reqA, endA, "test-A", true, false)

	carried := false
	for _, m := range drained(ch) {
		if m == mock {
			carried = true
		}
	}
	sent := mgr.SendingTestCase("test-A", reqA, endA)
	switch {
	case carried && sent:
		// Kept with its mock.
	case carried:
		t.Fatalf("A's mock was handed on, yet A's test case was left out")
	case sent:
		t.Fatalf("A's test case was sent without its mock: the stale cutoff dropped the mock of a request in flight and nothing left A out (OnDrop told %d mocks)", len(told()))
	default:
		if got := told(); len(got) != 1 || got[0] != mock {
			t.Fatalf("A was left out, but OnDrop was told %d mocks, want A's mock once", len(got))
		}
		if got := mgr.cutDrops.Load(); got != 1 {
			t.Fatalf("stale cutoff drops counted %d, want 1", got)
		}
		left := cutLines(logs, cutLeft)
		if len(left) != 1 || left[0].Level != zapcore.WarnLevel || left[0].ContextMap()["test_case"] != "test-A" {
			t.Fatalf("the left-out test case is not named at WARN once: %+v", left)
		}
		sum := cutLines(logs, cutSummary)
		if len(sum) != 1 || sum[0].Level != zapcore.DebugLevel || sum[0].ContextMap()["revoked_sent"] != int64(0) {
			t.Fatalf("a drop that revoked nothing is not said once at Debug: %+v", sum)
		}
		if _, held := sum[0].ContextMap()[heldThenLetGo]; held {
			t.Fatal("the cutoff's own drop is said as one the hold let go of")
		}
	}
}

// TestAWindowedRequestsMockIsHeldPastTheStaleCutoff: a request that opened its
// window keeps its mock past the cutoff (held), so nothing is dropped, told or
// left out, and its test case is sent with the mock.
func TestAWindowedRequestsMockIsHeldPastTheStaleCutoff(t *testing.T) {
	mgr, ch, told := staleCutoffFixture(t)
	now := time.Now()
	reqA := now.Add(-10 * time.Second)
	w := mgr.OpenWindow(reqA, nil)
	mock := staleAt(reqA.Add(time.Millisecond), time.Millisecond)
	mgr.AddMock(mock)

	mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
	if got := told(); len(got) != 0 {
		t.Fatalf("OnDrop told %d mocks of a request whose window is open, want none", len(got))
	}
	if !w.Keep() {
		t.Fatal("A's window was given up")
	}
	endA := time.Now()
	mgr.ResolveKept(w, reqA, endA, "test-A", false)
	w.Close()
	carried := false
	for _, m := range drained(ch) {
		carried = carried || m == mock
	}
	if !carried {
		t.Fatal("A's mock was not handed on with its kept window")
	}
	if !mgr.SendingTestCase("test-A", reqA, endA) {
		t.Fatal("A's test case was left out though it kept its mock")
	}
	if got := mgr.cutDrops.Load(); got != 0 {
		t.Fatalf("stale cutoff drops counted %d, want 0", got)
	}
}

// TestAnOrphanedStaleMockIsDroppedToldAndCountedLeavingOutOnlyAWindowHoldingIt:
// a call no request made (no window holds its request) is dropped, counted and
// told; a test case sent whose window overlaps the call but started after its
// request keeps its place, and a later one is sent.
func TestAnOrphanedStaleMockIsDroppedToldAndCountedLeavingOutOnlyAWindowHoldingIt(t *testing.T) {
	mgr, ch, told := staleCutoffFixture(t)
	now := time.Now()
	req := now.Add(-20 * time.Second)
	mock := staleAt(req, 5*time.Second)
	mgr.AddMock(mock)
	if !mgr.SendingTestCase("test-C", req.Add(time.Second), req.Add(2*time.Second)) {
		t.Fatal("C, sent before the drop, was left out")
	}

	mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
	for _, m := range drained(ch) {
		if m == mock {
			t.Fatal("an orphaned stale mock was handed on")
		}
	}
	if got := told(); len(got) != 1 || got[0] != mock {
		t.Fatalf("OnDrop told %d mocks, want the orphan once", len(got))
	}
	if got := mgr.cutDrops.Load(); got != 1 {
		t.Fatalf("stale cutoff drops counted %d, want 1", got)
	}
	if mgr.WasMockDroppedForTC("test-C") {
		t.Fatal("C, whose window overlaps the call but does not hold its request, was revoked")
	}
	if !mgr.SendingTestCase("test-D", req.Add(time.Millisecond), now) {
		t.Fatal("D, whose window does not hold the dropped mock's request, was left out")
	}
	if mgr.SendingTestCase("test-E", req.Add(-time.Millisecond), req.Add(time.Millisecond)) {
		t.Fatal("E, whose window holds the dropped mock's request, was sent")
	}
}

// TestAStaleCutoffRevokesTheSentTestCasesHoldingItsRequestByName: test cases
// the stream sent already whose window holds a dropped mock's request (their
// windows left the ring, or opened none) are revoked by name, said at WARN
// with the first dropNamesMax named, for a client that deletes them or not.
func TestAStaleCutoffRevokesTheSentTestCasesHoldingItsRequestByName(t *testing.T) {
	for _, deletes := range []bool{true, false} {
		t.Run(fmt.Sprintf("deletes=%v", deletes), func(t *testing.T) {
			mgr, _, _ := staleCutoffFixture(t)
			mgr.SetRevokeCapable(deletes)
			logs := cutLogs(mgr)
			now := time.Now()
			req := now.Add(-10 * time.Second)
			const held = dropNamesMax + 1
			for i := range held {
				if !mgr.SendingTestCase(fmt.Sprintf("test-S%02d", i), req.Add(-time.Duration(i+1)*time.Millisecond), req.Add(time.Millisecond)) {
					t.Fatalf("S%02d left out before the drop", i)
				}
			}
			mgr.AddMock(staleAt(req, time.Millisecond))
			mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)

			for i := range held {
				if !mgr.WasMockDroppedForTC(fmt.Sprintf("test-S%02d", i)) {
					t.Fatalf("S%02d, sent with the dropped mock's request in its window, was not revoked", i)
				}
			}
			sum := cutLines(logs, cutSummary)
			if len(sum) != 1 || sum[0].Level != zapcore.WarnLevel {
				t.Fatalf("a drop that revoked sent test cases is not said once at WARN: %+v", sum)
			}
			c := sum[0].ContextMap()
			names, _ := c["revoked_sent_names"].([]any)
			if c["revoked_sent"] != int64(held) || len(names) != dropNamesMax || names[0] != fmt.Sprintf("test-S%02d", held-1) || c["client_deletes_revoked"] != deletes {
				t.Fatalf("summary says %+v, want %d revoked, the %d that started first named", c, held, dropNamesMax)
			}
			if _, said := c["unnamed_sent_through"]; said {
				t.Fatal("a drop that reached no unnamed sent test case says one")
			}
			if len(cutLines(logs, "stay recorded without it, as the client that reads the stream cannot delete them")) != 0 {
				t.Fatal("a stale cutoff's revoke is said as a loss on the way out")
			}
		})
	}
}

// TestAStaleCutoffWithNoRecordingOpenLeavesOutNone: a manager whose output is
// not set drops a stale mock and tells OnDrop, but leaves out no test case.
func TestAStaleCutoffWithNoRecordingOpenLeavesOutNone(t *testing.T) {
	mgr := &SyncMockManager{}
	mgr.SetFirstRequestSignaled()
	mgr.resolvedTestCount = models.StartupMockTestCaseWindow
	var told atomic.Int32
	mgr.OnDrop(func(*models.Mock) { told.Add(1) })
	now := time.Now()
	req := now.Add(-10 * time.Second)
	mgr.AddMock(staleAt(req, time.Millisecond))
	mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
	if told.Load() != 1 {
		t.Fatalf("OnDrop told %d, want 1", told.Load())
	}
	if !mgr.SendingTestCase("test-A", req.Add(-time.Millisecond), now) {
		t.Fatal("a stale drop with no recording open left a test case out")
	}
}

// TestAStaleCutoffOfAMockNoTestNeedsLeavesOutNone: an answer the client never
// read (told to no one) and a mock whose replay needs none (told) leave out
// no test case.
func TestAStaleCutoffOfAMockNoTestNeedsLeavesOutNone(t *testing.T) {
	servesByCopy(t)
	for _, tc := range []struct {
		name string
		mk   func(req time.Time) *models.Mock
		told int
	}{
		{"undelivered", func(req time.Time) *models.Mock {
			m := staleAt(req, time.Millisecond)
			m.Spec.Metadata = map[string]string{models.MetadataUndeliveredKey: models.MetadataUndelivered}
			return m
		}, 0},
		{"needed by none", func(req time.Time) *models.Mock {
			m := staleAt(req, time.Millisecond)
			m.Kind = copyKind
			m.Spec.Metadata = map[string]string{"copy": "none"}
			return m
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, _, told := staleCutoffFixture(t)
			now := time.Now()
			req := now.Add(-10 * time.Second)
			mgr.AddMock(tc.mk(req))
			mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
			if got := len(told()); got != tc.told {
				t.Fatalf("OnDrop told %d, want %d", got, tc.told)
			}
			if mgr.cutDrops.Load() != 1 {
				t.Fatalf("stale cutoff drops counted %d, want 1", mgr.cutDrops.Load())
			}
			if !mgr.SendingTestCase("test-A", req.Add(-time.Millisecond), now) {
				t.Fatal("a stale drop no test needs left a test case out")
			}
		})
	}
}

// TestAStaleCutoffOfAMockWithNoRequestTimeLeavesOutEvery: a per-test mock with
// no request time may be any test's: its drop at the cutoff leaves out every
// test case still to be sent, said at ERROR with the cutoff as why.
func TestAStaleCutoffOfAMockWithNoRequestTimeLeavesOutEvery(t *testing.T) {
	mgr, _, told := staleCutoffFixture(t)
	logs := cutLogs(mgr)
	now := time.Now()
	mgr.AddMock(&models.Mock{Kind: models.Mongo})
	mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
	if len(told()) != 1 {
		t.Fatalf("OnDrop told %d, want 1", len(told()))
	}
	if mgr.SendingTestCase("test-A", now.Add(time.Millisecond), time.Now()) {
		t.Fatal("a test case was sent after a mock with no request time was dropped")
	}
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).FilterField(zap.String("why", staleCutoffWhy)).AllUntimed()
	if len(errs) != 1 {
		t.Fatalf("the loss of a mock any test may need is not said at ERROR once with the cutoff as why: %d lines", len(errs))
	}
	if left := cutLines(logs, cutLeft); len(left) != 0 {
		t.Fatalf("a test case left out for a mock any test may need was said as the cutoff's: %+v", left)
	}
}

// TestAStaleCutoffSaysTheUnnamedSentItCannotRevoke: past the sent test cases
// it names, a drop whose request a let-go one held is said at WARN with the
// latest such end, of the drops of one resolve, even when a later drop of it
// reaches none, and with the oldest request of them, whichever came first.
func TestAStaleCutoffSaysTheUnnamedSentItCannotRevoke(t *testing.T) {
	mgr, _, _ := staleCutoffFixture(t)
	logs := cutLogs(mgr)
	now := time.Now()
	t0 := now.Add(-30 * time.Second)
	for i := range maxSentTCs + 1 {
		s := t0.Add(time.Duration(i) * time.Microsecond)
		if !mgr.SendingTestCase(fmt.Sprintf("tc-%d", i), s, s.Add(500*time.Nanosecond)) {
			t.Fatalf("tc-%d left out", i)
		}
	}
	late := staleAt(now.Add(-9*time.Second), time.Nanosecond)      // past every one sent
	early := staleAt(t0.Add(100*time.Nanosecond), time.Nanosecond) // in tc-0, let go
	later := staleAt(now.Add(-8*time.Second), time.Nanosecond)     // past every one sent
	mgr.AddMock(late)
	mgr.AddMock(early)
	mgr.AddMock(later)
	mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
	sum := cutLines(logs, cutSummary)
	if len(sum) != 1 || sum[0].Level != zapcore.WarnLevel {
		t.Fatalf("a drop reaching unnamed sent test cases is not said once at WARN: %+v", sum)
	}
	c := sum[0].ContextMap()
	if c["unnamed_sent_through"] == nil || c["revoked_sent"] != int64(0) || c["dropped"] != int64(3) {
		t.Fatalf("summary says %+v, want the unnamed sent named, none revoked, 3 dropped", c)
	}
	if c["oldest_request"] != early.Spec.ReqTimestampMock.Format(time.RFC3339Nano) {
		t.Fatalf("oldest_request %v, want the early drop's", c["oldest_request"])
	}
}

// TestAStaleCutoffsLeftOutTestCasesAreNamedAtWarnThenSampled: the first
// dropNamesMax test cases a cutoff's loss leaves out are named at WARN, then
// the sampled ones; through SendTestCaseOf too. A test case left out for
// another loss is not said as the cutoff's, and a resolve that drops nothing
// at the cutoff says nothing of it.
func TestAStaleCutoffsLeftOutTestCasesAreNamedAtWarnThenSampled(t *testing.T) {
	mgr, _, _ := staleCutoffFixture(t)
	logs := cutLogs(mgr)
	now := time.Now()
	req := now.Add(-10 * time.Second)
	mgr.AddMock(staleAt(req, time.Millisecond))
	mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
	const rate = int(sendDropSampleRate)
	for i := 1; i <= rate; i++ {
		name := fmt.Sprintf("tc-%d", i)
		if i%2 == 0 {
			if SendTestCaseOf([]*SyncMockManager{mgr}, "", name, req.Add(-time.Millisecond), now, nil) {
				t.Fatalf("%s sent through SendTestCaseOf over the drop", name)
			}
		} else if mgr.SendingTestCase(name, req.Add(-time.Millisecond), now) {
			t.Fatalf("%s sent over the drop", name)
		}
	}
	left := cutLines(logs, cutLeft)
	if len(left) != rate {
		t.Fatalf("%d left-out lines, want %d", len(left), rate)
	}
	for i, e := range left {
		n := i + 1
		want := zapcore.DebugLevel
		if n <= dropNamesMax || n == rate {
			want = zapcore.WarnLevel
		}
		if e.Level != want || e.ContextMap()["test_case"] != fmt.Sprintf("tc-%d", n) {
			t.Fatalf("left-out line %d: %v %v, want %v naming tc-%d", n, e.Level, e.ContextMap()["test_case"], want, n)
		}
	}

	// Another loss's leave-out is not the cutoff's.
	mgr.leaveOutOver(staleAt(now.Add(time.Second), time.Millisecond), mgr.revokeEpoch.Load(), "test")
	before := len(cutLines(logs, cutLeft))
	if mgr.SendingTestCase("tc-over", now.Add(time.Second), now.Add(2*time.Second)) ||
		SendTestCaseOf([]*SyncMockManager{mgr}, "", "tc-over-of", now.Add(time.Second), now.Add(2*time.Second), nil) {
		t.Fatal("a test case over a lost call's span was sent")
	}
	if got := len(cutLines(logs, cutLeft)); got != before {
		t.Fatalf("a leave-out for a lost call's span was said as the cutoff's: %d lines", got-before)
	}

	// A resolve with nothing past the cutoff says nothing of it.
	sums := len(cutLines(logs, cutSummary))
	mgr.ResolveRange(time.Now().Add(-time.Millisecond), time.Now(), "test-F", true, false)
	if got := len(cutLines(logs, cutSummary)); got != sums {
		t.Fatal("a resolve that dropped nothing at the cutoff said a drop")
	}
}

// lockCheckCore is a zap core that counts the lines written while the
// manager's mu is held: the writer holds it when TryLock fails, as the test
// runs no other goroutine on the manager.
type lockCheckCore struct {
	zapcore.LevelEnabler
	mgr            *SyncMockManager
	lines, underMu *atomic.Int64
}

func (c lockCheckCore) With([]zapcore.Field) zapcore.Core { return c }
func (c lockCheckCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) {
		return ce.AddCore(e, c)
	}
	return ce
}
func (c lockCheckCore) Write(zapcore.Entry, []zapcore.Field) error {
	c.lines.Add(1)
	if c.mgr.mu.TryLock() {
		c.mgr.mu.Unlock()
	} else {
		c.underMu.Add(1)
	}
	return nil
}
func (c lockCheckCore) Sync() error { return nil }

// TestAStaleCutoffOverALargeBufferWritesNoLineUnderMu: under a Debug logger, a
// resolve that drops a large buffer at the cutoff writes no line while it
// holds mu, and says the drops in one line, not one per mock. The pass's time
// is logged: mu is held for the walk alone.
func TestAStaleCutoffOverALargeBufferWritesNoLineUnderMu(t *testing.T) {
	mgr, _, _ := staleCutoffFixture(t)
	var lines, underMu atomic.Int64
	mgr.SetLogger(zap.New(lockCheckCore{LevelEnabler: zapcore.DebugLevel, mgr: mgr, lines: &lines, underMu: &underMu}))
	now := time.Now()
	const n = 20000
	for i := range n {
		mgr.AddMock(staleAt(now.Add(-10*time.Second+time.Duration(i)*time.Microsecond), time.Microsecond))
	}
	lines.Store(0)
	began := time.Now()
	mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
	took := time.Since(began)
	if mgr.cutDrops.Load() != n {
		t.Fatalf("dropped %d at the cutoff, want %d", mgr.cutDrops.Load(), n)
	}
	if underMu.Load() != 0 {
		t.Fatalf("%d lines written while mu was held", underMu.Load())
	}
	if lines.Load() > 2 {
		t.Fatalf("%d lines for one pass, want the cutoff's summary and the buffer's transition", lines.Load())
	}
	t.Logf("a pass dropping %d mocks at the cutoff took %v under a Debug logger", n, took)
}

// heldThenLetGo is the field the cutoff's summary carries for mocks held for
// a request in flight and let go when its window ended without them.
const heldThenLetGo = "held_then_let_go"

// TestAHeldStaleMockLetGoWhenItsWindowClosesLeavesOutTheWindowlessRequest is
// the gRPC shape beside an HTTP one: request A opens no window (its ingress
// opens none), while an HTTP request W, which did, is in flight from before
// A's call. The cutoff holds A's call for W. W then ends without taking it
// (closed with no verdict: aborted, or filtered), and the hold lets go of it.
// A, decided after, must be left out with the drop told to OnDrop at once and
// said as the cutoff's: never sent without its mock. The orphan let go beside
// it leaves out no test case whose window does not hold its request.
func TestAHeldStaleMockLetGoWhenItsWindowClosesLeavesOutTheWindowlessRequest(t *testing.T) {
	mgr, ch, told := staleCutoffFixture(t)
	logs := cutLogs(mgr)
	now := time.Now()
	reqA := now.Add(-10 * time.Second)
	w := mgr.OpenWindow(reqA.Add(-time.Millisecond), nil)
	mock := staleAt(reqA.Add(time.Millisecond), time.Millisecond)
	orphan := staleAt(reqA.Add(2*time.Second), time.Millisecond)
	mgr.AddMock(mock)
	mgr.AddMock(orphan)

	mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
	if got := told(); len(got) != 0 {
		t.Fatalf("OnDrop told %d mocks the hold keeps for an open window, want none", len(got))
	}
	w.Close()
	// At once, as the window closes: not at the manager's next reaper call.
	if got := told(); len(got) != 2 {
		t.Fatalf("OnDrop told %d mocks as the hold let go of them, want both", len(got))
	}
	if got := mgr.cutDrops.Load(); got != 2 {
		t.Fatalf("stale cutoff drops counted %d, want 2", got)
	}
	sum := cutLines(logs, cutSummary)
	if len(sum) != 1 || sum[0].ContextMap()["dropped"] != int64(2) || sum[0].ContextMap()[heldThenLetGo] == nil || sum[0].ContextMap()["recording_open"] != true {
		t.Fatalf("the hold's let-go is not said once, as held then let go, for both: %+v", sum)
	}

	endA := time.Now()
	mgr.ResolveRange(reqA, endA, "test-A", true, false)
	for _, m := range drained(ch) {
		if m == mock {
			t.Fatal("A's mock was handed on after the hold let go of it")
		}
	}
	if mgr.SendingTestCase("test-A", reqA, endA) {
		t.Fatal("A's test case was sent without its mock: the hold let go of a mock held at the cutoff and nothing left A out")
	}
	if !mgr.SendingTestCase("test-D", reqA.Add(time.Second), reqA.Add(1500*time.Millisecond)) {
		t.Fatal("D, whose window holds neither let-go mock's request, was left out")
	}
}

// TestAHeldStaleMockLetGoAsAKeptWindowEndsLeavesOutTheWindowlessRequest: the
// window the hold kept the call for is decided, kept, over a span that does
// not hold the call, so it ends without it (a reaper's own call: the resolve
// lets go of what no open window may own). The drop is told by the end of
// that resolve, and A, decided after, is left out.
func TestAHeldStaleMockLetGoAsAKeptWindowEndsLeavesOutTheWindowlessRequest(t *testing.T) {
	mgr, _, told := staleCutoffFixture(t)
	now := time.Now()
	reqA := now.Add(-10 * time.Second)
	w := mgr.OpenWindow(reqA.Add(-time.Millisecond), nil)
	mock := staleAt(reqA.Add(time.Millisecond), time.Millisecond)
	mgr.AddMock(mock)
	mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
	if !w.Keep() {
		t.Fatal("W's window was given up")
	}
	mgr.ResolveKept(w, w.Start(), reqA, "test-W", false)
	if got := told(); len(got) != 1 || got[0] != mock {
		t.Fatalf("OnDrop told %d mocks by the end of the resolve that let go of A's call, want it once", len(got))
	}
	w.Close()
	endA := time.Now()
	mgr.ResolveRange(reqA, endA, "test-A", true, false)
	if mgr.SendingTestCase("test-A", reqA, endA) {
		t.Fatal("A's test case was sent without its mock")
	}
}

// TestAHeldStaleMockLetGoUnderMemoryPressureLeavesOutTheWindowlessRequest:
// memory pressure lets go of the hold and gives up the open windows that may
// own it, but a request with no window is not among them, and its response
// may have ended before the pressure span opened, which then does not cover
// it. Its test case is left out for the call the hold let go of, told at once.
func TestAHeldStaleMockLetGoUnderMemoryPressureLeavesOutTheWindowlessRequest(t *testing.T) {
	mgr, _, told := staleCutoffFixture(t)
	now := time.Now()
	reqA := now.Add(-10 * time.Second)
	w := mgr.OpenWindow(reqA.Add(-time.Millisecond), nil)
	defer w.Close()
	mock := staleAt(reqA.Add(time.Millisecond), time.Millisecond)
	mgr.AddMock(mock)
	mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
	endA := time.Now() // A's response ends before the pressure span opens
	time.Sleep(time.Millisecond)
	mgr.SetMemoryPressure(true)
	if got := told(); len(got) != 1 || got[0] != mock {
		t.Fatalf("OnDrop told %d mocks as pressure let go of the hold, want A's call once", len(got))
	}
	mgr.SetMemoryPressure(false)
	if w.Keep() {
		t.Fatal("pressure did not give W's window up")
	}
	mgr.ResolveRange(reqA, endA, "test-A", true, false)
	if mgr.SendingTestCase("test-A", reqA, endA) {
		t.Fatal("A's test case was sent without the call pressure let go of")
	}
}

// TestAHeldDuplicatesLeftoverLetGoLeavesOutNone: a duplicate's leftover held
// for an open window is the duplicate's (its own window holds it): let go, it
// is not the cutoff's, and leaves out no test case, nor is it told.
func TestAHeldDuplicatesLeftoverLetGoLeavesOutNone(t *testing.T) {
	mgr, _, told := staleCutoffFixture(t)
	now := time.Now()
	reqA := now.Add(-10 * time.Second)
	w := mgr.OpenWindow(reqA.Add(-time.Millisecond), nil)
	mock := staleAt(reqA.Add(time.Millisecond), time.Millisecond)
	mgr.AddMock(mock)
	mgr.ResolveRange(reqA, reqA.Add(2*time.Millisecond), "test-0", false, false)
	w.Close()
	if got := told(); len(got) != 0 {
		t.Fatalf("OnDrop told %d of a duplicate's leftover let go, want none", len(got))
	}
	if got := mgr.cutDrops.Load(); got != 0 {
		t.Fatalf("stale cutoff drops counted %d for a duplicate's leftover, want 0", got)
	}
	if !mgr.SendingTestCase("test-A", reqA, time.Now()) {
		t.Fatal("a test case whose window holds a duplicate's leftover was left out")
	}
}

// TestAHeldStaleMockLetGoInAnEndedRecordingLeavesOutNoneOfTheNext: what the
// hold let go of in a recording whose output was set again before it was
// said leaves out none of the next recording's test cases, and is said with
// no recording open; it is still told.
func TestAHeldStaleMockLetGoInAnEndedRecordingLeavesOutNoneOfTheNext(t *testing.T) {
	mgr, _, told := staleCutoffFixture(t)
	logs := cutLogs(mgr)
	at := time.Now().Add(-10 * time.Second)
	mock := staleAt(at, time.Millisecond)
	mgr.mu.Lock()
	mgr.heldCut = append(mgr.heldCut, cutHeld{mock: mock, epoch: mgr.revokeEpoch.Load()})
	mgr.letGo.Store(true)
	mgr.mu.Unlock()
	mgr.SetOutputChannel(make(chan *models.Mock, 4))
	mgr.settleLetGo()
	if got := told(); len(got) != 1 {
		t.Fatalf("OnDrop told %d, want the let-go mock once", len(got))
	}
	if !mgr.SendingTestCase("test-A", at.Add(-time.Millisecond), at.Add(time.Millisecond)) {
		t.Fatal("a test case of the next recording was left out for the last one's let-go mock")
	}
	sum := cutLines(logs, cutSummary)
	if len(sum) != 1 || sum[0].ContextMap()["recording_open"] != false {
		t.Fatalf("an older recording's let-go is not said once with no recording open: %+v", sum)
	}
	mgr.settleLetGo()
	if got := len(cutLines(logs, cutSummary)); got != 1 {
		t.Fatalf("%d lines after a second settle with nothing let go, want 1", got)
	}
	if n := logs.FilterMessageSnippet("past the bound kept for them").Len(); n != 0 {
		t.Fatalf("%d lines of startup notes forgotten, when none was", n)
	}
}

// TestAHeldStaleMockLetGoWithNoRecordingOpenLeavesOutNone: with no output set
// the hold's let-go is told and said with no recording open, and leaves out
// none of the recording that opens next.
func TestAHeldStaleMockLetGoWithNoRecordingOpenLeavesOutNone(t *testing.T) {
	mgr := &SyncMockManager{}
	mgr.SetFirstRequestSignaled()
	mgr.resolvedTestCount = models.StartupMockTestCaseWindow
	var told atomic.Int64
	mgr.OnDrop(func(*models.Mock) { told.Add(1) })
	logs := cutLogs(mgr)
	now := time.Now()
	reqA := now.Add(-10 * time.Second)
	w := mgr.OpenWindow(reqA.Add(-time.Millisecond), nil)
	mgr.AddMock(staleAt(reqA.Add(time.Millisecond), time.Millisecond))
	mgr.ResolveRange(now.Add(-5*time.Millisecond), now, "test-B", true, false)
	w.Close()
	if told.Load() != 1 {
		t.Fatalf("OnDrop told %d, want the let-go mock once", told.Load())
	}
	sum := cutLines(logs, cutSummary)
	if len(sum) != 1 || sum[0].ContextMap()["recording_open"] != false {
		t.Fatalf("a let-go with no output set is not said once with no recording open: %+v", sum)
	}
	mgr.SetOutputChannel(make(chan *models.Mock, 4))
	if !mgr.SendingTestCase("test-A", reqA, time.Now()) {
		t.Fatal("the next recording's test case was left out for a let-go with no recording open")
	}
}

// TestAHeldStaleMockLetGoInTwoRecordingsIsSaidOncePerRecording: let-go mocks
// of two recordings, waiting together, are said in one line each, each with
// its own recording.
func TestAHeldStaleMockLetGoInTwoRecordingsIsSaidOncePerRecording(t *testing.T) {
	mgr, _, told := staleCutoffFixture(t)
	logs := cutLogs(mgr)
	at := time.Now().Add(-10 * time.Second)
	mgr.mu.Lock()
	old := mgr.revokeEpoch.Load()
	mgr.heldCut = append(mgr.heldCut, cutHeld{mock: staleAt(at, 0), epoch: old}, cutHeld{mock: staleAt(at, 0), epoch: old})
	mgr.mu.Unlock()
	mgr.SetOutputChannel(make(chan *models.Mock, 4))
	mgr.mu.Lock()
	mgr.heldCut = append(mgr.heldCut, cutHeld{mock: staleAt(at.Add(time.Second), 0), epoch: mgr.revokeEpoch.Load()})
	mgr.letGo.Store(true)
	mgr.mu.Unlock()
	mgr.settleLetGo()
	if got := told(); len(got) != 3 {
		t.Fatalf("OnDrop told %d, want 3", len(got))
	}
	sum := cutLines(logs, cutSummary)
	if len(sum) != 2 || sum[0].ContextMap()["dropped"] != int64(2) || sum[0].ContextMap()["recording_open"] != false ||
		sum[1].ContextMap()["dropped"] != int64(1) || sum[1].ContextMap()["recording_open"] != true {
		t.Fatalf("let-go mocks of two recordings are not said once per recording: %+v", sum)
	}
	if mgr.SendingTestCase("test-A", at.Add(time.Second-time.Millisecond), at.Add(time.Second+time.Millisecond)) {
		t.Fatal("the current recording's test case holding its let-go mock was sent")
	}
}

// TestEachLetGoIsToldOnce: what the hold lets go of is told once, at its own
// let-go, and not again at the next one.
func TestEachLetGoIsToldOnce(t *testing.T) {
	mgr, _, told := staleCutoffFixture(t)
	now := time.Now()
	for i := range 2 {
		at := now.Add(-10*time.Second + time.Duration(i)*time.Second)
		w := mgr.OpenWindow(at.Add(-time.Millisecond), nil)
		mgr.AddMock(staleAt(at, time.Millisecond))
		k := time.Now()
		mgr.ResolveRange(k.Add(-time.Millisecond), k, fmt.Sprintf("test-B%d", i), true, false)
		w.Close()
		if got := len(told()); got != i+1 {
			t.Fatalf("after let-go %d OnDrop was told %d mocks, want %d", i+1, got, i+1)
		}
	}
}
