package manager

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// In proxyless / low-latency mode (SetWindowsOpenLate), a request's window may
// open only after its egress mock has already reached the manager. During the
// startup window, an earlier request's resolve or a flush tick rescues that
// buffered startup mock immediately while no window is open for the next
// request yet. The note must be kept for StaleHorizon so sequential requests
// (the alpha/beta/gamma/delta shape) each still map their own startup mock once
// their windows open and resolve.
func TestLateStartupClaimSequentialRequests(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 32)
	maps := make(chan models.TestMockMapping, 32)
	mgr := lateMockTestManager(out, maps, true)
	mgr.SetWindowsOpenLate()

	base := time.Now().Add(-500 * time.Millisecond)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }

	// beta is open; its own mock and gamma's egress mock are both in the buffer
	// before gamma's window has opened.
	wBeta := mgr.OpenWindow(at(0), nil)
	mBeta := httpMockAt(at(10))
	mGamma := httpMockAt(at(60))
	mgr.AddMock(mBeta)
	mgr.AddMock(mGamma)
	if !mBeta.TestModeInfo.IsStartup || !mGamma.TestModeInfo.IsStartup {
		t.Fatal("fixture: expected startup-tagged mocks")
	}

	// beta resolves and closes its window; m.open is now empty while mGamma was
	// rescued out of the buffer.
	wBeta.Keep()
	mgr.ResolveKept(wBeta, at(0), at(30), "get-kind-beta-1", true)
	wBeta.Close()
	if !hasMock(sentMocks(out), mGamma) {
		t.Fatal("fixture: beta's resolve did not rescue gamma's startup mock")
	}

	// A flush tick between beta closing and gamma opening must not prune mGamma.
	mgr.FlushOwnedWindows()

	// gamma opens late; delta's egress mock lands in the buffer before delta's
	// window opens.
	wGamma := mgr.OpenWindow(at(50), nil)
	mDelta := httpMockAt(at(110))
	mgr.AddMock(mDelta)
	wGamma.Keep()
	mgr.ResolveKept(wGamma, at(50), at(80), "get-kind-gamma-1", true)
	wGamma.Close()
	if !hasMock(sentMocks(out), mDelta) {
		t.Fatal("fixture: gamma's resolve did not rescue delta's startup mock")
	}

	// Another flush tick with m.open empty before delta opens.
	mgr.FlushOwnedWindows()

	wDelta := mgr.OpenWindow(at(100), nil)
	wDelta.Keep()
	mgr.ResolveKept(wDelta, at(100), at(130), "get-kind-delta-1", true)
	wDelta.Close()

	got := allMapped(maps)
	if !got["get-kind-beta-1"][mBeta.Name] || got["get-kind-beta-1"][mGamma.Name] || got["get-kind-beta-1"][mDelta.Name] {
		t.Fatalf("get-kind-beta-1 mapped %v, want only %q", got["get-kind-beta-1"], mBeta.Name)
	}
	if !got["get-kind-gamma-1"][mGamma.Name] || got["get-kind-gamma-1"][mBeta.Name] || got["get-kind-gamma-1"][mDelta.Name] {
		t.Fatalf("get-kind-gamma-1 mapped %v, want only %q", got["get-kind-gamma-1"], mGamma.Name)
	}
	if !got["get-kind-delta-1"][mDelta.Name] || got["get-kind-delta-1"][mBeta.Name] || got["get-kind-delta-1"][mGamma.Name] {
		t.Fatalf("get-kind-delta-1 mapped %v, want only %q", got["get-kind-delta-1"], mDelta.Name)
	}
	mgr.mu.Lock()
	left := len(mgr.sentClaimed)
	mgr.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d sentClaimed notes left after all late windows claimed their mocks, want 0", left)
	}
}

