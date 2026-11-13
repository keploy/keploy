package routes

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// redirectAgentReadyFile rewrites the package-level agentReadyFilePath
// to a sandboxed path under t.TempDir() so tests exercise the real
// os.WriteFile path without clobbering /tmp/agent.ready on the host.
// The original value is restored via t.Cleanup.
func redirectAgentReadyFile(t *testing.T) string {
	t.Helper()
	orig := agentReadyFilePath
	sandbox := filepath.Join(t.TempDir(), "agent.ready")
	agentReadyFilePath = sandbox
	t.Cleanup(func() { agentReadyFilePath = orig })
	return sandbox
}

// newTestAgent builds a routes.Agent with a test logger and nil service —
// MakeAgentReady does not touch svc, so a nil service is safe.
func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	return &Agent{
		logger: zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel)),
	}
}

func TestMakeAgentReady_RefusesWhenCANotReady(t *testing.T) {
	// Reset the package-level CAReady signal so this test observes the
	// "not ready" state regardless of previous tests in the binary.
	pTls.ResetCAReadyForTest()

	readyFile := redirectAgentReadyFile(t)
	a := newTestAgent(t)

	req := httptest.NewRequest(http.MethodPost, "/agent/agent/ready", nil)
	rr := httptest.NewRecorder()

	a.MakeAgentReady(rr, req)

	if got, want := rr.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("status: got %d, want %d", got, want)
	}
	// Confirm the readiness file was NOT touched on the 503 path —
	// that's the whole point of the gate.
	if _, err := os.Stat(readyFile); !os.IsNotExist(err) {
		t.Fatalf("agent.ready must not be created on 503; stat err=%v", err)
	}
}

func TestMakeAgentReady_SucceedsWhenCAReady(t *testing.T) {
	pTls.ResetCAReadyForTest()
	pTls.CloseCAReadyForTest()

	readyFile := redirectAgentReadyFile(t)
	a := newTestAgent(t)

	req := httptest.NewRequest(http.MethodPost, "/agent/agent/ready", nil)
	rr := httptest.NewRecorder()

	a.MakeAgentReady(rr, req)

	if got, want := rr.Code, http.StatusOK; got != want {
		t.Fatalf("status: got %d, want %d; body=%q", got, want, rr.Body.String())
	}
	if _, err := os.Stat(readyFile); err != nil {
		t.Fatalf("agent.ready must be written on 200: %v", err)
	}
}

