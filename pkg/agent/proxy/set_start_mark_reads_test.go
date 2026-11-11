package proxy

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A call's mark carries how many reads of its misses window had begun when
// it was made (integrations.CallRound.Reads), from every producer of a mark:
// the learning round's snapshot (RoundSnapshot.CallRound), which HTTP's and
// Mongo's learn steps mark their calls with, in the pool's view, with the
// learn tracker ahead of the store (a test begun whose view the store did not
// install), and in a test set the store has not staged; and the store's own
// (MockManager.MarkCallRound) in that set. The next set's start keeps an
// earlier set's call of no test only if it was marked before that set's last
// read (missWindow.keptAsEarlierSet), as at zero latency that read would have
// returned it: the pool-view call and the tracker-ahead call marked before
// set A's last read are kept, as set A's calls of no test, one set back. The
// same two marked after that read, and the calls marked between the sets (the
// tracker began set B, whose mocks are not staged yet), through the snapshot
// and the store alike, are cut and said, as the next set's start cuts them at
// zero latency.
func TestACallMarkedThroughTheRoundSnapshotIsKeptOnlyBeforeItsSetsLastRead(t *testing.T) {
	ctx := context.Background()
	core, logs := observer.New(zap.DebugLevel)
	p, mm := heldOrdersLogging(t, true, zap.New(core))
	p.dnsCache = newDNSCache()
	p.cutMissesAtSetStart(ctx)
	read := func(what string) {
		t.Helper()
		if got, err := p.GetMockErrors(ctx); err != nil || len(got) != 0 {
			t.Fatalf("precondition: %s: %v %+v", what, err, got)
		}
	}
	var sent []models.ParserError
	mark := func(call string, c integrations.CallRound) {
		t.Helper()
		if want := mm.windowReads.Load(); c.Reads() != want {
			t.Fatalf("%s's mark carries %d reads of the window; want the %d begun as it was made", call, c.Reads(), want)
		}
		e := mockMiss(call)
		integrations.StampMark(e.MismatchReport, c)
		sent = append(sent, e)
	}
	snap := func(what string, ahead, unstaged bool) integrations.CallRound {
		t.Helper()
		s, ok := mm.RoundSnapshot(integrations.RoundQuery{Kind: models.HTTP, Tracker: p.learnTracker})
		if !ok || s.TrackerAhead() != ahead || s.PoolsCall() != ahead || s.SetUnstaged() != unstaged || s.Test != "" {
			t.Fatalf("precondition: %s: the round's snapshot reads the tracker ahead %v (the pool's call %v), the set unstaged %v, test %q; want ahead %v, unstaged %v, no test: %+v", what, s.TrackerAhead(), s.PoolsCall(), s.SetUnstaged(), s.Test, ahead, unstaged, s.Boundary)
		}
		return s.CallRound()
	}
	mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own1")})
	p.ContinueTestErrorCapture()
	read("test-1's end read")
	p.EndTestErrorCapture()
	mm.ServeExactPool(models.PoolKeyNone, []string{"mock-pool"})
	mark("POST /pool-before-last-read", snap("between test-1 and test-2", false, false))
	// test-2 begun, its view not installed: the pool's call.
	p.ContinueTestErrorCapture()
	mark("POST /ahead-before-last-read", snap("test-2 begun", true, false))
	read("set A's last read")
	p.EndTestErrorCapture()
	mark("POST /pool-after-last-read", snap("after set A's last read", false, false))
	p.ContinueTestErrorCapture()
	mark("POST /ahead-after-last-read", snap("a test begun after set A's last read", true, false))
	p.WithdrawTestErrorCapture()
	p.learnBoundary(func(t *learn.Tracker) { t.NewSet() })
	mark("POST /unstaged-snapshot-mark", snap("set B begun, not staged", false, true))
	if c := mm.MarkCallRound(); !c.Staging() {
		t.Fatalf("precondition: the store's mark in set B, not staged, is no staging's")
	}
	mark("POST /unstaged-store-mark", mm.MarkCallRound())
	p.missCutPending.Store(true)
	if err := p.SetMocksWithWindow(ctx, nil, nil, models.BaseTime, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, e := range sent {
		p.SendError(e)
	}
	flushDrain(p)
	p.BeginTestErrorCapture()
	got, err := p.GetMockErrors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{"POST /pool-before-last-read": true, "POST /ahead-before-last-read": true}
	if len(got) != len(kept) {
		t.Fatalf("set B's first read %+v; want only set A's calls marked before its last read", got)
	}
	for _, c := range got {
		if !kept[c.ActualSummary] || c.EarlierSet != 1 || c.EarlierSetTest != "" || c.BeforeSet || c.Kind != models.MissOutsideTest {
			t.Fatalf("set B's first read %+v; want each of set A's calls marked before its last read as set A's call of no test, one set back, outside every test", got)
		}
		delete(kept, c.ActualSummary)
	}
	for _, c := range []string{"POST /pool-after-last-read", "POST /ahead-after-last-read", "POST /unstaged-snapshot-mark", "POST /unstaged-store-mark"} {
		if n := logs.FilterMessage(missesCutAtSetStart).FilterField(zap.Strings("calls", []string{c})).Len(); n != 1 {
			t.Fatalf("%s, marked after set A's last read, was said cut %d times; want once", c, n)
		}
	}
}