// Whichever pass rescues a recent startup mock before its request's window
// opens (SetWindowsOpenLate) — an earlier resolve, the flush ticker, a
// duplicate cycle's DeleteMocksStrictlyBefore, or a concurrent duplicate's
// ResolveRange — the late-opening request still maps that mock once it resolves.
func TestLateStartupClaimWhicheverPassRescues(t *testing.T) {
	t.Parallel()
	duplicate := func(mgr *SyncMockManager, start time.Time) {
		mgr.ResolveRange(start.Add(-5*time.Millisecond), start.Add(20*time.Millisecond), "test-0", false, true)
	}
	for _, tc := range []struct {
		name          string
		before, after func(mgr *SyncMockManager, start time.Time)
	}{
		{"earlier kept resolve", nil, func(mgr *SyncMockManager, start time.Time) {
			o := start.Add(-100 * time.Millisecond)
			mgr.ResolveRange(o, o.Add(10*time.Millisecond), "test-earlier", true, true)
		}},
		{"flush ticker", nil, func(mgr *SyncMockManager, _ time.Time) {
			mgr.FlushOwnedWindows()
		}},
		{"duplicate DeleteMocksStrictlyBefore", nil, func(mgr *SyncMockManager, start time.Time) {
			mgr.DeleteMocksStrictlyBefore(start.Add(50 * time.Millisecond))
		}},
		{"concurrent duplicate ResolveRange", nil, duplicate},
		{"flush after duplicate ResolveRange", duplicate, func(mgr *SyncMockManager, _ time.Time) {
			mgr.FlushOwnedWindows()
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := make(chan *models.Mock, 16)
			maps := make(chan models.TestMockMapping, 16)
			mgr := lateMockTestManager(out, maps, true)
			mgr.SetWindowsOpenLate()

			start := time.Now().Add(-300 * time.Millisecond)
			if tc.before != nil {
				tc.before(mgr, start)
			}
			call := httpMockAt(start.Add(10 * time.Millisecond))
			mgr.AddMock(call)
			tc.after(mgr, start)
			if !hasMock(sentMocks(out), call) {
				t.Fatal("fixture: rescue pass did not send the startup mock")
			}
			// A flush tick while m.open is still empty must keep the recent note.
			mgr.FlushOwnedWindows()

			w := mgr.OpenWindow(start, nil)
			w.Keep()
			mgr.ResolveKept(w, start, start.Add(30*time.Millisecond), "test-late", true)
			w.Close()
			if !mappedIDs(maps, "test-late")[call.Name] {
				t.Fatalf("test-late did not map its startup mock %q after %s", call.Name, tc.name)
			}
		})
	}
}

// Keeping a recent startup mock claimable under SetWindowsOpenLate must never
// attach it to the wrong test: a mock requested strictly between two late
// windows attaches to neither, while each window still maps only the mock
// whose request time falls within its own [start, end].
func TestLateStartupClaimBetweenTwoLateWindowsNeverAttachesToWrongTest(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := lateMockTestManager(out, maps, true)
	mgr.SetWindowsOpenLate()

	base := time.Now().Add(-500 * time.Millisecond)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }

	inW1 := httpMockAt(at(10))
	between := httpMockAt(at(50))
	inW2 := httpMockAt(at(90))
	mgr.AddMock(inW1)
	mgr.AddMock(between)
	mgr.AddMock(inW2)
	mgr.FlushOwnedWindows()

	if sent := sentMocks(out); !hasMock(sent, inW1) || !hasMock(sent, between) || !hasMock(sent, inW2) {
		t.Fatal("fixture: flush did not send all three startup mocks")
	}

	w1 := mgr.OpenWindow(at(0), nil)
	w1.Keep()
	mgr.ResolveKept(w1, at(0), at(30), "test-1", true)
	w1.Close()

	w2 := mgr.OpenWindow(at(70), nil)
	w2.Keep()
	mgr.ResolveKept(w2, at(70), at(110), "test-2", true)
	w2.Close()

	got := allMapped(maps)
	if !got["test-1"][inW1.Name] || got["test-1"][between.Name] || got["test-1"][inW2.Name] {
		t.Fatalf("test-1 mapped %v, want only %q", got["test-1"], inW1.Name)
	}
	if !got["test-2"][inW2.Name] || got["test-2"][between.Name] || got["test-2"][inW1.Name] {
		t.Fatalf("test-2 mapped %v, want only %q", got["test-2"], inW2.Name)
	}
	mgr.mu.Lock()
	left := len(mgr.sentClaimed)
	var leftName string
	if left == 1 {
		leftName = mgr.sentClaimed[0].name
	}
	mgr.mu.Unlock()
	if left != 1 || leftName != between.Name {
		t.Fatalf("sentClaimed left=%d (%q), want only unclaimed between mock %q", left, leftName, between.Name)
	}
}

