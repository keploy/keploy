package routes

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
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
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
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

// streamIncoming streams tcs through HandleIncoming to a client that asked
// with opts, and returns what the client got: each test case, and the
// marker's summary of the session.
func streamIncoming(t *testing.T, opts models.IncomingOptions, tcs ...*models.TestCase) ([]models.TestCase, models.StreamEndSummary) {
	t.Helper()
	syncmgr.Get().SetWatermark(nil) // no hold: test cases go at once
	svc := streamsSvc{tc: make(chan *models.TestCase, len(tcs)), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	tok := rand.Text()
	resp, done := openHeldStream(t, a.HandleIncoming, models.IncomingReq{IncomingOptions: opts}, tok)
	defer done()
	for _, tc := range tcs {
		svc.tc <- tc
	}
	endRecordingStreams(tok)
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	var got []models.TestCase
	for {
		part, err := mr.NextPart()
		if err != nil {
			t.Fatalf("the stream ended without its marker, after %d test cases: %v", len(got), err)
		}
		switch part.FormName() {
		case models.StreamEndPart:
			var sum models.StreamEndSummary
			if err := json.NewDecoder(part).Decode(&sum); err != nil {
				t.Fatal(err)
			}
			return got, sum
		case "metadata":
			var tc models.TestCase
			if err := json.NewDecoder(part).Decode(&tc); err != nil {
				t.Fatal(err)
			}
			got = append(got, tc)
		}
	}
}

// Under the proven switch a test case whose recording lost a call (an orphan
// span over its window: an exchange a connection carried that keploy could
// not record; memory pressure; a mock of its own dropped) is never left out
// unsaid: to a client that charges the loss (keploy record's capture watch,
// IncomingOptions.ChargesLosses) it is streamed with it, the session's
// summary counts it as kept with its loss, and tcs_suppressed_total stays 0.
// A client that does not charge it (one that predates it), or the switch off,
// gets it left out, as before: the byte-identical default.
//
// Not parallel: the spans are the package-global manager's, as
// HandleIncoming's are, and it sets the switch.
func TestATestCaseOverALossIsStreamedWithItToAClientThatChargesIt(t *testing.T) {
	// Far from now, so no other test's test case overlaps the span.
	base := time.Date(2002, 3, 4, 5, 6, 7, 0, time.UTC).Add(time.Duration(time.Now().UnixNano()%1e9) * time.Microsecond)
	// Recorded under the switch: with it off, why is not kept.
	t.Setenv(provenswitch.Env, "1")
	syncmgr.Get().RecordOrphanWindowWhy(base, base.Add(time.Millisecond), "MySQL: mysql exchange not recorded")
	tc := func(name string, from time.Duration) *models.TestCase {
		t := &models.TestCase{Name: name}
		t.HTTPReq.Timestamp = base.Add(from)
		t.HTTPResp.Timestamp = base.Add(from + time.Millisecond)
		return t
	}
	for _, c := range []struct {
		proven, charges bool
		kept            []string
		suppressed      int
		keptWithLoss    int
	}{
		{proven: true, charges: true, kept: []string{"over", "clear"}, keptWithLoss: 1},
		{proven: true, charges: false, kept: []string{"clear"}, suppressed: 1},
		{proven: false, charges: true, kept: []string{"clear"}, suppressed: 1},
		{proven: false, charges: false, kept: []string{"clear"}, suppressed: 1},
	} {
		t.Run(fmt.Sprintf("proven=%v charges=%v", c.proven, c.charges), func(t *testing.T) {
			if c.proven {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			got, sum := streamIncoming(t, models.IncomingOptions{ChargesLosses: c.charges}, tc("over", 0), tc("clear", time.Hour))
			var names []string
			for _, g := range got {
				names = append(names, g.Name)
				switch {
				case g.Name == "over":
					want := models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: "MySQL: mysql exchange not recorded"}}
					if fmt.Sprint(g.CaptureLost) != fmt.Sprint(want) {
						t.Fatalf("the test case over the span carries %+v, want %+v", g.CaptureLost, want)
					}
				case len(g.CaptureLost) != 0:
					t.Fatalf("%s, over no span, carries %+v", g.Name, g.CaptureLost)
				}
			}
			if strings.Join(names, ",") != strings.Join(c.kept, ",") {
				t.Fatalf("streamed %v, want %v", names, c.kept)
			}
			if sum.TestCasesSuppressed != c.suppressed || sum.KeptWithLoss != c.keptWithLoss || sum.TestCasesSent != len(c.kept) {
				t.Fatalf("the summary says %+v, want %d sent, %d suppressed, %d kept with their loss", sum, len(c.kept), c.suppressed, c.keptWithLoss)
			}
		})
	}
}

