package proxy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/matcher/learn"
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
// next set's: its staging cuts it, saying so, so its first test does not read
// it as made before that set. A staging that is not a set's first since its
// Mock (a test's window, a re-stage within the set) cuts nothing: what was
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
	if err := p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, time.Now()); err != nil {
		t.Fatal(err)
	}
	p.SendError(mockMiss("insert on events"))
	flushDrain(p)
	// A second staging of the set from BaseTime cuts nothing.
	if err := p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, time.Now()); err != nil {
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

// A window the agent's scope begin takes back (WithdrawTestErrorCapture: it
// opens the test's window before the test's view, and the view was refused)
// ends the learn tracker's test, and what the window held is carried into
// the next test's window whole: the misses made between tests that it took
// in, those made as the begin failed, and those made after it, until the
// next window opens. None is dropped, and none is left with the test that
// did not begin.
func TestAWithdrawnCaptureWindowIsCarriedIntoTheNextTestsWhole(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zap.WarnLevel)
	p := &Proxy{logger: zap.New(core), errChannel: make(chan error, 16), learnTracker: learn.NewTracker()}
	p.BeginTestErrorCapture()
	if got, err := p.GetMockErrors(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("the first test read %+v, %v; want nothing", got, err)
	}
	p.pendingMockErrors.addBounded(mockMiss("between tests"), maxPendingMockErrors)
	p.ContinueTestErrorCapture() // the begin that fails opens its window first
	if p.learnTracker.OpenEpoch() == 0 {
		t.Fatal("the window opened no learn tracker test")
	}
	// Filed as the drain files a miss: into the window open when it lands.
	p.activeTestErrors.Load().add(mockMiss("as the begin failed"))
	p.WithdrawTestErrorCapture()
	if n := p.learnTracker.OpenEpoch(); n != 0 {
		t.Fatalf("the window taken back left the tracker's test %d open", n)
	}
	p.activeTestErrors.Load().add(mockMiss("after the begin failed"))

	p.ContinueTestErrorCapture()
	got, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatalf("GetMockErrors: %v", err)
	}
	var read []string
	for _, c := range got {
		read = append(read, c.ActualSummary)
	}
	sort.Strings(read)
	if want := []string{"after the begin failed", "as the begin failed", "between tests"}; !reflect.DeepEqual(read, want) {
		t.Fatalf("the next test read %q; want %q", read, want)
	}
	if n := logs.FilterMessageSnippet("dropped").Len(); n != 0 {
		t.Fatalf("misses were said to be dropped (%d)", n)
	}
}

// A window taken back (WithdrawTestErrorCapture) ends the learn tracker's
// test through the mock manager, as every boundary of the proxy's tracker
// does (learnBoundary): holding the round lock, so the manager's round reads
// the tracker as it is. Ended on the tracker alone, the round still read the
// withdrawn test as open: a test's view the store then installed, before the
// tracker began the next test (the store opening it first), was bound to the
// withdrawn test, and once the tracker began that next test every read of
// the round found the two apart ("the learn tracker began another test"),
// though the store showed the very test the tracker had begun. Bound to
// none, the view is the test the tracker begins next, and the round is in it.
// No caller installs a view between a failed begin and the next window (the
// agent opens the window before the view); the row drives that order
// directly, as the store-led order LearnBoundary binds.
func TestAWithdrawnWindowEndsTheTrackersTestInTheManagersRound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own := exactMock("own", 0, models.LifetimePerTest)
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 4), learnTracker: learn.NewTracker()}
	mm := stagedManager(t, []*models.Mock{own}, nil)
	p.setMockManager(mm)
	p.BeginTestErrorCapture()
	mm.ServeExactTest("T1", []*models.Mock{own})
	p.EndTestErrorCapture()
	mm.ServeExactPool("-", nil)
	p.ContinueTestErrorCapture() // the begin that fails opens its window first
	p.WithdrawTestErrorCapture()
	if mm.exact.round.learn.open {
		t.Fatal("the manager's round still reads the withdrawn test as open")
	}
	mm.ServeExactTest("T2", []*models.Mock{own})
	p.ContinueTestErrorCapture()
	snap, ok := mm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP, Tracker: p.learnTracker})
	if !ok {
		t.Fatal("no round snapshot")
	}
	if snap.Phase != integrations.RoundInTest || snap.Test != "T2" {
		t.Fatalf("the round reads phase %v, test %q (%s); want T2's test, the one the tracker began over the view", snap.Phase, snap.Test, snap.Boundary)
	}
}

// kindedMiss is a miss its proxy placed (models.ParserError.MissKind).
func kindedMiss(summary, kind string) models.ParserError {
	m := mockMiss(summary)
	m.MissKind = kind
	return m
}