// A true boot mock older than StaleHorizon must not linger in sentClaimed even
// when SetWindowsOpenLate is enabled — neither noted initially nor retained
// across pruning once any spanning open window closes.
func TestLateStartupClaimTrueBootMockAgesOut(t *testing.T) {
	t.Parallel()
	t.Run("not noted when already older than StaleHorizon", func(t *testing.T) {
		t.Parallel()
		out := make(chan *models.Mock, 16)
		maps := make(chan models.TestMockMapping, 16)
		mgr := lateMockTestManager(out, maps, true)
		mgr.SetWindowsOpenLate()

		old := time.Now().Add(-StaleHorizon - time.Second)
		boot := httpMockAt(old)
		mgr.AddMock(boot)
		// Rescue via an unrelated resolve (which does not run pruneSentClaimedLocked)
		// so we directly verify startupClaimedLocked itself does not note it.
		other := time.Now().Add(-100 * time.Millisecond)
		mgr.ResolveRange(other, other.Add(10*time.Millisecond), "test-other", true, true)
		if !hasMock(sentMocks(out), boot) {
			t.Fatal("fixture: boot mock was not sent")
		}
		mgr.mu.Lock()
		n := len(mgr.sentClaimed)
		mgr.mu.Unlock()
		if n != 0 {
			t.Fatalf("%d sentClaimed notes kept for a boot mock older than StaleHorizon, want 0", n)
		}
		mgr.ResolveRange(old.Add(-time.Millisecond), old.Add(time.Millisecond), "test-after-boot", true, true)
		if mappedIDs(maps, "test-after-boot")[boot.Name] {
			t.Fatal("boot mock older than StaleHorizon was attached to a later resolve")
		}
	})
	t.Run("pruned once older than StaleHorizon after open window closes", func(t *testing.T) {
		t.Parallel()
		out := make(chan *models.Mock, 16)
		maps := make(chan models.TestMockMapping, 16)
		mgr := lateMockTestManager(out, maps, true)
		mgr.SetWindowsOpenLate()

		old := time.Now().Add(-StaleHorizon - time.Second)
		w := mgr.OpenWindow(old, nil)
		boot := httpMockAt(old.Add(10 * time.Millisecond))
		mgr.AddMock(boot)
		mgr.FlushOwnedWindows()
		mgr.mu.Lock()
		notedCount := len(mgr.sentClaimed)
		mgr.mu.Unlock()
		if notedCount != 1 {
			t.Fatalf("fixture: %d sentClaimed notes while spanning open window exists, want 1", notedCount)
		}
		w.Close()
		mgr.FlushOwnedWindows()
		mgr.mu.Lock()
		afterPrune := len(mgr.sentClaimed)
		mgr.mu.Unlock()
		if afterPrune != 0 {
			t.Fatalf("%d sentClaimed notes left after open window closed and entry is older than StaleHorizon, want 0", afterPrune)
		}
		mgr.ResolveRange(old, old.Add(50*time.Millisecond), "test-after-prune", true, true)
		if mappedIDs(maps, "test-after-prune")[boot.Name] {
			t.Fatal("pruned boot mock was attached to a later resolve")
		}
	})
}