// Under the proven switch a dropped mock that named no owner keeps a span of
// its own (syncMock SpanDroppedMock), not an orphan span: to a client that
// charges what a test case lost, a test case over it is streamed with it (a
// mock let go); to one that does not (an older keploy, another client of the
// agent's), it is left out and counted, as over an orphan span, never streamed
// without the mock unsaid. With the switch off no such span is kept, and the
// test case is streamed as before.
func TestATestCaseOverAnOwnerlessDroppedMockIsChargedOrLeftOutNeverStreamedUnsaid(t *testing.T) {
	// Far from now, so no other test's test case overlaps the span.
	base := time.Date(2008, 3, 4, 5, 6, 7, 0, time.UTC).Add(time.Duration(time.Now().UnixNano()%1e9) * time.Microsecond)
	tc := func(name string, from time.Duration) *models.TestCase {
		t := &models.TestCase{Name: name}
		t.HTTPReq.Timestamp = base.Add(from)
		t.HTTPResp.Timestamp = base.Add(from + time.Millisecond)
		return t
	}
	dropped := &models.Mock{Kind: models.MySQL}
	dropped.Spec.ReqTimestampMock, dropped.Spec.ResTimestampMock = base, base.Add(time.Millisecond)
	for _, c := range []struct {
		proven, charges bool
		kept            []string
		suppressed      int
		keptWithLoss    int
	}{
		{proven: true, charges: true, kept: []string{"over", "clear"}, keptWithLoss: 1},
		{proven: true, charges: false, kept: []string{"clear"}, suppressed: 1},
		{proven: false, charges: true, kept: []string{"over", "clear"}},
		{proven: false, charges: false, kept: []string{"over", "clear"}},
	} {
		t.Run(fmt.Sprintf("proven=%v charges=%v", c.proven, c.charges), func(t *testing.T) {
			if c.proven {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			// Its own stretch of time per case: the manager is the agent's,
			// and keeps what the case before recorded.
			base = base.Add(10 * time.Hour)
			dropped.Spec.ReqTimestampMock, dropped.Spec.ResTimestampMock = base, base.Add(time.Millisecond)
			syncmgr.Get().SpanDroppedMock(dropped)
			if left, _ := syncmgr.Get().WasDroppedMockInWindow(base, base); left != c.proven {
				t.Fatalf("a dropped mock's span over its exchange: %v, want %v", left, c.proven)
			}
			got, sum := streamIncoming(t, models.IncomingOptions{ChargesLosses: c.charges}, tc("over", 0), tc("clear", time.Hour))
			var names []string
			for _, g := range got {
				names = append(names, g.Name)
				switch {
				case g.Name == "over" && c.keptWithLoss > 0:
					want := models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: 1, What: "MySQL: " + syncmgr.MockDroppedWhileItRan}}
					if fmt.Sprint(g.CaptureLost) != fmt.Sprint(want) {
						t.Fatalf("the test case over the span carries %+v, want %+v", g.CaptureLost, want)
					}
				case len(g.CaptureLost) != 0:
					t.Fatalf("%s carries %+v, want nothing", g.Name, g.CaptureLost)
				}
			}
			if strings.Join(names, ",") != strings.Join(c.kept, ",") {
				t.Fatalf("streamed %v, want %v", names, c.kept)
			}
			if sum.TestCasesSuppressed != c.suppressed || sum.KeptWithLoss != c.keptWithLoss || sum.TestCasesSent != len(c.kept) {
				t.Fatalf("the summary says %+v, want %d sent, %d suppressed, %d kept with their loss", sum, len(c.kept), c.suppressed, c.keptWithLoss)
			}
		})
	}
}

