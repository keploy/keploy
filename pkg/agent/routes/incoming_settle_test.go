package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// incomingSvc hands out a test-case channel the test feeds.
type incomingSvc struct {
	agent.Service // nil: any other call panics loudly
	tc            chan *models.TestCase
}

func (s incomingSvc) StartIncomingProxy(context.Context, models.IncomingOptions) (chan *models.TestCase, error) {
	return s.tc, nil
}

// switchWatermark settles every test case once flipped.
type switchWatermark struct{ settled atomic.Bool }

func (w *switchWatermark) Settled(string, string, time.Time, time.Time) bool { return w.settled.Load() }

// The tests here record their spans on the process-global manager
// (syncmgr.Get()), and nothing clears them, so every test sees the spans of
// the tests that ran before it, in this run and in an earlier one (-count,
// -shuffle). Past the cap, the manager joins the spans that end before the
// test cases it still holds into one (TestHandleIncoming_SpansOfHeldTestCasesStayExactPastTheCap
// does that), which covers all the time between them: a test case checked
// there later is left out. A join covers only time between spans already
// recorded, never time before the first of them. So a test that records spans
// takes a stretch of time of its own (newStretch) and keeps its spans and test
// cases in it. Each stretch comes before every earlier one, and so before
// every span recorded before it was taken. The past-the-cap test keeps its
// spans after every stretch, from stretchesEnd on: the test cases it holds
// must start after every one another test held (Spans.CheckedBefore only
// moves forward).
var stretchesTaken atomic.Int64

// stretchLen is how long a stretch is. A test keeps its spans and test cases
// in its first half.
const stretchLen = time.Minute

// stretchesEnd is where the first stretch ends.
var stretchesEnd = time.Unix(90_000, 0)

// newStretch returns the start of a stretch no other test or run has taken,
// before every span recorded so far.
func newStretch() time.Time {
	return stretchesEnd.Add(-time.Duration(stretchesTaken.Add(1)) * stretchLen)
}

// streamWait bounds how long a test waits for the next test case of a
// HandleIncoming stream, or its end: a regression that leaves a test case out
// fails the test rather than hanging the package until its timeout.
const streamWait = 30 * time.Second

// streamOf reads a HandleIncoming stream: it hands on the name of each test
// case the stream carries, in order, and is closed at the stream's end.
func streamOf(t *testing.T, resp *http.Response) <-chan string {
	t.Helper()
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	names := make(chan string, 16)
	go func() {
		defer close(names)
		for {
			p, err := mr.NextPart()
			if err != nil {
				return
			}
			if strings.Contains(p.Header.Get("Content-Disposition"), `name="metadata"`) {
				var tc models.TestCase
				b, _ := io.ReadAll(p)
				if json.Unmarshal(b, &tc) == nil {
					names <- tc.Name
				}
			}
		}
	}()
	return names
}

// nextStreamed is the next test case a stream (streamOf) carries, or false at
// its end. It fails the test if neither comes within streamWait.
func nextStreamed(t *testing.T, names <-chan string) (string, bool) {
	t.Helper()
	select {
	case name, ok := <-names:
		return name, ok
	case <-time.After(streamWait):
		t.Fatalf("the stream carried no test case, nor ended, within %v", streamWait)
		return "", false
	}
}

// streamedNames reads the test cases a HandleIncoming stream carried, to its
// end.
func streamedNames(t *testing.T, resp *http.Response) []string {
	t.Helper()
	names := streamOf(t, resp)
	var got []string
	for {
		name, ok := nextStreamed(t, names)
		if !ok {
			return got
		}
		got = append(got, name)
	}
}