// When SetWindowsOpenLate has NOT been called, startup mocks with no spanning
// open window are neither noted nor retained across pruning, preserving the
// proxy-mode invariant unchanged.
func TestLateStartupClaimUnchangedWhenWindowsDoNotOpenLate(t *testing.T) {
	t.Parallel()
	t.Run("not noted when no window is open", func(t *testing.T) {
		t.Parallel()
		out := make(chan *models.Mock, 16)
		maps := make(chan models.TestMockMapping, 16)
		mgr := lateMockTestManager(out, maps, true)

		start := time.Now().Add(-100 * time.Millisecond)
		call := httpMockAt(start.Add(10 * time.Millisecond))
		mgr.AddMock(call)
		// Rescue via an unrelated resolve (which does not run pruneSentClaimedLocked)
		// so we directly verify startupClaimedLocked itself does not note it.
		other := start.Add(-50 * time.Millisecond)
		mgr.ResolveRange(other, other.Add(10*time.Millisecond), "test-other", true, true)
		if !hasMock(sentMocks(out), call) {
			t.Fatal("fixture: startup mock was not sent")
		}
		mgr.mu.Lock()
		n := len(mgr.sentClaimed)
		mgr.mu.Unlock()
		if n != 0 {
			t.Fatalf("%d sentClaimed notes kept without SetWindowsOpenLate, want 0", n)
		}
		w := mgr.OpenWindow(start, nil)
		w.Keep()
		mgr.ResolveKept(w, start, start.Add(30*time.Millisecond), "test-proxy", true)
		w.Close()
		if mappedIDs(maps, "test-proxy")[call.Name] {
			t.Fatal("proxy-mode resolve mapped a startup mock rescued while no window was open")
		}
	})
	t.Run("pruned immediately once open window closes", func(t *testing.T) {
		t.Parallel()
		out := make(chan *models.Mock, 16)
		maps := make(chan models.TestMockMapping, 16)
		mgr := lateMockTestManager(out, maps, true)

		start := time.Now().Add(-100 * time.Millisecond)
		w := mgr.OpenWindow(start, nil)
		call := httpMockAt(start.Add(10 * time.Millisecond))
		mgr.AddMock(call)
		mgr.FlushOwnedWindows()
		w.Close()
		mgr.FlushOwnedWindows()
		mgr.mu.Lock()
		n := len(mgr.sentClaimed)
		mgr.mu.Unlock()
		if n != 0 {
			t.Fatalf("%d sentClaimed notes left after open window closed without SetWindowsOpenLate, want 0", n)
		}
	})
}

// A capacity drop on a startup mock rescued before its request's window opens
// (SetWindowsOpenLate) must still revoke the test that later claims it and omit
// the dropped mock from that test's mapping.
func TestLateStartupClaimCapacityDropRevokesClaimingTest(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock) // no reader: send drops on budget
	maps := make(chan models.TestMockMapping, 16)
	mgr := lateMockTestManager(out, maps, true)
	mgr.SetWindowsOpenLate()
	stalledOutClock(mgr)

	start := time.Now().Add(-200 * time.Millisecond)
	call := httpMockAt(start.Add(10 * time.Millisecond))
	mgr.AddMock(call)
	mgr.FlushOwnedWindows()
	waitOutQueue(t, mgr)
	if mgr.DropCount() != 1 {
		t.Fatalf("fixture: %d mocks dropped, want 1", mgr.DropCount())
	}

	// An intermediate flush while m.open is still empty must keep the drop note.
	mgr.FlushOwnedWindows()

	w := mgr.OpenWindow(start, nil)
	w.Keep()
	mgr.ResolveKept(w, start, start.Add(30*time.Millisecond), "test-late-dropped", true)
	w.Close()

	if !mgr.WasMockDroppedForTC("test-late-dropped") {
		t.Fatal("test-late-dropped claimed a capacity-dropped startup mock and was not revoked")
	}
	if mappedIDs(maps, "test-late-dropped")[call.Name] {
		t.Fatalf("test-late-dropped mapping names %q, which was dropped", call.Name)
	}
}