// TestMakeAgentReady_SurfacesCASetupFailure verifies that a terminal
// SetupCA error recorded via MarkCAFailed is reflected in the 503
// body instead of the generic "not yet written" message, and that
// the readiness file is NOT created. This is the failure-mode
// Copilot raised: without this path, operators see endless 503s with
// no diagnostic when SetupCA errored out at boot.
func TestMakeAgentReady_SurfacesCASetupFailure(t *testing.T) {
	pTls.ResetCAReadyForTest()
	pTls.MarkCAFailed(errors.New("synthetic: no writable CA store"))

	readyFile := redirectAgentReadyFile(t)
	a := newTestAgent(t)

	rr := httptest.NewRecorder()
	a.MakeAgentReady(rr, httptest.NewRequest(http.MethodPost, "/agent/agent/ready", nil))

	if got, want := rr.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("status: got %d, want %d; body=%q", got, want, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "CA setup failed") {
		t.Fatalf("body should surface setup error, got %q", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "synthetic: no writable CA store") {
		t.Fatalf("body should echo the underlying error, got %q", rr.Body.String())
	}
	if _, err := os.Stat(readyFile); !os.IsNotExist(err) {
		t.Fatalf("agent.ready must not be created on CA-setup-failure 503; stat err=%v", err)
	}

	// Clear the failure so subsequent tests in the binary see a
	// clean baseline regardless of ordering.
	pTls.ResetCAReadyForTest()
}

// TestMakeAgentReady_NoReadinessFileOn503 is a tight regression guard
// for the exact failure mode the gate protects against: an app
// container observing /tmp/agent.ready and booting before the CA
// bundle is on disk. It is intentionally narrower than the 503 test
// above so a regression lights up here first.
func TestMakeAgentReady_NoReadinessFileOn503(t *testing.T) {
	pTls.ResetCAReadyForTest()

	readyFile := redirectAgentReadyFile(t)
	a := newTestAgent(t)

	// Hit the endpoint several times; every call must 503 and none
	// must ever create the readiness file.
	for i := 0; i < 5; i++ {
		rr := httptest.NewRecorder()
		a.MakeAgentReady(rr, httptest.NewRequest(http.MethodPost, "/agent/agent/ready", nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("call %d: expected 503, got %d", i, rr.Code)
		}
		if _, err := os.Stat(readyFile); !os.IsNotExist(err) {
			t.Fatalf("call %d: agent.ready leaked (stat err=%v)", i, err)
		}
	}

	// Now flip the signal and verify the next call writes the file.
	pTls.CloseCAReadyForTest()
	rr := httptest.NewRecorder()
	a.MakeAgentReady(rr, httptest.NewRequest(http.MethodPost, "/agent/agent/ready", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("after CA ready: expected 200, got %d", rr.Code)
	}
	if _, err := os.Stat(readyFile); err != nil {
		t.Fatalf("after CA ready: expected agent.ready on disk: %v", err)
	}
}

// A gRPC test case is checked against the recording's spans by its own window
// (GrpcReq to GrpcResp), as an HTTP one is by its HTTPReq and HTTPResp: one a
// pause of the memory guard overlapped, or a hole in a connection's capture,
// is left out, and one clear of both is streamed. Read as an HTTP test case's,
// its window was zero, on which the span checks make no claim, so every gRPC
// test case was streamed, without the mocks a pause or a hole had cost it.
func TestHandleIncoming_ChecksAGRPCTestCaseByItsOwnWindow(t *testing.T) {
	syncmgr.Get().SetWatermark(nil)
	tcs := make(chan *models.TestCase, 3)
	resp := openStream(t, tcs)
	grpcCase := func(name string, at time.Time) *models.TestCase {
		return &models.TestCase{Name: name, Kind: models.GRPC_EXPORT,
			GrpcReq: models.GrpcReq{Timestamp: at}, GrpcResp: models.GrpcResp{Timestamp: at.Add(time.Millisecond)}}
	}

	// A hole, in a stretch of its own: no span of another test reaches it.
	start := newStretch()
	if over, n := syncmgr.Get().WasMockOrphanedInWindow(start, start.Add(30*time.Second)); over {
		t.Fatalf("%d spans recorded before this test already cover its test cases", n)
	}
	syncmgr.Get().RecordOrphanWindow(start, start.Add(time.Millisecond))
	// A pause, on the clock the memory guard's spans are on.
	syncmgr.Get().SetMemoryPressure(true)
	during := time.Now()
	syncmgr.Get().SetMemoryPressure(false)

	tcs <- grpcCase("over-a-hole", start)
	tcs <- grpcCase("over-a-pause", during)
	tcs <- grpcCase("clean", start.Add(20*time.Second))
	close(tcs)
	if got := streamedNames(t, resp); len(got) != 1 || got[0] != "clean" {
		t.Fatalf("streamed %v, want only [clean]", got)
	}
}

// A test case the stream sent before a mock any test of the recording may
// need (a session's) is lost on its way out, whose window reaches past that
// mock's request (it began before it and ended after), is revoked by name: the span the loss opens reaches only
// the test cases checked after it. One checked after the loss that has no
// window to check is left out: it may need the mock. In a late stretch: the
// loss leaves the test cases out until the recording ends, at the real now.
func TestHandleIncoming_ALostSessionMockRevokesTheTestCasesSentBeforeIt(t *testing.T) {
	mgr := syncmgr.Get()
	mgr.SetWatermark(nil)
	mgr.SetRevokeCapable(true)
	t.Cleanup(func() { mgr.SetRevokeCapable(false) })
	start := newLateStretch()
	tcs := make(chan *models.TestCase, 1)
	names := streamOf(t, openStream(t, tcs))
	mgr.SetOutputChannel(make(chan *models.Mock)) // a consumer that takes nothing
	// A new channel ends the recording: the loss's span closes with it.
	t.Cleanup(func() { mgr.SetOutputChannel(make(chan *models.Mock, 1)) })

	tcs <- &models.TestCase{Name: "sent-before-the-loss", Kind: models.HTTP,
		HTTPReq: models.HTTPReq{Timestamp: start.Add(-time.Second)}, HTTPResp: models.HTTPResp{Timestamp: start.Add(time.Second)}}
	if name, ok := nextStreamed(t, names); !ok || name != "sent-before-the-loss" {
		t.Fatalf("streamed %q (open %v), want the test case before the loss", name, ok)
	}
	shared := &models.Mock{Kind: models.Kind("Kafka"), ConnectionID: "lost-session",
		Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}, ReqTimestampMock: start, ResTimestampMock: start.Add(time.Millisecond)}}
	shared.DeriveLifetime()
	mgr.SendConfigMock(shared)
	mgr.CloseOutChan() // given up once the consumer took nothing for the close's bound
	if !mgr.WasMockDroppedForTC("sent-before-the-loss") {
		t.Fatal("a test case sent before the session's mock was lost, whose window reaches past its request, was not revoked")
	}
	tcs <- &models.TestCase{Name: "no-window-after-the-loss", Kind: models.HTTP}
	close(tcs)
	if name, ok := nextStreamed(t, names); ok {
		t.Fatalf("streamed %q after the loss, want none", name)
	}
}

// A test case the stream sent before a test's mock no test owns (the async
// capture's, requested after the first test case the stream sent) is lost on its
// way out, whose window holds that mock's request, is revoked by name: the
// stream gives the manager its window's start as well as its end. One sent
// after the loss whose window ends before the call is sent. In a late
// stretch: the loss revokes the only test case sent, so the start-up mock is
// then one before the recording's first test case, re-read as one any test
// may need, and leaves the test cases out from its request until the
// recording ends, at the real now.
func TestHandleIncoming_ALostAsyncMockRevokesTheTestCaseSentOverItsCall(t *testing.T) {
	mgr := syncmgr.Get()
	mgr.SetWatermark(nil)
	mgr.SetRevokeCapable(true)
	t.Cleanup(func() { mgr.SetRevokeCapable(false) })
	start := newLateStretch()
	mgr.StartRecordSession()
	tcs := make(chan *models.TestCase, 1)
	names := streamOf(t, openStream(t, tcs))
	mgr.SetOutputChannel(make(chan *models.Mock)) // a consumer that takes nothing
	t.Cleanup(func() { mgr.SetOutputChannel(make(chan *models.Mock, 1)) })

	tcs <- &models.TestCase{Name: "sent-over-its-call", Kind: models.HTTP,
		HTTPReq: models.HTTPReq{Timestamp: start.Add(-time.Second)}, HTTPResp: models.HTTPResp{Timestamp: start.Add(time.Second)}}
	if name, ok := nextStreamed(t, names); !ok || name != "sent-over-its-call" {
		t.Fatalf("streamed %q (open %v), want the test case before the loss", name, ok)
	}
	lost := &models.Mock{Kind: models.Kind("Kafka"), ConnectionID: "lost-async",
		Spec: models.MockSpec{ReqTimestampMock: start.Add(-500 * time.Millisecond), ResTimestampMock: start.Add(-400 * time.Millisecond)}}
	lost.DeriveLifetime()
	if got := mgr.AddMock(lost); got != syncmgr.MockTaken {
		t.Fatalf("the async capture's mock: %q, want it queued", got)
	}
	mgr.CloseOutChan() // given up once the consumer took nothing for the close's bound
	if !mgr.WasMockDroppedForTC("sent-over-its-call") {
		t.Fatal("a test case sent before a test's mock no test owns was lost, whose window holds its request, was not revoked")
	}
	tcs <- &models.TestCase{Name: "ended-before-the-call", Kind: models.HTTP,
		HTTPReq: models.HTTPReq{Timestamp: start.Add(-1500 * time.Millisecond)}, HTTPResp: models.HTTPResp{Timestamp: start.Add(-time.Second)}}
	close(tcs)
	if name, ok := nextStreamed(t, names); !ok || name != "ended-before-the-call" {
		t.Fatalf("streamed %q (open %v), want the test case that ended before the lost call", name, ok)
	}
}

// The window a test case is checked over against the memory guard's pauses
// and the holes in a connection's capture, on which a zero bound makes no
// claim: its own when it has both times; with no response time, to now; with
// no request time to trust (none, or one after its end), from the earliest
// request of its recording; and from the earliest of all (the Unix epoch)
// when that is unknown or after its end.
func TestSpanWindow(t *testing.T) {
	req := time.Unix(1_000, 0)
	resp := req.Add(time.Second)
	from := req.Add(-time.Minute)
	epoch := time.Unix(0, 0)
	for _, c := range []struct {
		name             string
		req, resp, from  time.Time
		wantFrom, wantTo time.Time
		wantToNowOrAfter bool
	}{
		{name: "both times", req: req, resp: resp, from: from, wantFrom: req, wantTo: resp},
		{name: "a request at its end", req: resp, resp: resp, from: from, wantFrom: resp, wantTo: resp},
		{name: "no request time", resp: resp, from: from, wantFrom: from, wantTo: resp},
		{name: "a request after its end", req: resp.Add(time.Nanosecond), resp: resp, from: from, wantFrom: from, wantTo: resp},
		{name: "no request time, the recording's start unknown", resp: resp, wantFrom: epoch, wantTo: resp},
		{name: "no request time, the recording started after its end", resp: resp, from: resp.Add(time.Nanosecond), wantFrom: epoch, wantTo: resp},
		{name: "no request time, the recording started at its end", resp: resp, from: resp, wantFrom: resp, wantTo: resp},
		{name: "no response time", req: req, from: from, wantFrom: req, wantToNowOrAfter: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := time.Now()
			gotFrom, gotTo := spanWindow(c.req, c.resp, c.from)
			if !gotFrom.Equal(c.wantFrom) {
				t.Errorf("from %v, want %v", gotFrom, c.wantFrom)
			}
			if c.wantToNowOrAfter {
				if gotTo.Before(before) {
					t.Errorf("to %v, want now (at or after %v)", gotTo, before)
				}
			} else if !gotTo.Equal(c.wantTo) {
				t.Errorf("to %v, want %v", gotTo, c.wantTo)
			}
		})
	}
}