// The stream says where the test cases it still holds start, and the
// recording's spans keep those exact past the cap: the spans before them are
// joined first. Here the newest gaps are the smallest; joined by gap size
// alone, a held test case in one was left out, though no span covers it.
func TestHandleIncoming_SpansOfHeldTestCasesStayExactPastTheCap(t *testing.T) {
	w := &switchWatermark{}
	syncmgr.Get().SetWatermark(w)
	t.Cleanup(func() { syncmgr.Get().SetWatermark(nil) })

	tcs := make(chan *models.TestCase, 4)
	a := &Agent{logger: zap.NewNop(), svc: incomingSvc{tc: tcs}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleIncoming))
	defer srv.Close()
	body, _ := json.Marshal(models.IncomingReq{})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// After every stretch (newStretch), and so after every other test's
	// spans and test cases: the watermark only moves forward, and a test case
	// that comes after it moved past its start is checked against what was
	// joined before it.
	base := stretchesEnd.Add(time.Hour)
	// 6,000 spans 10 ms apart, then, from `held` on, 4,300 spans 1 ms
	// apart with 0.5 ms gaps: past the cap (8,192).
	var at []time.Time
	for i := 0; i < 6000; i++ {
		at = append(at, base.Add(time.Duration(i)*10*time.Millisecond))
	}
	held := at[len(at)-1].Add(10 * time.Millisecond)
	for i := 0; i < 4300; i++ {
		at = append(at, held.Add(time.Duration(i)*1500*time.Microsecond))
	}
	// A test case in a gap among the newest spans, held for its verdict.
	gap := held.Add(100*1500*time.Microsecond + 1100*time.Microsecond)
	tcs <- &models.TestCase{Name: "in-a-gap", HTTPReq: models.HTTPReq{Timestamp: gap}, HTTPResp: models.HTTPResp{Timestamp: gap.Add(200 * time.Microsecond)}}
	over := held.Add(200*1500*time.Microsecond + 500*time.Microsecond)
	tcs <- &models.TestCase{Name: "over-a-span", HTTPReq: models.HTTPReq{Timestamp: over}, HTTPResp: models.HTTPResp{Timestamp: over.Add(200 * time.Microsecond)}}
	time.Sleep(100 * time.Millisecond) // held, and said so
	for _, s := range at {
		syncmgr.Get().RecordOrphanWindow(s, s.Add(time.Millisecond))
	}
	w.settled.Store(true)
	close(tcs)

	got := streamedNames(t, resp)
	if len(got) != 1 || got[0] != "in-a-gap" {
		t.Fatalf("streamed %v, want only [in-a-gap]: past the cap, a held test case's spans were joined", got)
	}
}

// A capture that can tell when a test case's verdict is final has it held
// until then, and checked then: a connection that stopped being recorded over
// the test case can become known after it completed (its parser failed behind
// the traffic). Checked as it came, it was streamed without its mocks.
func TestHandleIncoming_HoldsATestCaseUntilItsVerdictIsFinal(t *testing.T) {
	w := &switchWatermark{}
	syncmgr.Get().SetWatermark(w)
	t.Cleanup(func() { syncmgr.Get().SetWatermark(nil) })

	tcs := make(chan *models.TestCase, 4)
	a := &Agent{logger: zap.NewNop(), svc: incomingSvc{tc: tcs}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleIncoming))
	defer srv.Close()
	body, _ := json.Marshal(models.IncomingReq{})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// In a stretch of its own, so no other test's spans reach it.
	start := newStretch().Add(time.Second)
	tcs <- &models.TestCase{Name: "over-a-stop-found-late", HTTPReq: models.HTTPReq{Timestamp: start}, HTTPResp: models.HTTPResp{Timestamp: start.Add(time.Millisecond)}}
	later := start.Add(20 * time.Second)
	tcs <- &models.TestCase{Name: "clean", HTTPReq: models.HTTPReq{Timestamp: later}, HTTPResp: models.HTTPResp{Timestamp: later.Add(time.Millisecond)}}
	time.Sleep(100 * time.Millisecond) // held, not streamed

	// The parser fails, behind the traffic: it stopped where it had got to,
	// before the first test case ended.
	syncmgr.Get().RecordOrphanWindow(start.Add(-time.Second), start.Add(time.Second))
	w.settled.Store(true)
	close(tcs)

	got := streamedNames(t, resp)
	if len(got) != 1 || got[0] != "clean" {
		t.Fatalf("streamed %v, want only [clean]: a test case over a stop found after it completed was saved without its mocks", got)
	}
}

// backlogWatermark says a backlog is pending until flipped.
type backlogWatermark struct {
	switchWatermark
	pending atomic.Bool
}

func (w *backlogWatermark) PendingBefore(time.Time) bool { return w.pending.Load() }