// Under the proven switch, to a client that charges the loss, a test case
// streamed with nothing lost that a span recorded after it went overlaps (a
// parser reports what it could not record behind the traffic) is named at the
// stream's end, in its marker, with what it lost: by the window it was
// streamed with, and by its name when it has one. The agent streams most test
// cases unnamed (keploy record names each as it saves it), so an unnamed one
// is named by its window alone, and two unnamed ones are told apart by it.
// One streamed with its loss is not named again, nor one the late span does
// not overlap. Without the switch, or to a client that does not charge it,
// the marker names none.
//
// Not parallel: the spans are the package-global manager's, and it sets the
// switch.
func TestATestCaseALateSpanOverlapsIsNamedAtTheStreamsEnd(t *testing.T) {
	base := time.Date(2003, 4, 5, 6, 7, 8, 0, time.UTC).Add(time.Duration(time.Now().UnixNano()%1e9) * time.Microsecond)
	tc := func(name string, from time.Duration) *models.TestCase {
		t := &models.TestCase{Name: name}
		t.HTTPReq.Timestamp = base.Add(from)
		t.HTTPResp.Timestamp = base.Add(from + time.Millisecond)
		return t
	}
	t.Setenv(provenswitch.Env, "1")
	syncmgr.Get().RecordOrphanWindowWhy(base, base.Add(time.Millisecond), "MySQL: early")
	for _, c := range []struct {
		proven, charges bool
		late            bool
	}{
		{proven: true, charges: true, late: true},
		{proven: true, charges: false},
		{proven: false, charges: true},
	} {
		t.Run(fmt.Sprintf("proven=%v charges=%v", c.proven, c.charges), func(t *testing.T) {
			if !c.proven {
				t.Setenv(provenswitch.Env, "")
			}
			// Each run its own late span, recorded once its test case went.
			at := time.Duration(time.Now().UnixNano()%1e6) * time.Hour / 1e6
			late := base.Add(time.Hour + at)
			syncmgr.Get().SetWatermark(nil)
			svc := streamsSvc{tc: make(chan *models.TestCase, 4), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
			a := &Agent{logger: zap.NewNop(), svc: svc}
			tok := rand.Text()
			resp, done := openHeldStream(t, a.HandleIncoming, models.IncomingReq{IncomingOptions: models.IncomingOptions{ChargesLosses: c.charges}}, tok)
			defer done()
			lateTC := tc("late", late.Sub(base))
			// Unnamed, as the agent streams a test case keploy record names:
			// one before the late span's instant and one after it, both over it.
			unnamedBefore := tc("", late.Sub(base)-time.Millisecond)
			unnamedAfter := tc("", late.Sub(base))
			unnamedAfter.HTTPReq.Timestamp = late.Add(-time.Microsecond)
			unnamedAfter.HTTPResp.Timestamp = late.Add(2 * time.Millisecond)
			svc.tc <- tc("early", 0) // streamed with its loss (the early span), or left out
			svc.tc <- lateTC
			svc.tc <- unnamedBefore
			svc.tc <- unnamedAfter
			svc.tc <- tc("clear", late.Sub(base)+time.Hour)
			_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			mr := multipart.NewReader(resp.Body, params["boundary"])
			// Read until "clear" went: every one is checked by then.
			for seen := false; !seen; {
				part, err := mr.NextPart()
				if err != nil {
					t.Fatalf("the stream ended before its test cases went: %v", err)
				}
				if part.FormName() == "metadata" {
					var got models.TestCase
					if err := json.NewDecoder(part).Decode(&got); err != nil {
						t.Fatal(err)
					}
					seen = got.Name == "clear"
				}
			}
			syncmgr.Get().RecordOrphanWindowWhy(late, late, "MySQL: late")
			endRecordingStreams(tok)
			var sum models.StreamEndSummary
			for {
				part, err := mr.NextPart()
				if err != nil {
					t.Fatalf("the stream ended without its marker: %v", err)
				}
				if part.FormName() == models.StreamEndPart {
					if err := json.NewDecoder(part).Decode(&sum); err != nil {
						t.Fatal(err)
					}
					break
				}
			}
			if !c.late {
				if len(sum.LateLosses) != 0 {
					t.Fatalf("the marker names %+v", sum.LateLosses)
				}
				return
			}
			want := models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: "MySQL: late"}}
			named := func(t *models.TestCase) string {
				return fmt.Sprintf("%q [%d,%d] %v", t.Name, t.HTTPReq.Timestamp.UnixNano(), t.HTTPResp.Timestamp.UnixNano(), want)
			}
			var got []string
			for _, l := range sum.LateLosses {
				got = append(got, fmt.Sprintf("%q [%d,%d] %v", l.Name, l.Request.UnixNano(), l.Response.UnixNano(), l.Lost))
			}
			if wantNamed := []string{named(lateTC), named(unnamedBefore), named(unnamedAfter)}; strings.Join(got, "\n") != strings.Join(wantNamed, "\n") {
				t.Fatalf("the marker names\n%s\nwant the late test cases alone, each by its window\n%s", strings.Join(got, "\n"), strings.Join(wantNamed, "\n"))
			}
		})
	}
}

