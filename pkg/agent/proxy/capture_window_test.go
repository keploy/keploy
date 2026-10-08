package proxy

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func mockMiss(summary string) models.ParserError {
	return models.ParserError{
		ParserErrorType: models.ErrMockNotFound,
		MismatchReport:  &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: summary, ClosestMock: "mock-0"},
	}
}

// TestCaptureWindow_NoBleedAcrossTests proves the per-test capture contract:
// a miss surfaces for the test it occurred in, and a consumed miss never
// carries over to the next test's GetMockErrors (the misattribution bug).
func TestCaptureWindow_NoBleedAcrossTests(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}

	p.BeginTestErrorCapture()
	p.errChannel <- mockMiss("POST /t1")
	got, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ActualSummary != "POST /t1" {
		t.Fatalf("test1 expected one miss [POST /t1], got %+v", got)
	}

	// Next test: fresh window, nothing pending — the previous miss must be gone.
	p.BeginTestErrorCapture()
	got, _ = p.GetMockErrors(context.Background())
	if len(got) != 0 {
		t.Fatalf("test2 must not inherit test1's miss, got %+v", got)
	}
}

// TestCaptureWindow_RendezvousNoLoss runs the real StartErrorDrain goroutine
// and proves the flush-marker rendezvous keeps a miss that's still in flight:
// the error is pushed onto errChannel and GetMockErrors is called immediately
// (the goroutine may not have routed it yet), yet it must still be returned —
// not lost to the window close.
func TestCaptureWindow_RendezvousNoLoss(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)

	for i := 0; i < 25; i++ {
		p.BeginTestErrorCapture()
		p.errChannel <- mockMiss("POST /t")
		got, err := p.GetMockErrors(context.Background())
		if err != nil {
			t.Fatalf("iter %d: unexpected error: %v", i, err)
		}
		if len(got) != 1 {
			t.Fatalf("iter %d: rendezvous lost the miss, got %d", i, len(got))
		}
	}
}

// TestCaptureWindow_BeginClearsStale proves BeginTestErrorCapture discards
// misses retained before the window (startup / background traffic) so they
// don't attach to the first test.
func TestCaptureWindow_BeginClearsStale(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}

	p.pendingMockErrors.addBounded(mockMiss("GET /background"), maxPendingMockErrors)
	p.BeginTestErrorCapture()

	got, _ := p.GetMockErrors(context.Background())
	if len(got) != 0 {
		t.Fatalf("stale pre-window miss should be cleared on Begin, got %+v", got)
	}
}

// pendingLen is how many misses wait with no capture window open.
func (p *Proxy) pendingLen() int {
	p.pendingMockErrors.mu.Lock()
	defer p.pendingMockErrors.mu.Unlock()
	return len(p.pendingMockErrors.errs)
}

// A miss made between two tests (after one test's window closed, before the
// next opened) is the next test's when the replayer continues the capture: a
// SEND the app publishes then would otherwise be reported against no test.
// Through the real drain goroutine, which files it with no window open.
func TestCaptureWindow_ContinueCarriesTheGapsMisses(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)

	p.BeginTestErrorCapture()
	p.errChannel <- mockMiss("POST /t1")
	if got, _ := p.GetMockErrors(context.Background()); len(got) != 1 || got[0].ActualSummary != "POST /t1" {
		t.Fatalf("test1 expected [POST /t1], got %+v", got)
	}

	p.errChannel <- mockMiss("SEND between tests") // no window is open
	deadline := time.Now().Add(5 * time.Second)
	for p.pendingLen() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the drain goroutine never filed the miss made between tests")
		}
		time.Sleep(time.Millisecond)
	}

	p.ContinueTestErrorCapture()
	p.errChannel <- mockMiss("POST /t2")
	got, _ := p.GetMockErrors(context.Background())
	if len(got) != 2 || got[0].ActualSummary != "SEND between tests" || got[1].ActualSummary != "POST /t2" {
		t.Fatalf("test2 expected [SEND between tests, POST /t2], got %+v", got)
	}

	// And a test that begins instead (a set's first) does not inherit it.
	p.errChannel <- mockMiss("SEND between tests")
	deadline = time.Now().Add(5 * time.Second)
	for p.pendingLen() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the drain goroutine never filed the second miss made between tests")
		}
		time.Sleep(time.Millisecond)
	}
	p.BeginTestErrorCapture()
	if got, _ := p.GetMockErrors(context.Background()); len(got) != 0 {
		t.Fatalf("a set's first test must not inherit what was missed before it, got %+v", got)
	}
}

// What a set's first test discards — misses made before it, with no window
// open — is logged, with the calls, so no miss is dropped without a word.
func TestCaptureWindow_BeginLogsWhatItDiscards(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	p := &Proxy{logger: zap.New(core), errChannel: make(chan error, 16)}

	p.pendingMockErrors.addBounded(mockMiss("GET /background"), maxPendingMockErrors)
	p.BeginTestErrorCapture()

	e := logs.FilterMessageSnippet("not attributed to a test").All()
	if len(e) != 1 || e[0].ContextMap()["count"] != int64(1) {
		t.Fatalf("want one Warn naming the discarded miss, got %+v", logs.All())
	}
	if calls, _ := e[0].ContextMap()["calls"].([]interface{}); len(calls) != 1 || calls[0] != "GET /background" {
		t.Fatalf("the Warn does not name the call: %+v", e[0].ContextMap())
	}

	// Nothing discarded, nothing said.
	logs.TakeAll()
	p.BeginTestErrorCapture()
	if n := logs.Len(); n != 0 {
		t.Fatalf("a Begin that discards nothing logged %d line(s)", n)
	}

	// DNS misses alone (an offline runner's upstream at the app's startup)
	// are not warned about: the end-of-run summary leaves them out too.
	dns := mockMiss("A example.com")
	dns.MismatchReport.Protocol = "DNS"
	p.pendingMockErrors.addBounded(dns, maxPendingMockErrors)
	p.BeginTestErrorCapture()
	if n := logs.Len(); n != 0 {
		t.Fatalf("a DNS-only discard logged %d Warn line(s): %+v", n, logs.All())
	}
}

// When GetMockErrors cannot close a test's window (the drain goroutine did not
// meet its flush marker in time), it reads the window and leaves it installed.
// What lands in it after that was made after the test's misses were taken,
// between two tests: the next test that continues the capture carries it,
// rather than dropping it as a window never read.
func TestCaptureWindow_ContinueCarriesWhatAReadWindowGotLater(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	p.errDrainActive.Store(true) // and no goroutine meets the marker: the degenerate path

	p.BeginTestErrorCapture()
	p.activeTestErrors.Load().addBounded(mockMiss("POST /t1"), maxPendingMockErrors)
	if got, _ := p.GetMockErrors(context.Background()); len(got) != 1 || got[0].ActualSummary != "POST /t1" {
		t.Fatalf("test1 expected [POST /t1], got %+v", got)
	}
	acc := p.activeTestErrors.Load()
	if acc == nil {
		t.Fatal("premise: the degenerate path closed the window")
	}
	acc.addBounded(mockMiss("SEND after test1 was read"), maxPendingMockErrors)

	p.ContinueTestErrorCapture()
	p.errDrainActive.Store(false) // the next read closes the window normally
	if got, _ := p.GetMockErrors(context.Background()); len(got) != 1 || got[0].ActualSummary != "SEND after test1 was read" {
		t.Fatalf("test2 expected [SEND after test1 was read], got %+v", got)
	}
}