// A recording's stop asks the agent whether it may still hand over something
// captured before it; an agent whose capture cannot tell says so (501), and
// the stop goes by quiet alone.
func TestHandlePending(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	ask := func() (int, string) {
		rr := httptest.NewRecorder()
		a.HandlePending(rr, httptest.NewRequest(http.MethodGet, "/agent/record/pending?before=1000", nil))
		return rr.Code, strings.TrimSpace(rr.Body.String())
	}
	syncmgr.Get().SetWatermark(nil)
	if code, _ := ask(); code != http.StatusNotImplemented {
		t.Fatalf("without a watermark: %d, want 501", code)
	}
	w := &backlogWatermark{}
	syncmgr.Get().SetWatermark(w)
	t.Cleanup(func() { syncmgr.Get().SetWatermark(nil); syncmgr.Get().NoteHeld(time.Time{}) })
	w.pending.Store(true)
	if code, body := ask(); code != http.StatusOK || body != `{"pending":true}` {
		t.Fatalf("capture behind: %d %s", code, body)
	}
	w.pending.Store(false)
	if code, body := ask(); code != http.StatusOK || body != `{"pending":false}` {
		t.Fatalf("capture through: %d %s", code, body)
	}
	// A test case held for its verdict that ended before the time asked
	// about is pending too.
	syncmgr.Get().NoteHeld(time.Unix(0, 999))
	if _, body := ask(); body != `{"pending":true}` {
		t.Fatalf("a held test case from before: %s", body)
	}
	syncmgr.Get().NoteHeld(time.Unix(0, 1001))
	if _, body := ask(); body != `{"pending":false}` {
		t.Fatalf("a held test case from after: %s", body)
	}
}

// leaveOutIncomplete has a session of the default manager, as OSS's are
// (Session.Mgr unset), leave the mock of the exchange [at, at+1ms] out for the
// incomplete-mock flag.
func leaveOutIncomplete(t *testing.T, at time.Time) {
	t.Helper()
	s := &supervisor.Session{Mocks: make(chan *models.Mock, 1), Ctx: context.Background()}
	m := &models.Mock{Name: "left-out"}
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = at, at.Add(time.Millisecond)
	s.MarkMockIncomplete("per_conn_cap")
	if err := s.EmitMock(m); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	if len(s.Mocks) != 0 {
		t.Fatal("the mock marked incomplete was emitted")
	}
}

// The recording's summary counts the mocks a session left out because their
// capture was marked incomplete: their WARN is rate-limited, so the summary is
// the one place each is counted. It counts the ones reported while the session
// was open: the manager's count is of its life, and a mock left out before the
// session began was counted in the summary of the session it was reported in.
func TestHandleIncoming_TheSummaryCountsMocksLeftOut(t *testing.T) {
	at := newStretch()        // its spans must not reach another test's test cases
	leaveOutIncomplete(t, at) // before the session: not this session's
	summary, _ := incomingSummary(t, func() {
		for i := 1; i <= 3; i++ {
			leaveOutIncomplete(t, at.Add(time.Duration(i)*time.Second))
		}
	})
	if got := summary["mocks_left_out"]; got != int64(3) {
		t.Fatalf("the summary counts %v mocks left out, want this session's 3", got)
	}
}

// A session that loses a mock to the incomplete-mock flag and a test case to a
// request left out in flight reports each in its summary under its own count,
// once; the next session, which loses nothing, reports neither again.
func TestHandleIncoming_EachLossIsInTheSummaryOfItsSessionOnce(t *testing.T) {
	at := newStretch() // its spans must not reach another test's test cases
	summary, warns := incomingSummary(t, func() {
		leaveOutIncomplete(t, at)
		leaveOutOnGlobalManager(t)
	})
	if summary["mocks_left_out"] != int64(1) || summary["tcs_left_out_in_flight"] != uint64(1) {
		t.Fatalf("summary mocks_left_out = %v, tcs_left_out_in_flight = %v, want 1 each",
			summary["mocks_left_out"], summary["tcs_left_out_in_flight"])
	}
	if len(warns) != 1 || warns[0].ContextMap()["test_cases"] != uint64(1) {
		t.Fatalf("the test case left out in flight is not warned of once in the session's summary: %v", warns)
	}

	summary, warns = incomingSummary(t, func() {})
	if summary["mocks_left_out"] != int64(0) || summary["tcs_left_out_in_flight"] != uint64(0) {
		t.Fatalf("the next session's summary mocks_left_out = %v, tcs_left_out_in_flight = %v, want 0 each: it counts the earlier session's losses again",
			summary["mocks_left_out"], summary["tcs_left_out_in_flight"])
	}
	if len(warns) != 0 {
		t.Fatalf("a session that left nothing out warned of test cases left out: %v", warns[0].ContextMap())
	}
}