func sentClaimCount(mgr *SyncMockManager) int {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	return len(mgr.sentClaimed)
}

// A mock well inside StaleHorizon (5.5 s old) must stay claimable across a
// flush tick and be mapped by its late window: pins the size of StaleHorizon
// from below so a tightened bound (e.g. 1 s) fails.
func TestLateStartupClaimHorizonNotTightened(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := lateMockTestManager(out, maps, true)
	mgr.SetWindowsOpenLate()

	start := time.Now().Add(-StaleHorizon + 1500*time.Millisecond)
	m := httpMockAt(start.Add(10 * time.Millisecond))
	mgr.AddMock(m)
	mgr.FlushOwnedWindows() // rescue + note
	mgr.FlushOwnedWindows() // prune pass with no window open

	w := mgr.OpenWindow(start, nil)
	w.Keep()
	mgr.ResolveKept(w, start, start.Add(30*time.Millisecond), "test-horizon", true)
	w.Close()
	if !mappedIDs(maps, "test-horizon")[m.Name] {
		t.Fatal("a 5.5 s old startup mock was not claimable by its late window")
	}
}

// Many startup mocks rescued with no window open: once they pass StaleHorizon a
// flush tick forgets every note (memory bounded by age, not only maxSentClaimed).
func TestLateStartupClaimNotesAgeOut(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 4096)
	maps := make(chan models.TestMockMapping, 16)
	mgr := lateMockTestManager(out, maps, true)
	mgr.SetWindowsOpenLate()

	// Just inside StaleHorizon now; outside it 300 ms later.
	old := time.Now().Add(-StaleHorizon + 200*time.Millisecond)
	for i := 0; i < 2000; i++ {
		mgr.AddMock(httpMockAt(old.Add(time.Duration(i) * time.Microsecond)))
	}
	mgr.FlushOwnedWindows()
	if n := sentClaimCount(mgr); n != 2000 {
		t.Fatalf("fixture: %d notes, want 2000", n)
	}
	time.Sleep(300 * time.Millisecond)
	mgr.FlushOwnedWindows()
	if n := sentClaimCount(mgr); n != 0 {
		t.Fatalf("%d notes left past StaleHorizon, want 0", n)
	}
}

// Boundaries of the [start, end] take in takeSentClaimedLocked: a mock 1 ns
// before start or 1 ns after end is not mapped; one exactly at start or exactly
// at end is mapped.
func TestLateStartupClaimWindowBoundaries(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := lateMockTestManager(out, maps, true)
	mgr.SetWindowsOpenLate()

	start := time.Now().Add(-500 * time.Millisecond)
	end := start.Add(30 * time.Millisecond)
	before := httpMockAt(start.Add(-time.Nanosecond))
	atStart := httpMockAt(start)
	atEnd := httpMockAt(end)
	after := httpMockAt(end.Add(time.Nanosecond))
	for _, m := range []*models.Mock{before, atStart, atEnd, after} {
		mgr.AddMock(m)
	}
	mgr.FlushOwnedWindows()
	mgr.FlushOwnedWindows()

	w := mgr.OpenWindow(start, nil)
	w.Keep()
	mgr.ResolveKept(w, start, end, "test-bounds", true)
	w.Close()

	got := mappedIDs(maps, "test-bounds")
	if got[before.Name] || got[after.Name] {
		t.Fatalf("mapped a mock outside [start,end]: %v", got)
	}
	if !got[atStart.Name] || !got[atEnd.Name] {
		t.Fatalf("did not map the mocks exactly at start/end: %v (start %q end %q)", got, atStart.Name, atEnd.Name)
	}
}

