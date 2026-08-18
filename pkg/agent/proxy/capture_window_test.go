package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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