// Proxy mode has no watermark, so each test case is checked as it is streamed.
// A mock left out for the incomplete-mock flag is reported when the parser
// reaches it, which can be after the test cases recorded over its exchange
// were streamed: the parser runs behind the traffic, by a full capture buffer
// when the reason is per_conn_cap. The test case streamed before the report is
// saved without the mock (its replay fails with no_mocks); one streamed after
// it is left out. The left-out WARN says exactly that, and no more
// (supervisor's TestTheLeftOutWarnPromisesOnlyWhatHolds): if proxy mode gets a
// watermark, both this test and that WARN change.
func TestHandleIncoming_AMockLeftOutReachesOnlyTestCasesNotYetStreamed(t *testing.T) {
	syncmgr.Get().SetWatermark(nil)
	tcs := make(chan *models.TestCase, 2) // a send cannot block on a handler that stopped reading
	a := &Agent{logger: zap.NewNop(), svc: incomingSvc{tc: tcs}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleIncoming))
	defer srv.Close()
	body, _ := json.Marshal(models.IncomingReq{})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	names := streamOf(t, resp)

	// In a stretch of its own: no span of another test, or of an earlier run
	// of this one, reaches it.
	start := newStretch()
	end := start.Add(10 * time.Millisecond)
	if over, n := syncmgr.Get().WasMockOrphanedInWindow(start, end); over {
		t.Fatalf("%d spans recorded before this test already cover its test cases", n)
	}
	over := func(name string) *models.TestCase {
		return &models.TestCase{Name: name, HTTPReq: models.HTTPReq{Timestamp: start},
			HTTPResp: models.HTTPResp{Timestamp: end}}
	}
	tcs <- over("streamed-before-the-report")
	if got, _ := nextStreamed(t, names); got != "streamed-before-the-report" {
		t.Fatalf("streamed %q first, want the test case sent before the report", got)
	}

	// The parser reaches the exchange, inside that test case's window, and
	// the flag the relay set for a chunk it dropped leaves its mock out.
	s := &supervisor.Session{Mocks: make(chan *models.Mock, 1), Ctx: context.Background()}
	m := &models.Mock{Name: "left-out"}
	m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = start.Add(2*time.Millisecond), start.Add(5*time.Millisecond)
	s.MarkMockIncomplete("per_conn_cap")
	if err := s.EmitMock(m); err != nil {
		t.Fatalf("EmitMock: %v", err)
	}
	if len(s.Mocks) != 0 {
		t.Fatal("the mock marked incomplete was emitted")
	}

	tcs <- over("streamed-after-the-report")
	close(tcs)
	if got, more := nextStreamed(t, names); more {
		t.Fatalf("streamed %q, want nothing more: a test case over the exchange left out was streamed after it was reported", got)
	}
}

// leaveOutOnGlobalManager has the process's mock manager give up a request in
// flight (the hold's budget) and then decides it kept: one test case left out,
// told. It returns the request's window.
func leaveOutOnGlobalManager(t *testing.T) *syncmgr.Window {
	t.Helper()
	mgr := syncmgr.Get()
	mgr.SetFirstRequestSignaled()
	old := time.Now().Add(-time.Hour)
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		mgr.ResolveRange(old, old, "", true, false) // past the startup window: no mock is rescued as boot traffic
	}
	start := time.Now().Add(-time.Minute)
	w := mgr.OpenWindow(start, nil)
	// Seventeen mocks of a sixteenth of the hold's budget each (the body is
	// shared; each mock is sized with it).
	body := strings.Repeat("b", int(syncmgr.MaxHeldBytes/16))
	for i := 0; i <= 16; i++ {
		mgr.AddMock(&models.Mock{Kind: models.HTTP,
			Spec:         models.MockSpec{ReqTimestampMock: start.Add(time.Duration(i) * time.Microsecond), HTTPResp: &models.HTTPResp{Body: body}},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}})
	}
	n := time.Now()
	mgr.ResolveRange(n, n, "", true, false) // stale: held for it, past the budget
	if w.Keep() {
		t.Fatal("fixture: the window was not given up")
	}
	w.Close()
	return w
}

// pauseCore holds up the logger's caller at the entry whose message is msg
// until release is closed: a test's way to hold a handler at a known point.
type pauseCore struct {
	zapcore.Core
	msg     string
	release <-chan struct{}
}

func (c pauseCore) With(f []zapcore.Field) zapcore.Core {
	return pauseCore{Core: c.Core.With(f), msg: c.msg, release: c.release}
}

func (c pauseCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if e.Message == c.msg {
		<-c.release
	}
	return c.Core.Check(e, ce)
}