// The earliest a request of a test case an incoming stream checks can have
// come: the earliest of its recording's start, the stream's own and the
// requests the stop of an ended recording found in flight; unknown (zero) when
// no recording has started.
func TestRequestsFrom(t *testing.T) {
	rec := time.Unix(2_000, 0)
	early, late := rec.Add(-time.Second), rec.Add(time.Second)
	lefts := func(ts ...time.Time) map[int64]struct{} {
		m := map[int64]struct{}{}
		for _, at := range ts {
			m[at.UnixNano()] = struct{}{}
		}
		return m
	}
	for _, c := range []struct {
		name      string
		recording time.Time
		stream    time.Time
		leftovers map[int64]struct{}
		want      time.Time
	}{
		{name: "no recording started", stream: early, leftovers: lefts(early), want: time.Time{}},
		{name: "the stream after the recording's start", recording: rec, stream: late, want: rec},
		{name: "the stream before the recording's start", recording: rec, stream: early, want: early},
		{name: "a request in flight across an earlier stop", recording: rec, stream: late, leftovers: lefts(late, early), want: early},
		{name: "a request in flight after the start", recording: rec, stream: late, leftovers: lefts(late.Add(time.Second)), want: rec},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := requestsFrom(c.recording, c.stream, c.leftovers); !got.Equal(c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// A test case with no request time (or one after its end) is checked against
// the memory guard's pauses and the holes in a connection's capture over every
// request of its recording made before its end, as any may be its own (fail
// closed): one whose recording paused or lost a hole before its end is left
// out, and one that ended before it is streamed: a span from before its
// recording started leaves none out. A test case with no response time is
// checked to now. With a zero bound the checks made no claim, so such test
// cases were streamed without the mocks a pause or a hole cost them. On the
// real clock, each in a recording and stream of its own, which start after
// every span an earlier test recorded.
func TestHandleIncoming_ATestCaseWithNoRequestTimeIsCheckedOverItsRecording(t *testing.T) {
	mgr := syncmgr.Get()
	mgr.SetWatermark(nil)
	noRequestTime := func(name string, end time.Time) *models.TestCase {
		return &models.TestCase{Name: name, Kind: models.HTTP, HTTPResp: models.HTTPResp{Timestamp: end}}
	}
	for _, c := range []struct {
		name string
		span func() // records a span at the real now
	}{
		{name: "a pause", span: func() {
			mgr.SetMemoryPressure(true)
			mgr.SetMemoryPressure(false)
		}},
		{name: "a hole", span: func() {
			at := time.Now()
			mgr.RecordOrphanWindow(at, at.Add(time.Millisecond))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// One before the recording starts: no request of its is in it.
			c.span()
			time.Sleep(2 * time.Millisecond)
			mgr.StartRecordSession()
			// No request an earlier test's stop found in flight is this
			// stream's: those reach back to that test's spans.
			mgr.TakeLeftovers()
			from := mgr.RecordingFrom()
			tcs := make(chan *models.TestCase, 5)
			resp := openStream(t, tcs)
			clean := time.Now()
			if over, n := mgr.WasPressureActiveInWindow(from, clean); over {
				t.Fatalf("%d pauses an earlier test left open cover this recording", n)
			}
			if over, n := mgr.WasMockOrphanedInWindow(from, clean); over {
				t.Fatalf("%d holes an earlier test left open cover this recording", n)
			}
			time.Sleep(2 * time.Millisecond)
			beforeSpan := time.Now()
			c.span()
			time.Sleep(2 * time.Millisecond)
			after := time.Now().Add(time.Millisecond)

			tcs <- noRequestTime("ended-before-it", clean)
			tcs <- noRequestTime("over-it", after)
			tcs <- &models.TestCase{Name: "request-after-its-end", Kind: models.HTTP,
				HTTPReq: models.HTTPReq{Timestamp: after.Add(time.Second)}, HTTPResp: models.HTTPResp{Timestamp: after}}
			tcs <- &models.TestCase{Name: "no-response-time", Kind: models.HTTP,
				HTTPReq: models.HTTPReq{Timestamp: beforeSpan}}
			tcs <- &models.TestCase{Name: "both-times-before-it", Kind: models.HTTP,
				HTTPReq: models.HTTPReq{Timestamp: from}, HTTPResp: models.HTTPResp{Timestamp: clean}}
			close(tcs)
			got := streamedNames(t, resp)
			if strings.Join(got, ",") != "ended-before-it,both-times-before-it" {
				t.Fatalf("streamed %v, want only [ended-before-it both-times-before-it]", got)
			}
		})
	}
}
