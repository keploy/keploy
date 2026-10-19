package proxy

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
	// With the proven switch off; on, the first test reads them as the set's
	// (TestUnderTheSwitchAMissBeforeTheSetsFirstTestIsTheSets).
	t.Setenv(provenswitch.Env, "")
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
	// With the proven switch off; on, the first test reads them as the set's
	// (TestUnderTheSwitchAMissBeforeTheSetsFirstTestIsTheSets).
	t.Setenv(provenswitch.Env, "")
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

	// Nor are connections that sent nothing (models.MatchPhaseNoRequest):
	// the end-of-run summary leaves them out of a green run too.
	silent := mockMiss("connection phase: server greeting")
	silent.MismatchReport.MatchPhase = models.MatchPhaseNoRequest
	p.pendingMockErrors.addBounded(silent, maxPendingMockErrors)
	p.BeginTestErrorCapture()
	if n := logs.Len(); n != 0 {
		t.Fatalf("a discard of connections that sent nothing logged %d Warn line(s): %+v", n, logs.All())
	}
}

// When GetMockErrors cannot close a test's window, it reads the window and
// leaves it installed. What lands in it after that was made after the test's
// misses were taken, between two tests: the next test that continues the
// capture carries it, rather than dropping it as a window never read.
//
// The drain is running (errDrainDone set before errDrainActive, as
// StartErrorDrain sets them) but never meets the flush marker. Nothing is
// bounded by a clock, with the proven switch on or off: the read ends when its
// caller gives it up, saying it could not vouch for every miss
// (ErrUnmatchedCallsUnread). With the switch off a two-second wait gave up on
// the drain and the read said nothing, so a miss the drain had not routed yet
// was read with the next test's as if it were that test's.
func TestCaptureWindow_ContinueCarriesWhatAReadWindowGotLater(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			if proven {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
			p.errDrainDone = make(chan struct{}) // a drain that runs and never reaches the marker
			p.errDrainActive.Store(true)

			// The caller gives the read up: the event that ends it.
			ctx, giveUp := context.WithCancel(context.Background())
			giveUp()
			p.BeginTestErrorCapture()
			p.activeTestErrors.Load().addBounded(mockMiss("POST /t1"), maxPendingMockErrors)
			got, err := p.GetMockErrors(ctx)
			if len(got) != 1 || got[0].ActualSummary != "POST /t1" {
				t.Fatalf("test1 expected [POST /t1], got %+v", got)
			}
			if !errors.Is(err, models.ErrUnmatchedCallsUnread) {
				t.Fatalf("test1's read error %v; a read that could not close its window says so", err)
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
		})
	}
}

// Under the proven switch a miss made before a set's first test began (the
// application's startup, with no window open) is the set's, no test's: the
// first test's read returns it marked as made before the set (BeforeSet), an
// outside_test miss, with the test's own, for the replay to fail the set
// with. It was dropped with a warning, and a set whose startup made a call
// no mock answers passed. The next test does not read it again.
func TestUnderTheSwitchAMissBeforeTheSetsFirstTestIsTheSets(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zap.WarnLevel)
	p := &Proxy{logger: zap.New(core), errChannel: make(chan error, 16)}

	p.pendingMockErrors.addBounded(mockMiss("insert on events"), maxPendingMockErrors)
	p.BeginTestErrorCapture()
	p.errChannel <- mockMiss("POST /t1")

	got, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatalf("GetMockErrors: %v", err)
	}
	read := map[string]models.UnmatchedCall{}
	for _, c := range got {
		read[c.ActualSummary] = c
	}
	if len(got) != 2 || len(read) != 2 {
		t.Fatalf("the first test read %+v; want the startup's miss and its own", got)
	}
	if c := read["insert on events"]; !c.BeforeSet || c.Kind != models.MissOutsideTest {
		t.Errorf("the startup's miss reads %+v; want it marked BeforeSet, %s", c, models.MissOutsideTest)
	}
	if c := read["POST /t1"]; c.BeforeSet {
		t.Errorf("the test's own miss reads %+v; want it unmarked", c)
	}
	if n := logs.FilterMessageSnippet("not attributed to a test").Len(); n != 0 {
		t.Errorf("the startup's miss was said to be dropped (%d)", n)
	}

	p.ContinueTestErrorCapture()
	if got, _ := p.GetMockErrors(context.Background()); len(got) != 0 {
		t.Fatalf("the next test read %+v; want nothing", got)
	}
}

// What the application missed after a set's last read (a call it made as it
// stopped, or later still), and while the next set's mocks were not staged yet
// (a warm application's call that missed for keploy's staging), is not the
// next set's: its begin cuts it, saying so, so its first test does not read
// it as made before that set. A begin that is not the set's first since its
// Mock (the set begun again), and a test's window, cut nothing: what was
// missed between two tests is the next test's.
func TestUnderTheSwitchAMissBeforeASetsMocksAreStagedIsNotTheSets(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zap.WarnLevel)
	p := New(zap.New(core), fakeDestInfo{}, config.New())
	p.nsswitchData = []byte("preseeded-by-test") // Mock leaves the host's DNS alone
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	p.StartErrorDrain(ctx)

	// The previous set's last test and its end read, and a call its
	// application makes after that read.
	p.BeginTestErrorCapture()
	if _, err := p.GetMockErrors(context.Background()); err != nil {
		t.Fatalf("the set's end read: %v", err)
	}
	p.SendError(mockMiss("POST /stopped"))
	// The next set: its Mock, a call missed before its mocks are staged,
	// its staging, then its application's startup.
	if err := p.Mock(context.Background(), models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	p.SendError(mockMiss("GET /health"))
	if err := p.BeginTestSet(context.Background(), time.Time{}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	p.SendError(mockMiss("insert on events"))
	flushDrain(p)
	// The set begun again (a replacement agent's repair) cuts nothing.
	if err := p.BeginTestSet(context.Background(), time.Time{}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	p.BeginTestErrorCapture()

	got, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatalf("GetMockErrors: %v", err)
	}
	if len(got) != 1 || got[0].ActualSummary != "insert on events" || !got[0].BeforeSet {
		t.Fatalf("the set's first test read %+v; want only its own startup's miss, as the set's", got)
	}
	e := logs.FilterMessageSnippet("before this set's mocks were staged").All()
	if len(e) != 1 {
		t.Fatalf("the cut was said %d times; want once: %+v", len(e), logs.All())
	}
	calls, _ := e[0].ContextMap()["calls"].([]interface{})
	if len(calls) != 2 || calls[0] != "POST /stopped" || calls[1] != "GET /health" {
		t.Fatalf("the warning names %v; want the call after the last read and the one before the staging", calls)
	}

	// Within the set, a miss between two tests is the next test's.
	p.SendError(mockMiss("POST /between"))
	flushDrain(p)
	if err := p.SetMocksWithWindow(context.Background(), nil, nil, time.Now(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	p.ContinueTestErrorCapture()
	if got, _ := p.GetMockErrors(context.Background()); len(got) != 1 || got[0].ActualSummary != "POST /between" || got[0].BeforeSet {
		t.Fatalf("the next test read %+v; want the miss made between the tests, its own", got)
	}
}