// incomingSummary runs one /incoming session, calls during while it is open,
// and returns the session's recording-complete summary and its warnings about
// test cases left out.
//
// during runs once the client has the stream's headers, with the handler held
// right after it flushed them (at its "headers flushed" line) until during has
// returned: the earliest the client can act on an open session, and the
// latest the handler can have reached. What during does belongs to the
// session wherever the handler is in between.
func incomingSummary(t *testing.T, during func()) (summary map[string]interface{}, leftOutWarns []observer.LoggedEntry) {
	t.Helper()
	core, logs := observer.New(zap.DebugLevel)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	held := pauseCore{Core: core, msg: "Incoming stream connection established and headers flushed", release: release}
	tcs := make(chan *models.TestCase)
	a := &Agent{logger: zap.New(held), svc: incomingSvc{tc: tcs}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleIncoming))
	defer srv.Close()
	body, _ := json.Marshal(models.IncomingReq{})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	during()
	close(release)
	released = true
	close(tcs)
	_ = streamedNames(t, resp)
	deadline := time.Now().Add(5 * time.Second)
	for logs.FilterMessage("agent: recording complete").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no recording-complete summary")
		}
		time.Sleep(time.Millisecond)
	}
	return logs.FilterMessage("agent: recording complete").All()[0].ContextMap(),
		logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet("left out of the recording").All()
}

// A kept request the mock manager left out of the recording — it stayed in
// flight while the mocks held for the requests in flight passed their budget —
// is never captured, so no other count sees it. The session's summary says how
// many, at warning level: the manager's own warning is sampled.
func TestHandleIncoming_SaysHowManyTestCasesWereLeftOutInFlight(t *testing.T) {
	leaveOutOnGlobalManager(t) // an earlier session's loss: not this session's

	// A request in flight on the recording's manager, given up by the hold's
	// budget, and then kept: this session's one loss.
	summary, warns := incomingSummary(t, func() { leaveOutOnGlobalManager(t) })
	if got := summary["tcs_left_out_in_flight"]; got != uint64(1) {
		t.Fatalf("summary tcs_left_out_in_flight = %v, want 1", got)
	}
	if len(warns) != 1 || warns[0].ContextMap()["test_cases"] != uint64(1) {
		t.Fatalf("the left-out test case is not warned of in the session's summary: %v", warns)
	}
}

// A loss told before the session began can be taken back during it (a later
// request recorded in the left-out one's place, syncMock.Window.Replaced): the
// manager's count then stands below what it was when the session began. The
// session lost nothing, and its summary says so: none, not the count wrapped
// around.
func TestHandleIncoming_ALossTakenBackDuringTheSessionIsNotItsLoss(t *testing.T) {
	earlier := leaveOutOnGlobalManager(t)
	summary, warns := incomingSummary(t, earlier.Replaced)
	if got := summary["tcs_left_out_in_flight"]; got != uint64(0) {
		t.Fatalf("summary tcs_left_out_in_flight = %v, want 0", got)
	}
	if len(warns) != 0 {
		t.Fatalf("a session that left nothing out warned of test cases left out: %v", warns[0].ContextMap())
	}
}

// The mock manager and a deduper that runs for the agent's life (--static-dedup
// at boot, a sidecar agent serving one stream per recording) outlive a
// session: a loss told in an earlier session can be taken back in this one,
// next to a loss of this session's own. The session lost one test case, and
// its summary, the only report of that second loss (the manager's warning is
// sampled), says so: the earlier loss's take-back does not cancel it.
func TestHandleIncoming_AnEarlierLossTakenBackDoesNotHideThisSessionsLoss(t *testing.T) {
	earlier := leaveOutOnGlobalManager(t) // told in an earlier session
	summary, warns := incomingSummary(t, func() {
		earlier.Replaced()         // its kind recorded in this session
		leaveOutOnGlobalManager(t) // this session's own loss
	})
	if got := summary["tcs_left_out_in_flight"]; got != uint64(1) {
		t.Fatalf("summary tcs_left_out_in_flight = %v, want 1: this session's loss is hidden by an earlier one's take-back", got)
	}
	if len(warns) != 1 || warns[0].ContextMap()["test_cases"] != uint64(1) {
		t.Fatalf("this session's loss is not warned of in its summary: %v", warns)
	}
}

// A session's own loss taken back during it (its kind recorded later in the
// session) is no loss of the session's, and the session's tally of losses
// goes with it: the handler closes it on its way out.
func TestHandleIncoming_ItsOwnLossTakenBackIsNoLoss(t *testing.T) {
	summary, warns := incomingSummary(t, func() {
		leaveOutOnGlobalManager(t).Replaced()
	})
	if got := summary["tcs_left_out_in_flight"]; got != uint64(0) {
		t.Fatalf("summary tcs_left_out_in_flight = %v, want 0", got)
	}
	if len(warns) != 0 {
		t.Fatalf("a session whose loss was taken back warned of test cases left out: %v", warns[0].ContextMap())
	}
	if n := syncmgr.Get().OpenLossTallies(); n != 0 {
		t.Fatalf("%d loss tallies still open after the session ended", n)
	}
}