// Under the proven switch a miss made between two tests, filed while no
// test's window was open and made while no test's view served
// (models.MissOutsideTest), is the set's: the next test's read carries it
// marked Between, for the replay to fail the set with, charging that test
// nothing. A miss made in the previous test and filed after its read
// (models.MissInTest, models.MissExtraCall) is the set's too: it is carried
// marked Late, named under the test its mark names (MadeIn). With the switch
// off every miss is carried unmarked.
func TestUnderTheSwitchAMissBetweenTwoTestsIsTheSets(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("switch=%v", on), func(t *testing.T) {
			if on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}

			p.BeginTestErrorCapture()
			if got, _ := p.GetMockErrors(context.Background()); len(got) != 0 {
				t.Fatalf("the first test read %+v; want nothing", got)
			}
			p.pendingMockErrors.addBounded(kindedMiss("delete on sessions", models.MissOutsideTest), maxPendingMockErrors)
			p.pendingMockErrors.addBounded(kindedMiss("find on carts", models.MissInTest), maxPendingMockErrors)
			p.pendingMockErrors.addBounded(markedMiss("count on carts", models.MissExtraCall, "t1"), maxPendingMockErrors)
			p.ContinueTestErrorCapture()
			p.errChannel <- kindedMiss("POST /t2", models.MissInTest)

			got, err := p.GetMockErrors(context.Background())
			if err != nil {
				t.Fatalf("GetMockErrors: %v", err)
			}
			read := map[string]models.UnmatchedCall{}
			for _, c := range got {
				read[c.ActualSummary] = c
			}
			if len(got) != 4 || len(read) != 4 {
				t.Fatalf("the second test read %+v; want the three carried misses and its own", got)
			}
			if c := read["delete on sessions"]; c.Between != on || c.Kind != models.MissOutsideTest || c.BeforeSet {
				t.Errorf("the miss made between the tests reads %+v; want Between=%v, %s, not BeforeSet", c, on, models.MissOutsideTest)
			}
			if c := read["delete on sessions"]; c.Late {
				t.Errorf("the miss made between the tests reads %+v; want it not Late", c)
			}
			if c := read["find on carts"]; c.Between || c.Late != on || c.MadeIn != "" {
				t.Errorf("the previous test's late miss reads %+v; want Late=%v, no mark to name its test, not Between", c, on)
			}
			if c := read["count on carts"]; c.Between || c.Late != on || (on && c.MadeIn != "t1") {
				t.Errorf("the previous test's late extra call reads %+v; want Late=%v, made in t1, not Between", c, on)
			}
			if c := read["POST /t2"]; c.Between || c.Late {
				t.Errorf("the test's own miss reads %+v; want it unmarked", c)
			}
		})
	}
}

// markedMiss is a miss its parser marked in test (models.MockMismatchReport
// MarkedTest, with a known round), of kind.
func markedMiss(summary, kind, test string) models.ParserError {
	m := kindedMiss(summary, kind)
	m.MismatchReport.Mark = models.RoundMarkedSettled
	m.MismatchReport.MarkedInTest = test != ""
	m.MismatchReport.MarkRound = true
	m.MismatchReport.MarkedTest = test
	return m
}

// Under the proven switch keploy test's read of a test's end
// (models.WithTestEndRead) returns a miss whose mark names another test as
// made in that test (Late, MadeIn): keploy decided it after that test's end
// was read, and it reached the window of the test being read. A miss marked
// in the test read, or with no test in its mark, is that test's. A read that
// names no test (the scope agent's, the set's end) leaves every miss as it
// is, and so does a read with the switch off.
func TestATestsEndReadNamesAMissMarkedInAnEarlierTest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		on     bool
		ctx    context.Context
		isLate bool
	}{
		{"switch on, keploy test's end read", true, models.WithTestEndRead(context.Background(), "t2"), true},
		{"switch on, a read naming no test", true, context.Background(), false},
		{"switch off", false, models.WithTestEndRead(context.Background(), "t2"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
			p.BeginTestErrorCapture()
			if got, _ := p.GetMockErrors(models.WithTestEndRead(context.Background(), "t1")); len(got) != 0 {
				t.Fatalf("the first test read %+v; want nothing", got)
			}
			p.ContinueTestErrorCapture()
			p.errChannel <- markedMiss("GET /t1-late", models.MissInTest, "t1")
			p.errChannel <- markedMiss("GET /t2-own", models.MissInTest, "t2")
			p.errChannel <- kindedMiss("GET /t2-unmarked", models.MissInTest)

			got, err := p.GetMockErrors(tc.ctx)
			if err != nil {
				t.Fatalf("GetMockErrors: %v", err)
			}
			read := map[string]models.UnmatchedCall{}
			for _, c := range got {
				read[c.ActualSummary] = c
			}
			if len(got) != 3 || len(read) != 3 {
				t.Fatalf("the second test read %+v; want its three misses", got)
			}
			if c := read["GET /t1-late"]; c.Late != tc.isLate || (tc.isLate && c.MadeIn != "t1") || c.Between || c.BeforeSet {
				t.Errorf("the miss marked in t1 reads %+v; want Late=%v, made in t1", c, tc.isLate)
			}
			for _, own := range []string{"GET /t2-own", "GET /t2-unmarked"} {
				if c := read[own]; c.Late || c.MadeIn != "" {
					t.Errorf("t2's own miss %s reads %+v; want it unmarked", own, c)
				}
			}
		})
	}
}

// A miss marked in a test that a set's first window carries is made before
// the set (BeforeSet), never Late, even when the read names another test:
// the set's start keeps it as the set's.
func TestASetsFirstReadKeepsAMarkedMissBeforeTheSetNotLate(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	p.pendingMockErrors.addBounded(markedMiss("GET /last-sets-test", models.MissInTest, "t9"), maxPendingMockErrors)
	p.BeginTestErrorCapture()
	got, err := p.GetMockErrors(models.WithTestEndRead(context.Background(), "t1"))
	if err != nil {
		t.Fatalf("GetMockErrors: %v", err)
	}
	if len(got) != 1 || !got[0].BeforeSet || got[0].Late || got[0].MadeIn != "" {
		t.Fatalf("the set's first read %+v; want the marked miss BeforeSet, not Late", got)
	}
}
