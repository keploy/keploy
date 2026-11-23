package routes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// openStreamLogged opens a HandleIncoming stream fed from tcs whose agent logs
// to logger.
func openStreamLogged(t *testing.T, tcs chan *models.TestCase, logger *zap.Logger) *http.Response {
	t.Helper()
	a := &Agent{logger: logger, svc: incomingSvc{tc: tcs}}
	srv := httptest.NewServer(http.HandlerFunc(a.HandleIncoming))
	t.Cleanup(srv.Close)
	body, _ := json.Marshal(models.IncomingReq{})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// httpCaseAt is an HTTP test case named name whose request came at `at`, and
// its answer a millisecond later.
func httpCaseAt(name string, at time.Time) *models.TestCase {
	return &models.TestCase{Name: name, Kind: models.HTTP,
		HTTPReq: models.HTTPReq{Timestamp: at}, HTTPResp: models.HTTPResp{Timestamp: at.Add(time.Millisecond)}}
}

// waitForSummary waits for the session's summary, logged as its stream ends:
// what the session says of its losses is logged before it.
func waitForSummary(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()
	deadline := time.Now().Add(streamWait)
	for logs.FilterMessage("agent: recording complete").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no summary once the stream ended")
		}
		time.Sleep(time.Millisecond)
	}
}

// A test case over the requests a parser never read because the recording's
// stop cut it short (the hang watchdog found it making no progress after the
// stop) is left out, and the session names it, as it ends, in one WARN for
// the stop: the first stopCutNamesMax test cases that cut left out, then how
// many more, and the reason. A test case left out over a span of another kind
// is not named there (it stays at Debug, per test case), and a clean one is
// streamed.
func TestHandleIncoming_NamesTheTestCasesTheStopsCutLeftOutInOneWarn(t *testing.T) {
	syncmgr.Get().SetWatermark(nil)
	core, logs := observer.New(zapcore.DebugLevel)
	const cut = stopCutNamesMax + 2
	tcs := make(chan *models.TestCase, cut+2)
	resp := openStreamLogged(t, tcs, zap.New(core))

	// In a stretch of its own: no span of another test reaches it.
	start := newStretch()
	if over, n := syncmgr.Get().WasMockOrphanedInWindow(start, start.Add(30*time.Second)); over {
		t.Fatalf("%d spans recorded before this test already cover its test cases", n)
	}
	syncmgr.Get().RecordOrphanWindow(start, start.Add(time.Millisecond))
	syncmgr.Get().RecordStopCutWindow(start.Add(time.Second), start.Add(2*time.Second))

	tcs <- httpCaseAt("over-another-span", start)
	var want []string
	for i := 0; i < cut; i++ {
		name := fmt.Sprintf("cut-%02d", i)
		if i < stopCutNamesMax {
			want = append(want, name)
		}
		tcs <- httpCaseAt(name, start.Add(time.Second+time.Duration(i)*10*time.Millisecond))
	}
	want = append(want, "and 2 more")
	tcs <- httpCaseAt("clean", start.Add(20*time.Second))
	close(tcs)

	if got := streamedNames(t, resp); len(got) != 1 || got[0] != "clean" {
		t.Fatalf("streamed %v, want only [clean]", got)
	}
	waitForSummary(t, logs)
	said := logs.FilterMessage(stopCutLeftOutMsg).All()
	if len(said) != 1 {
		t.Fatalf("%d lines name the test cases the stop's cut left out, want 1", len(said))
	}
	e := said[0]
	f := e.ContextMap()
	if e.Level != zapcore.WarnLevel {
		t.Errorf("the stop's cut is said at %v, want WARN", e.Level)
	}
	if n, _ := f["test_cases_left_out"].(int64); n != cut {
		t.Errorf("the WARN counts %v test cases left out, want %d", f["test_cases_left_out"], cut)
	}
	if got := fmt.Sprint(f["test_cases"]); got != fmt.Sprint(want) {
		t.Errorf("the WARN names %s, want %v", got, want)
	}
	if f["reason"] != stopCutReason {
		t.Errorf("the WARN gives the reason %q, want %q", f["reason"], stopCutReason)
	}
}

// A session that left test cases out over spans of other kinds alone says no
// stop cut's WARN: those stay at Debug, per test case.
func TestHandleIncoming_SaysNoStopCutWhenNoneLeftATestCaseOut(t *testing.T) {
	syncmgr.Get().SetWatermark(nil)
	core, logs := observer.New(zapcore.DebugLevel)
	tcs := make(chan *models.TestCase, 2)
	resp := openStreamLogged(t, tcs, zap.New(core))

	start := newStretch()
	if over, n := syncmgr.Get().WasMockOrphanedInWindow(start, start.Add(30*time.Second)); over {
		t.Fatalf("%d spans recorded before this test already cover its test cases", n)
	}
	syncmgr.Get().RecordOrphanWindow(start, start.Add(time.Millisecond))
	tcs <- httpCaseAt("over-a-span", start)
	tcs <- httpCaseAt("clean", start.Add(20*time.Second))
	close(tcs)

	if got := streamedNames(t, resp); len(got) != 1 || got[0] != "clean" {
		t.Fatalf("streamed %v, want only [clean]", got)
	}
	waitForSummary(t, logs)
	if n := logs.FilterMessage(stopCutLeftOutMsg).Len(); n != 0 {
		t.Errorf("%d lines say the stop's cut left out test cases, with none left out by it", n)
	}
	if n := logs.FilterMessageSnippet("agent: TC suppressed").FilterField(zap.String("tc_name", "over-a-span")).Len(); n != 1 {
		t.Errorf("%d Debug lines say the test case over a span was left out, want 1", n)
	}
}

// The WARN lists the first stopCutNamesMax names, and then how many more, only
// when there are more.
func TestStopCutLeftOut_ListsTheFirstNamesThenHowManyMore(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, stopCutNamesMax, stopCutNamesMax + 1, stopCutNamesMax + 5} {
		var l stopCutLeftOut
		var want []string
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("tc-%d", i)
			l.add(name)
			if i < stopCutNamesMax {
				want = append(want, name)
			}
		}
		if n > stopCutNamesMax {
			want = append(want, fmt.Sprintf("and %d more", n-stopCutNamesMax))
		}
		if got := l.listed(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%d left out: listed %v, want %v", n, got, want)
		}
		if l.n != n {
			t.Errorf("%d left out: counted %d", n, l.n)
		}
	}
}