// The late-open StaleHorizon fallback applies only to owner-less notes: an
// owner-bound note (yielded-window return path, owner != nil) is not noted
// through the horizon clause when no other open window spans it.
func TestLateStartupClaimOwnerBoundNotNotedViaHorizon(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := lateMockTestManager(out, maps, true)
	mgr.SetWindowsOpenLate()

	now := time.Now()
	mk := httpMockAt(now.Add(-100 * time.Millisecond))
	mk.Name = "mock-owner-bound"
	owner := &resolvedWindow{start: now.Add(-time.Second), end: now, testName: "test-owner", mapping: true, keep: true}
	mgr.mu.Lock()
	noted := mgr.noteSentClaimedLocked(mk, nil, owner)
	n := len(mgr.sentClaimed)
	mgr.mu.Unlock()
	if noted || n != 0 {
		t.Fatalf("owner-bound mock noted=%v len=%d under SetWindowsOpenLate with no spanning open window, want false/0", noted, n)
	}
}

// Two late windows that both span one rescued startup mock: the first kept
// resolve takes it and the second never maps it (no double mapping), in either
// resolve order.
func TestLateStartupClaimOverlappingWindowsMapOnce(t *testing.T) {
	t.Parallel()
	for _, innerFirst := range []bool{true, false} {
		out := make(chan *models.Mock, 16)
		maps := make(chan models.TestMockMapping, 16)
		mgr := lateMockTestManager(out, maps, true)
		mgr.SetWindowsOpenLate()

		base := time.Now().Add(-500 * time.Millisecond)
		at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
		m := httpMockAt(at(50))
		mgr.AddMock(m)
		mgr.FlushOwnedWindows()
		mgr.FlushOwnedWindows()

		resolve := func(s, e int, name string) {
			w := mgr.OpenWindow(at(s), nil)
			w.Keep()
			mgr.ResolveKept(w, at(s), at(e), name, true)
			w.Close()
		}
		if innerFirst {
			resolve(40, 60, "inner")
			resolve(0, 100, "outer")
		} else {
			resolve(0, 100, "outer")
			resolve(40, 60, "inner")
		}
		got := allMapped(maps)
		n := 0
		for _, ids := range got {
			if ids[m.Name] {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("innerFirst=%v: mock mapped by %d tests (%v), want exactly 1", innerFirst, n, got)
		}
		if left := sentClaimCount(mgr); left != 0 {
			t.Fatalf("innerFirst=%v: %d notes left, want 0", innerFirst, left)
		}
	}
}

// A static-dedup duplicate's resolve over a rescued note does not take it; the
// kept late window that spans it still does.
func TestLateStartupClaimDuplicateDoesNotTake(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := lateMockTestManager(out, maps, true)
	mgr.SetWindowsOpenLate()

	base := time.Now().Add(-500 * time.Millisecond)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	m := httpMockAt(at(50))
	mgr.AddMock(m)
	mgr.FlushOwnedWindows()
	mgr.ResolveRange(at(40), at(60), "test-0", false, true)
	mgr.FlushOwnedWindows()

	w := mgr.OpenWindow(at(45), nil)
	w.Keep()
	mgr.ResolveKept(w, at(45), at(55), "test-kept", true)
	w.Close()

	got := allMapped(maps)
	if got["test-0"][m.Name] {
		t.Fatal("a duplicate's resolve mapped the note")
	}
	if !got["test-kept"][m.Name] {
		t.Fatalf("the kept late window did not map the note: %v", got)
	}
}

// An untaken owner-less note never holds a stop's or a test's wait: PendingIn
// over its range is false.
func TestLateStartupClaimPendingInIgnoresUntakenNote(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := lateMockTestManager(out, maps, true)
	mgr.SetWindowsOpenLate()

	start := time.Now().Add(-300 * time.Millisecond)
	m := httpMockAt(start.Add(10 * time.Millisecond))
	mgr.AddMock(m)
	mgr.FlushOwnedWindows()
	waitOutQueue(t, mgr)
	if n := sentClaimCount(mgr); n != 1 {
		t.Fatalf("fixture: %d notes, want 1", n)
	}
	if mgr.PendingIn(start, start.Add(30*time.Millisecond)) {
		t.Fatal("PendingIn waits on an untaken owner-less note")
	}
}