// Under the proven switch, to a client that charges it, a gRPC test case is
// checked against the spans over its own window (GrpcReq, GrpcResp), as an
// HTTP one is over its: one over an exchange keploy could not record is
// streamed with it. With the switch off a gRPC test case is checked against
// nothing, and streamed as it was: the byte-identical default.
//
// Not parallel: the spans are the package-global manager's, and it sets the
// switch.
func TestUnderTheProvenSwitchAGRPCTestCaseOverALossIsStreamedWithIt(t *testing.T) {
	base := time.Date(2004, 5, 6, 7, 8, 9, 0, time.UTC).Add(time.Duration(time.Now().UnixNano()%1e9) * time.Microsecond)
	t.Setenv(provenswitch.Env, "1")
	syncmgr.Get().RecordOrphanWindowWhy(base, base.Add(time.Millisecond), "MySQL: under a gRPC call")
	grpcTC := func() *models.TestCase {
		t := &models.TestCase{Name: "grpc-1", Kind: models.GRPC_EXPORT}
		t.GrpcReq.Timestamp = base
		t.GrpcResp.Timestamp = base.Add(time.Millisecond)
		return t
	}
	for _, proven := range []bool{true, false} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			if !proven {
				t.Setenv(provenswitch.Env, "")
			}
			got, sum := streamIncoming(t, models.IncomingOptions{ChargesLosses: true}, grpcTC())
			if len(got) != 1 || got[0].Name != "grpc-1" || sum.TestCasesSuppressed != 0 {
				t.Fatalf("streamed %+v, summary %+v: the gRPC test case is to be streamed", got, sum)
			}
			want := models.CaptureLosses(nil)
			if proven {
				want = models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: "MySQL: under a gRPC call"}}
			}
			if fmt.Sprint(got[0].CaptureLost) != fmt.Sprint(want) || sum.KeptWithLoss != len(want) {
				t.Fatalf("the gRPC test case carries %+v (kept with loss %d), want %+v", got[0].CaptureLost, sum.KeptWithLoss, want)
			}
		})
	}
}

