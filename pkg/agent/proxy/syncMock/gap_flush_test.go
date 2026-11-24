package manager

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func gapMockAt(req, res time.Time) *models.Mock {
	return &models.Mock{
		Kind: models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "HTTP_CLIENT"},
			ReqTimestampMock: req,
			ResTimestampMock: res,
		},
		TestModeInfo: models.TestModeInfo{
			Lifetime:        models.LifetimePerTest,
			LifetimeDerived: true,
		},
	}
}

func drainMocks(ch chan *models.Mock) []*models.Mock {
	var out []*models.Mock
	for {
		select {
		case mk, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, mk)
		default:
			return out
		}
	}
}

func TestWindowedManagerFlushesPostResponseGapMock(t *testing.T) {
	t.Run("live FlushOwnedWindows waits for successor kept window and StaleHorizon, then stamps interTestGap without OutsideRequests", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.SetWindowsOpenLate()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		base := time.Now().Add(-200 * time.Millisecond)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "post-async-1", true)

		gapReq := base.Add(5 * time.Millisecond)
		gapMock := gapMockAt(gapReq, gapReq.Add(3*time.Millisecond))
		mgr.AddMock(gapMock)

		// Before successor kept window resolves: never flushed even past StaleHorizon.
		mgr.flushOwnedWindowsAt(base.Add(StaleHorizon+time.Second), false)
		if got := drainMocks(ch); len(got) != 0 {
			t.Fatalf("flushed %d mocks before successor kept window resolved; want 0", len(got))
		}

		w2Start := base.Add(15 * time.Millisecond)
		w2 := mgr.OpenWindow(w2Start, nil)
		mgr.ResolveKept(w2, w2Start, w2Start.Add(50*time.Millisecond), "wait-async-1", true)

		// Within StaleHorizon: live FlushOwnedWindows holds the mock.
		mgr.FlushOwnedWindows()
		if got := drainMocks(ch); len(got) != 0 {
			t.Fatalf("live FlushOwnedWindows within StaleHorizon emitted %d mocks; want 0", len(got))
		}

		// Once older than StaleHorizon and bounded between w1 and w2: flushed
		// live even on late-opening ingresses (e.g. K8s skipListener where
		// CloseOutChan does not run).
		mgr.flushOwnedWindowsAt(base.Add(StaleHorizon+time.Second), false)
		got := drainMocks(ch)
		if len(got) != 1 || got[0] != gapMock {
			t.Fatalf("FlushOwnedWindows past StaleHorizon emitted %d mocks (%v); want gapMock", len(got), got)
		}
		if !models.IsInterTestGap(gapMock) {
			t.Fatalf("flushed gap mock missing %s=true metadata: %+v", models.MetadataInterTestGapKey, gapMock.Spec.Metadata)
		}
		if gapMock.OutsideRequests {
			t.Fatal("flushed gap mock must not set OutsideRequests=true")
		}
		if gapMock.Name == "" {
			t.Fatal("flushed gap mock must receive a generated mock name")
		}
	})

	t.Run("ResolveKept rescues aged gap mock at stale cutoff when successor kept window resolves", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		base := time.Now().Add(-StaleHorizon - time.Second)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "post-async-1", true)

		gapMock := gapMockAt(base.Add(5*time.Millisecond), base.Add(8*time.Millisecond))
		mgr.AddMock(gapMock)

		// w2 resolves after gapMock has already aged past StaleHorizon: w2's
		// own stale-cutoff pass must rescue gapMock instead of dropping it.
		w2Start := base.Add(15 * time.Millisecond)
		w2 := mgr.OpenWindow(w2Start, nil)
		mgr.ResolveKept(w2, w2Start, w2Start.Add(50*time.Millisecond), "wait-async-1", true)

		got := drainMocks(ch)
		if len(got) != 1 || got[0] != gapMock || !models.IsInterTestGap(gapMock) {
			t.Fatalf("ResolveKept(w2) emitted %v; want aged gapMock stamped interTestGap=true", got)
		}
	})

	t.Run("live FlushOwnedWindows holds young mocks within StaleHorizon so late-opening or windowless requests claim them; stop drain rescues true gap", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.SetWindowsOpenLate()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		base := time.Now().Add(-200 * time.Millisecond)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "t1", true)

		// Request A's egress arrives before A's late window (or windowless gRPC
		// resolve) runs, while a faster request B resolves kept ahead of A.
		lateReqMock := gapMockAt(base.Add(5*time.Millisecond), base.Add(7*time.Millisecond))
		mgr.AddMock(lateReqMock)
		trueGapMock := gapMockAt(base.Add(12*time.Millisecond), base.Add(14*time.Millisecond))
		mgr.AddMock(trueGapMock)

		wBStart := base.Add(20 * time.Millisecond)
		wB := mgr.OpenWindow(wBStart, nil)
		mgr.ResolveKept(wB, wBStart, wBStart.Add(5*time.Millisecond), "tB", true)

		// Live periodic flush must NOT steal young mocks within StaleHorizon.
		mgr.FlushOwnedWindows()
		if got := drainMocks(ch); len(got) != 0 {
			t.Fatalf("live FlushOwnedWindows within StaleHorizon emitted %d mocks; want 0", len(got))
		}

		// Windowless kept resolve (e.g. gRPC) claims lateReqMock cleanly.
		mgr.ResolveRange(base.Add(4*time.Millisecond), base.Add(10*time.Millisecond), "grpc-A", true, true)
		if got := drainMocks(ch); len(got) != 1 || got[0] != lateReqMock {
			t.Fatalf("ResolveRange(grpc-A) emitted %v; want lateReqMock", got)
		}
		if models.IsInterTestGap(lateReqMock) {
			t.Fatal("mock claimed by ResolveRange(grpc-A) must not be stamped interTestGap")
		}

		// Stop drain rescues trueGapMock with interTestGap=true.
		mgr.CloseOutChan()
		got := drainMocks(ch)
		if len(got) != 1 || got[0] != trueGapMock || !models.IsInterTestGap(trueGapMock) {
			t.Fatalf("CloseOutChan emitted %v; want trueGapMock stamped interTestGap=true", got)
		}
	})

	t.Run("trailing mocks after the last kept window are never flushed", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		base := time.Now().Add(-200 * time.Millisecond)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "only-test", true)

		trailing := gapMockAt(base.Add(5*time.Millisecond), base.Add(7*time.Millisecond))
		mgr.AddMock(trailing)

		mgr.flushOwnedWindowsAt(base.Add(StaleHorizon+time.Second), false)
		mgr.CloseOutChan()
		if got := drainMocks(ch); len(got) != 0 {
			t.Fatalf("trailing post-last-test mock leaked: emitted %d mocks; want 0", len(got))
		}
	})

	t.Run("does not flush mock covered by an open window", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		base := time.Now().Add(-200 * time.Millisecond)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "test-1", true)

		w2Start := base.Add(10 * time.Millisecond)
		w2 := mgr.OpenWindow(w2Start, nil)
		inFlight := gapMockAt(w2Start.Add(2*time.Millisecond), w2Start.Add(4*time.Millisecond))
		mgr.AddMock(inFlight)

		w3Start := base.Add(40 * time.Millisecond)
		w3 := mgr.OpenWindow(w3Start, nil)
		mgr.ResolveKept(w3, w3Start, w3Start.Add(5*time.Millisecond), "test-3", true)

		mgr.flushOwnedWindowsAt(base.Add(StaleHorizon+time.Second), false)
		if got := drainMocks(ch); len(got) != 0 {
			t.Fatalf("flushed %d mocks while owning window w2 is still open; want 0", len(got))
		}

		mgr.ResolveKept(w2, w2Start, w2Start.Add(10*time.Millisecond), "test-2", true)
		if got := drainMocks(ch); len(got) != 1 || got[0] != inFlight {
			t.Fatalf("ResolveKept emitted %d mocks; want inFlight", len(got))
		}
		if models.IsInterTestGap(inFlight) {
			t.Fatal("in-window mock claimed by ResolveKept must not be stamped interTestGap")
		}
	})

	t.Run("filtered window overlapping a kept window never leaks its egress as gap mock", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		now := time.Now()
		w1Start := now.Add(-50 * time.Millisecond)
		w1End := now.Add(-20 * time.Millisecond)
		w1 := mgr.OpenWindow(w1Start, nil)
		// Filtered health check starts INSIDE kept w1, then runs past w1End.
		filteredStart := now.Add(-30 * time.Millisecond)
		filteredWin := mgr.OpenWindow(filteredStart, nil)
		mgr.ResolveKept(w1, w1Start, w1End, "kept-1", true)

		filteredMock := gapMockAt(now.Add(-10*time.Millisecond), now.Add(-8*time.Millisecond))
		mgr.AddMock(filteredMock)
		filteredWin.Close()

		w2Start := time.Now().Add(10 * time.Millisecond)
		w2 := mgr.OpenWindow(w2Start, nil)
		mgr.ResolveKept(w2, w2Start, w2Start.Add(5*time.Millisecond), "kept-2", true)

		mgr.flushOwnedWindowsAt(now.Add(StaleHorizon+time.Second), false)
		mgr.CloseOutChan()
		if got := drainMocks(ch); len(got) != 0 {
			t.Fatalf("filtered window overlapping kept window leaked %d mock(s); want 0", len(got))
		}
	})

	t.Run("sync duplicate window mocks are pruned and never flushed as gap mocks", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.SetWindowsOpenLate()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		base := time.Now().Add(-200 * time.Millisecond)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "test-1", true)

		dupStart := base.Add(10 * time.Millisecond)
		dupEnd := base.Add(20 * time.Millisecond)
		dupWin := mgr.OpenWindow(dupStart, nil)
		dupMock := gapMockAt(dupStart.Add(3*time.Millisecond), dupStart.Add(5*time.Millisecond))
		mgr.AddMock(dupMock)
		mgr.ResolveRange(dupStart, dupEnd, "", false, false)
		dupWin.Close()

		lateDupMock := gapMockAt(dupStart.Add(6*time.Millisecond), dupStart.Add(8*time.Millisecond))
		mgr.AddMock(lateDupMock)

		w2Start := base.Add(40 * time.Millisecond)
		w2 := mgr.OpenWindow(w2Start, nil)
		mgr.ResolveKept(w2, w2Start, w2Start.Add(5*time.Millisecond), "test-2", true)

		mgr.flushOwnedWindowsAt(base.Add(StaleHorizon+time.Second), false)
		mgr.CloseOutChan()
		if got := drainMocks(ch); len(got) != 0 {
			t.Fatalf("sync duplicate mocks leaked: emitted %d mocks; want 0", len(got))
		}
	})

	t.Run("async duplicate closed via Window.Close is pruned by DeleteMocksStrictlyBefore while rescuing earlier gap mock", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.SetWindowsOpenLate()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		now := time.Now()
		w1Start := now.Add(-100 * time.Millisecond)
		w1 := mgr.OpenWindow(w1Start, nil)
		mgr.ResolveKept(w1, w1Start, w1Start.Add(2*time.Millisecond), "post-async-1", true)

		gapMock := gapMockAt(w1Start.Add(5*time.Millisecond), w1Start.Add(7*time.Millisecond))
		mgr.AddMock(gapMock)

		w2Start := w1Start.Add(20 * time.Millisecond)
		w2 := mgr.OpenWindow(w2Start, nil)
		mgr.ResolveKept(w2, w2Start, w2Start.Add(10*time.Millisecond), "wait-async-1", true)

		// Async duplicate opens a window, emits its own mock, closes win
		// (recording unkeptSpan [dupStart, now]) without calling ResolveRange,
		// and later runs DeleteMocksStrictlyBefore in queue order.
		dupStart := w1Start.Add(40 * time.Millisecond)
		dupWin := mgr.OpenWindow(dupStart, nil)
		dupMock := gapMockAt(dupStart.Add(2*time.Millisecond), dupStart.Add(4*time.Millisecond))
		mgr.AddMock(dupMock)
		dupWin.Close()

		w3Start := time.Now().Add(10 * time.Millisecond)
		w3 := mgr.OpenWindow(w3Start, nil)
		mgr.ResolveKept(w3, w3Start, w3Start.Add(5*time.Millisecond), "later-kept-3", true)

		mgr.DeleteMocksStrictlyBefore(w3Start)
		got := drainMocks(ch)
		if len(got) != 1 || got[0] != gapMock {
			t.Fatalf("DeleteMocksStrictlyBefore emitted %v; want only gapMock rescued and dupMock dropped", got)
		}
		if !models.IsInterTestGap(gapMock) {
			t.Fatal("rescued gapMock missing interTestGap=true")
		}
	})

	t.Run("exchange hold and givenUpInFlight block gap flush", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		base := time.Now().Add(-200 * time.Millisecond)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "t1", true)

		gapMock := gapMockAt(base.Add(5*time.Millisecond), base.Add(7*time.Millisecond))
		mgr.AddMock(gapMock)

		w2Start := base.Add(20 * time.Millisecond)
		w2 := mgr.OpenWindow(w2Start, nil)
		mgr.ResolveKept(w2, w2Start, w2Start.Add(5*time.Millisecond), "t2", true)

		later := base.Add(StaleHorizon + time.Second)
		_, releaseHold := mgr.HoldExchanges(base.Add(4 * time.Millisecond))
		mgr.flushOwnedWindowsAt(later, false)
		if got := drainMocks(ch); len(got) != 0 {
			t.Fatalf("flushed %d mocks while exchange hold active; want 0", len(got))
		}
		releaseHold()

		mgr.mu.Lock()
		mgr.givenUpInFlight = 1
		mgr.mu.Unlock()
		mgr.flushOwnedWindowsAt(later, false)
		if got := drainMocks(ch); len(got) != 0 {
			t.Fatalf("flushed %d mocks while givenUpInFlight > 0; want 0", len(got))
		}
		mgr.mu.Lock()
		mgr.givenUpInFlight = 0
		mgr.mu.Unlock()

		mgr.flushOwnedWindowsAt(later, false)
		if got := drainMocks(ch); len(got) != 1 || got[0] != gapMock {
			t.Fatalf("after blockers cleared: emitted %d mocks; want gapMock", len(got))
		}
	})

	t.Run("open test scope under proven switch blocks gap flush", func(t *testing.T) {
		t.Setenv("KEPLOY_PROVEN", "1")
		ch := make(chan *models.Mock, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		base := time.Now().Add(-200 * time.Millisecond)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "t1", true)

		gapMock := gapMockAt(base.Add(5*time.Millisecond), base.Add(7*time.Millisecond))
		mgr.AddMock(gapMock)

		w2Start := base.Add(20 * time.Millisecond)
		w2 := mgr.OpenWindow(w2Start, nil)
		mgr.ResolveKept(w2, w2Start, w2Start.Add(5*time.Millisecond), "t2", true)

		later := base.Add(StaleHorizon + time.Second)
		scope := mgr.BeginTestScope(base.Add(4 * time.Millisecond))
		if scope == nil {
			t.Fatal("BeginTestScope returned nil with KEPLOY_PROVEN=1")
		}
		mgr.flushOwnedWindowsAt(later, false)
		if got := drainMocks(ch); len(got) != 0 {
			t.Fatalf("flushed %d mocks while test scope open; want 0", len(got))
		}
		scope.Abandon()

		mgr.flushOwnedWindowsAt(later, false)
		if got := drainMocks(ch); len(got) != 1 || got[0] != gapMock {
			t.Fatalf("after scope abandoned: emitted %d mocks; want gapMock", len(got))
		}
	})

	t.Run("caps each gap independently at maxGapMocksPerGap and drops overflow without blocking later gaps", func(t *testing.T) {
		ch := make(chan *models.Mock, 2*maxGapMocksPerGap+32)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		base := time.Now().Add(-500 * time.Millisecond)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "t1", true)

		// Gap 1: maxGapMocksPerGap + 10 mocks -> capped at maxGapMocksPerGap,
		// overflow dropped from buffer.
		for i := 0; i < maxGapMocksPerGap+10; i++ {
			ts := base.Add(5*time.Millisecond + time.Duration(i)*time.Microsecond)
			mgr.AddMock(gapMockAt(ts, ts.Add(time.Microsecond)))
		}

		w2Start := base.Add(200 * time.Millisecond)
		w2 := mgr.OpenWindow(w2Start, nil)
		mgr.ResolveKept(w2, w2Start, w2Start.Add(5*time.Millisecond), "t2", true)

		// Gap 2: 2 mocks -> independent cap, both must still flush!
		g2a := gapMockAt(base.Add(220*time.Millisecond), base.Add(221*time.Millisecond))
		g2b := gapMockAt(base.Add(225*time.Millisecond), base.Add(226*time.Millisecond))
		mgr.AddMock(g2a)
		mgr.AddMock(g2b)

		w3Start := base.Add(300 * time.Millisecond)
		w3 := mgr.OpenWindow(w3Start, nil)
		mgr.ResolveKept(w3, w3Start, w3Start.Add(5*time.Millisecond), "t3", true)

		mgr.flushOwnedWindowsAt(base.Add(StaleHorizon+2*time.Second), false)
		got := drainMocks(ch)
		if len(got) != maxGapMocksPerGap+2 {
			t.Fatalf("emitted %d gap mocks across 2 gaps; want %d (cap %d for gap1 + 2 for gap2)",
				len(got), maxGapMocksPerGap+2, maxGapMocksPerGap)
		}
		mgr.mu.Lock()
		remaining := len(mgr.buffer)
		mgr.mu.Unlock()
		if remaining != 0 {
			t.Fatalf("buffer still holds %d capped gap mocks after FlushOwnedWindows; want 0", remaining)
		}
		mgr.CloseOutChan()
	})

	t.Run("late-opening Backlog PendingBefore blocks gap rescue so delayed capture hits stale-cutoff leave-out instead of saving incomplete TC", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.SetWindowsOpenLate()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		wm := &backlogWatermark{}
		wm.pending.Store(true)
		mgr.SetWatermark(wm)

		base := time.Now().Add(-StaleHorizon - 2*time.Second)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "t1", true)

		// Delayed request A's mock arrives while A's window has not opened yet
		// and Backlog watermark says capture has NOT progressed past it.
		lateReqMock := gapMockAt(base.Add(5*time.Millisecond), base.Add(8*time.Millisecond))
		mgr.AddMock(lateReqMock)

		// Faster request B resolves kept past StaleHorizon: gap rescue MUST NOT
		// steal lateReqMock while Backlog.PendingBefore is true; instead it
		// falls through to cutDropped so request A is left out at its verdict.
		wBStart := base.Add(20 * time.Millisecond)
		wB := mgr.OpenWindow(wBStart, nil)
		mgr.ResolveKept(wB, wBStart, wBStart.Add(5*time.Millisecond), "tB", true)

		if got := drainMocks(ch); len(got) != 0 {
			t.Fatalf("emitted %d mock(s) while Backlog.PendingBefore=true; want 0", len(got))
		}
		if mgr.cutDrops.Load() != 1 {
			t.Fatalf("cutDrops = %d; want 1 (mock must enter cutDropped leave-out path)", mgr.cutDrops.Load())
		}
		mgr.CloseOutChan()
	})

	t.Run("late-opening window resolving after gap rescue claims rescued mock into its mapping", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mapCh := make(chan models.TestMockMapping, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetMappingChannel(context.Background(), mapCh)
		mgr.SetFirstRequestSignaled()
		mgr.SetWindowsOpenLate()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		base := time.Now().Add(-200 * time.Millisecond)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "t1", true)

		lateMock := gapMockAt(base.Add(5*time.Millisecond), base.Add(8*time.Millisecond))
		mgr.AddMock(lateMock)

		wBStart := base.Add(20 * time.Millisecond)
		wB := mgr.OpenWindow(wBStart, nil)
		mgr.ResolveKept(wB, wBStart, wBStart.Add(5*time.Millisecond), "tB", true)

		// Periodic flush past StaleHorizon rescues lateMock and notes it in sentClaimed.
		mgr.flushOwnedWindowsAt(base.Add(StaleHorizon+time.Second), false)
		got := drainMocks(ch)
		if len(got) != 1 || got[0] != lateMock {
			t.Fatalf("flushOwnedWindowsAt emitted %v; want lateMock", got)
		}

		// A subsequent flush (running pruneSentClaimedLocked) with no Backlog
		// watermark must NOT prune the delivered gap claim before late-A opens.
		mgr.FlushOwnedWindows()
		mgr.mu.Lock()
		keptClaims := len(mgr.sentClaimed)
		mgr.mu.Unlock()
		if keptClaims != 1 {
			t.Fatalf("sentClaimed after no-Backlog prune = %d; want 1", keptClaims)
		}

		// Later, the delayed request A finally opens and resolves kept over
		// lateMock's timestamp: takeSentClaimedLocked must attach lateMock.Name
		// to A's mapping so A is never saved without its mock.
		wAStart := base.Add(4 * time.Millisecond)
		wA := mgr.OpenWindow(wAStart, nil)
		mgr.ResolveKept(wA, wAStart, base.Add(12*time.Millisecond), "late-A", true)

		var foundMapping bool
		for {
			select {
			case mp := <-mapCh:
				if mp.TestName == "late-A" {
					for _, id := range mp.MockIDs {
						if id == lateMock.Name {
							foundMapping = true
						}
					}
				}
			default:
				goto doneDrainMap
			}
		}
	doneDrainMap:
		if !foundMapping {
			t.Fatalf("late-A mapping did not claim rescued gap mock %q", lateMock.Name)
		}
		mgr.CloseOutChan()
	})

	t.Run("gap key ring evicts oldest past maxRecentWindows and resets across EndRecording/ResumeRecording", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		// Pre-fill maxRecentWindows distinct gap keys to exercise ring eviction.
		mgr.mu.Lock()
		mgr.gapFlushed = make(map[int64]int, maxRecentWindows)
		mgr.gapCapWarned = make(map[int64]bool, maxRecentWindows)
		mgr.gapKeyOrder = make([]int64, maxRecentWindows)
		for i := 0; i < maxRecentWindows; i++ {
			k := int64(i + 1)
			mgr.gapFlushed[k] = 1
			mgr.gapKeyOrder[i] = k
		}
		mgr.mu.Unlock()

		base := time.Now().Add(-200 * time.Millisecond)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "t1", true)

		gapMock := gapMockAt(base.Add(5*time.Millisecond), base.Add(7*time.Millisecond))
		mgr.AddMock(gapMock)

		w2Start := base.Add(20 * time.Millisecond)
		w2 := mgr.OpenWindow(w2Start, nil)
		mgr.ResolveKept(w2, w2Start, w2Start.Add(5*time.Millisecond), "t2", true)

		// Gap maxRecentWindows+1 must still flush by evicting oldest key 1.
		mgr.flushOwnedWindowsAt(base.Add(StaleHorizon+time.Second), false)
		if got := drainMocks(ch); len(got) != 1 || got[0] != gapMock {
			t.Fatalf("gap past maxRecentWindows emitted %v; want gapMock", got)
		}
		mgr.mu.Lock()
		_, oldestStillPresent := mgr.gapFlushed[1]
		mapLen := len(mgr.gapFlushed)
		mgr.mu.Unlock()
		if oldestStillPresent || mapLen != maxRecentWindows {
			t.Fatalf("ring eviction failed: oldestStillPresent=%v len=%d want false/%d", oldestStillPresent, mapLen, maxRecentWindows)
		}

		// EndRecording + ResumeRecording must reset gap state cleanly.
		mgr.EndRecording()
		mgr.ResumeRecording()
		mgr.mu.Lock()
		if len(mgr.gapFlushed) != 0 || len(mgr.gapKeyOrder) != 0 || !mgr.firstKeptEnd.IsZero() || !mgr.latestKeptStart.IsZero() {
			mgr.mu.Unlock()
			t.Fatal("EndRecording/ResumeRecording did not reset gap state")
		}
		mgr.mu.Unlock()
		mgr.CloseOutChan()
	})

	t.Run("late-opening capped flush overflow without Backlog records cut drop and leaves late test case out", func(t *testing.T) {
		ch := make(chan *models.Mock, maxGapMocksPerGap+32)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetFirstRequestSignaled()
		mgr.SetWindowsOpenLate()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		base := time.Now().Add(-500 * time.Millisecond)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "t1", true)

		// Delayed request A spans [base+4ms, base+20ms] and emits more than
		// maxGapMocksPerGap calls before its window opens; no Backlog watermark
		// is registered.
		gapStart := base.Add(4 * time.Millisecond)
		for i := 0; i < maxGapMocksPerGap+10; i++ {
			ts := base.Add(5*time.Millisecond + time.Duration(i)*time.Microsecond)
			mgr.AddMock(gapMockAt(ts, ts.Add(time.Microsecond)))
		}
		gapEnd := base.Add(20 * time.Millisecond)

		w2Start := base.Add(100 * time.Millisecond)
		w2 := mgr.OpenWindow(w2Start, nil)
		mgr.ResolveKept(w2, w2Start, w2Start.Add(5*time.Millisecond), "t2", true)

		mgr.flushOwnedWindowsAt(base.Add(StaleHorizon+time.Second), false)
		if got := drainMocks(ch); len(got) != maxGapMocksPerGap {
			t.Fatalf("emitted %d mocks; want %d", len(got), maxGapMocksPerGap)
		}
		if mgr.cutDrops.Load() != 10 {
			t.Fatalf("cutDrops = %d; want 10 (overflow mocks must enter cutDropped)", mgr.cutDrops.Load())
		}
		if mgr.SendingTestCase("late-A", gapStart, gapEnd) {
			t.Fatal("SendingTestCase(late-A) = true; want false so late test case with capped drops is left out")
		}
		mgr.CloseOutChan()
	})

	t.Run("gap claims in sentClaimed survive prune even when Backlog reports clear so late two-goroutine parsers can still claim them", func(t *testing.T) {
		ch := make(chan *models.Mock, 16)
		mapCh := make(chan models.TestMockMapping, 16)
		mgr := New(nil)
		mgr.SetOutputChannel(ch)
		mgr.SetMappingChannel(context.Background(), mapCh)
		mgr.SetFirstRequestSignaled()
		mgr.SetWindowsOpenLate()
		mgr.resolvedTestCount = models.StartupMockTestCaseWindow

		wm := &backlogWatermark{}
		wm.pending.Store(false)
		mgr.SetWatermark(wm)

		// Timestamp strictly older than StaleHorizon so startup-claim retention
		// (openClaimsStartupLocked) expires; only c.gap keeps the claim alive.
		base := time.Now().Add(-StaleHorizon - 2*time.Second)
		w1 := mgr.OpenWindow(base, nil)
		mgr.ResolveKept(w1, base, base.Add(2*time.Millisecond), "t1", true)

		gapMock := gapMockAt(base.Add(5*time.Millisecond), base.Add(7*time.Millisecond))
		untakenGapMock := gapMockAt(base.Add(14*time.Millisecond), base.Add(15*time.Millisecond))
		mgr.AddMock(gapMock)
		mgr.AddMock(untakenGapMock)

		w2Start := base.Add(20 * time.Millisecond)
		w2 := mgr.OpenWindow(w2Start, nil)
		mgr.ResolveKept(w2, w2Start, w2Start.Add(5*time.Millisecond), "t2", true)
		if got := drainMocks(ch); len(got) != 2 {
			t.Fatalf("ResolveKept(w2) emitted %d mocks (%v); want 2 rescued and noted", len(got), got)
		}

		// Even with Backlog reporting PendingBefore=false, c.at older than
		// StaleHorizon, and no open window yet (e.g. a two-goroutine gRPC
		// parser whose response side ran ahead of request HEADERS),
		// FlushOwnedWindows must NOT prune either gap claim.
		mgr.FlushOwnedWindows()
		mgr.mu.Lock()
		n := len(mgr.sentClaimed)
		mgr.mu.Unlock()
		if n != 2 {
			t.Fatalf("sentClaimed = %d after prune with Backlog clear; want 2", n)
		}

		// When the late gRPC window finally opens and resolves kept over
		// gapMock ([base+4ms, base+12ms]), takeSentClaimedLocked maps gapMock
		// onto it and removes only gapMock's claim, leaving untakenGapMock's
		// claim in sentClaimed.
		wAStart := base.Add(4 * time.Millisecond)
		wA := mgr.OpenWindow(wAStart, nil)
		mgr.ResolveKept(wA, wAStart, base.Add(12*time.Millisecond), "late-grpc", true)
		var found bool
		for {
			select {
			case mp := <-mapCh:
				if mp.TestName == "late-grpc" {
					for _, id := range mp.MockIDs {
						if id == gapMock.Name {
							found = true
						}
					}
				}
			default:
				goto doneGrpcMap
			}
		}
	doneGrpcMap:
		if !found {
			t.Fatalf("late-grpc mapping did not claim retained gap mock %q", gapMock.Name)
		}
		mgr.mu.Lock()
		beforeReset := len(mgr.sentClaimed)
		mgr.mu.Unlock()
		if beforeReset != 1 {
			t.Fatalf("sentClaimed before recording reset = %d; want 1 (untakenGapMock)", beforeReset)
		}

		// Recording reset must clear the remaining untaken gap claim.
		mgr.EndRecording()
		mgr.ResumeRecording()
		mgr.mu.Lock()
		remaining := len(mgr.sentClaimed)
		mgr.mu.Unlock()
		if remaining != 0 {
			t.Fatalf("sentClaimed after EndRecording/ResumeRecording = %d; want 0", remaining)
		}
		mgr.CloseOutChan()
	})
}