// Under the proven switch, a stream that ends other than at its marker (its
// capture closed it) carries no account of what test cases streamed with
// nothing lost turned out to have lost: the agent says how many at WARN, and
// its summary counts them (tcs_late_unsent), rather than leave them saved
// without that as their evidence unsaid.
//
// Not parallel: the spans are the package-global manager's, and it sets the
// switch.
func TestALateLossAStreamEndingWithoutItsMarkerCouldNotCarryIsSaid(t *testing.T) {
	base := time.Date(2005, 6, 7, 8, 9, 10, 0, time.UTC).Add(time.Duration(time.Now().UnixNano()%1e9) * time.Microsecond)
	t.Setenv(provenswitch.Env, "1")
	syncmgr.Get().SetWatermark(nil)
	core, logs := observer.New(zap.WarnLevel)
	svc := streamsSvc{tc: make(chan *models.TestCase, 1), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
	a := &Agent{logger: zap.New(core), svc: svc}
	resp, done := openHeldStream(t, a.HandleIncoming, models.IncomingReq{IncomingOptions: models.IncomingOptions{ChargesLosses: true}}, rand.Text())
	defer done()
	tc := &models.TestCase{Name: "clean-then-late"}
	tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp = base, base.Add(time.Millisecond)
	svc.tc <- tc
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "metadata" {
		t.Fatalf("the test case was not streamed: %v", err)
	}
	// After it went: the span its parser reports behind the traffic.
	syncmgr.Get().RecordOrphanWindowWhy(base, base, "MySQL: behind the traffic")
	close(svc.tc) // the capture ends the stream: no marker
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		if p.FormName() == models.StreamEndPart {
			t.Fatal("a stream its capture ended carried a marker")
		}
	}
	var said bool
	for i := 0; i < 200 && !said; i++ {
		for _, e := range logs.All() {
			if strings.Contains(e.Message, "the stream ended before its end marker could say so") && e.ContextMap()["test_cases"] == int64(1) && e.ContextMap()["ended_by"] == "capture" {
				said = true
			}
		}
		if !said {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !said {
		var msgs []string
		for _, e := range logs.All() {
			msgs = append(msgs, e.Message)
		}
		t.Fatalf("the late loss the stream could not carry was not said; WARNs: %q", msgs)
	}
}

// Under the proven switch, a test case streamed with what its recording lost
// stays watched for a span recorded after it went (syncMock.LateWatch.Streamed):
// the marker names it for a source its check did not see, that alone, so its
// evidence names every source over it, and not for one its check named, so no
// span is charged to it twice, the one its check read among them. One
// streamed with nothing lost that a later span overlaps is named with it.
//
// Not parallel: the spans are the package-global manager's, and it sets the
// switch.
func TestATestCaseStreamedWithItsLossIsNamedAtTheStreamsEndForASourceItsCheckDidNotSee(t *testing.T) {
	base := time.Date(2006, 7, 8, 9, 10, 11, 0, time.UTC).Add(time.Duration(time.Now().UnixNano()%1e9) * time.Microsecond)
	t.Setenv(provenswitch.Env, "1")
	syncmgr.Get().RecordOrphanWindowWhy(base, base.Add(time.Millisecond), "MySQL: early")
	syncmgr.Get().SetWatermark(nil)
	svc := streamsSvc{tc: make(chan *models.TestCase, 2), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	tok := rand.Text()
	resp, done := openHeldStream(t, a.HandleIncoming, models.IncomingReq{IncomingOptions: models.IncomingOptions{ChargesLosses: true}}, tok)
	defer done()
	lossy := &models.TestCase{Name: "lossy"}
	lossy.HTTPReq.Timestamp, lossy.HTTPResp.Timestamp = base, base.Add(time.Millisecond)
	clean := &models.TestCase{Name: "clean"}
	clean.HTTPReq.Timestamp, clean.HTTPResp.Timestamp = base.Add(time.Hour), base.Add(time.Hour+time.Millisecond)
	svc.tc <- lossy
	svc.tc <- clean
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	got := map[string]models.CaptureLosses{}
	for len(got) < 2 {
		part, err := mr.NextPart()
		if err != nil {
			t.Fatalf("the stream ended before its test cases went: %v", err)
		}
		if part.FormName() == "metadata" {
			var tc models.TestCase
			if err := json.NewDecoder(part).Decode(&tc); err != nil {
				t.Fatal(err)
			}
			got[tc.Name] = tc.CaptureLost
		}
	}
	if len(got["lossy"]) != 1 || got["lossy"][0].What != "MySQL: early" || len(got["clean"]) != 0 {
		t.Fatalf("streamed %+v: lossy with the early span, clean with nothing", got)
	}
	// After both went: a span over each, as a parser behind the traffic
	// reports one, and, over lossy, one more of the source its check named.
	syncmgr.Get().RecordOrphanWindowWhy(base, base.Add(time.Millisecond), "MySQL: early")
	syncmgr.Get().RecordOrphanWindowWhy(base, base.Add(time.Millisecond), "MySQL: late")
	syncmgr.Get().RecordOrphanWindowWhy(clean.HTTPReq.Timestamp, clean.HTTPResp.Timestamp, "MySQL: late")
	endRecordingStreams(tok)
	var sum models.StreamEndSummary
	for {
		part, err := mr.NextPart()
		if err != nil {
			t.Fatalf("the stream ended without its marker: %v", err)
		}
		if part.FormName() == models.StreamEndPart {
			if err := json.NewDecoder(part).Decode(&sum); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	late := map[string]models.CaptureLosses{}
	for _, l := range sum.LateLosses {
		late[l.Name] = l.Lost
	}
	want := models.CaptureLosses{{Kind: models.CaptureLossExchangeUnrecorded, Count: 1, What: "MySQL: late"}}
	if len(sum.LateLosses) != 2 || fmt.Sprint(late["lossy"]) != fmt.Sprint(want) || fmt.Sprint(late["clean"]) != fmt.Sprint(want) {
		t.Fatalf("the marker names %+v, want lossy with the late source alone (its check named the early one) and clean with it", sum.LateLosses)
	}
}

// Under the proven switch, to a client that charges it, the marker says how
// many test cases the stream sent past the most it watches for a span it
// learns of later (tcs_not_rechecked): the client takes that as what it could
// not be told, never as nothing lost. Within the bound, and to a client that
// does not charge it, it says none.
//
// Not parallel: it sets the switch and the watch's bound, both process-wide.
func TestTheMarkerSaysHowManyTestCasesWentPastTheLateWatch(t *testing.T) {
	base := time.Date(2007, 8, 9, 10, 11, 12, 0, time.UTC).Add(time.Duration(time.Now().UnixNano()%1e9) * time.Microsecond)
	t.Setenv(provenswitch.Env, "1")
	defer syncmgr.SetLateWatchBound(1)()
	tcs := func() []*models.TestCase {
		var out []*models.TestCase
		for i := 0; i < 3; i++ {
			tc := &models.TestCase{Name: fmt.Sprintf("t-%d", i)}
			tc.HTTPReq.Timestamp = base.Add(time.Duration(i) * time.Hour)
			tc.HTTPResp.Timestamp = tc.HTTPReq.Timestamp.Add(time.Millisecond)
			out = append(out, tc)
		}
		return out
	}
	if _, sum := streamIncoming(t, models.IncomingOptions{ChargesLosses: true}, tcs()...); sum.NotRechecked != 2 {
		t.Fatalf("the marker says %d went unwatched, want 2 (3 sent, 1 watched)", sum.NotRechecked)
	}
	if _, sum := streamIncoming(t, models.IncomingOptions{}, tcs()...); sum.NotRechecked != 0 {
		t.Fatalf("to a client that does not charge it the marker says %d went unwatched, want none", sum.NotRechecked)
	}
}

// The incoming stream is a record stream open on the manager from before its
// ingress starts until it ends (syncMock.SyncMockManager.OpenRecordStream),
// charging what its test cases lost or not as its client asked: so the lines
// that report a span keploy could not record say what this stream does with
// the test cases over it. Once it ends it says nothing of them.
//
// Not parallel: the streams are the package-global manager's, and it sets the
// switch.
func TestTheIncomingStreamSaysWhatItDoesWithTheTestCasesOverASpanWhileItIsOpen(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, c := range []struct {
		charges bool
		want    models.LossFate
	}{{true, models.LossFateKept}, {false, models.LossFateLeftOut}} {
		syncmgr.Get().SetWatermark(nil)
		svc := streamsSvc{tc: make(chan *models.TestCase), mocks: make(chan *models.Mock), maps: make(chan models.TestMockMapping)}
		a := &Agent{logger: zap.NewNop(), svc: svc}
		tok := rand.Text()
		resp, done := openHeldStream(t, a.HandleIncoming, models.IncomingReq{IncomingOptions: models.IncomingOptions{ChargesLosses: c.charges}}, tok)
		if got := syncmgr.Get().LossFate(); got != c.want {
			done()
			t.Fatalf("charges=%v: while the stream is open the manager says %v, want %v", c.charges, got, c.want)
		}
		endRecordingStreams(tok)
		_, _ = io.Copy(io.Discard, resp.Body)
		done()
		if got := syncmgr.Get().LossFate(); got != models.LossFateLeftOut {
			t.Fatalf("charges=%v: once the stream ended the manager says %v, want left out", c.charges, got)
		}
	}
}
