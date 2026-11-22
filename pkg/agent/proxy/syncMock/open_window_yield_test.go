package manager

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// The synchronous ingress gives its lock back at the headers of a response of
// unknown length, and the next request runs beside the rest of it; its window
// yields there (Window.Yield). The window still runs to the response's last
// byte, so a call the app makes while it sends the body is its own; but a mock
// requested within another request's claim (from its start to its own yield)
// is that request's, as it was when the window ended at the headers: the kept
// resolve of the yielded window
// leaves it, whichever of the two is decided first, and a mock of the other
// decoded late is late-binned to it. The other request's window yields at its
// own last byte, so what the stream does after that is the stream's even while
// the other request's capture is not decided yet.
func TestYieldedWindowLeavesWhatAnotherRequestClaims(t *testing.T) {
	t.Parallel()
	for _, otherFirst := range []bool{false, true} {
		name := map[bool]string{false: "the stream is decided first", true: "the other request is decided first"}[otherFirst]
		t.Run(name, func(t *testing.T) {
			out := make(chan *models.Mock, 16)
			maps := make(chan models.TestMockMapping, 16)
			mgr := openWindowManager(out, maps)

			s0 := time.Now().Add(-2 * time.Second)
			stream := mgr.OpenWindow(s0, nil)
			own := httpMockAt(s0.Add(10 * time.Millisecond))  // before its headers
			stream.Yield(s0.Add(100 * time.Millisecond))      // the lock is given back at its headers
			gap := httpMockAt(s0.Add(150 * time.Millisecond)) // while it sends its body, and no other request is in flight
			next := mgr.OpenWindow(s0.Add(200*time.Millisecond), nil)
			nextOwn := httpMockAt(s0.Add(300 * time.Millisecond)) // the next request's, made within its claim
			after := httpMockAt(s0.Add(700 * time.Millisecond))   // the body goes on after the next request was answered
			for _, mk := range []*models.Mock{own, gap, nextOwn, after} {
				mgr.AddMock(mk)
			}
			nextEnd := s0.Add(600 * time.Millisecond)
			next.Yield(nextEnd) // the ingress, at the next request's last byte
			resolveNext := func() {
				if !next.Keep() {
					t.Fatal("the next request's window was given up")
				}
				mgr.ResolveKept(next, next.Start(), nextEnd, "test-next", true)
			}
			var nextLate *models.Mock
			if otherFirst {
				resolveNext()
				// Decoded after the next request was decided: late, and still its.
				nextLate = httpMockAt(s0.Add(350 * time.Millisecond))
				mgr.AddMock(nextLate)
			}
			if !stream.Keep() {
				t.Fatal("the stream's window was given up")
			}
			mgr.ResolveKept(stream, s0, s0.Add(time.Second), "test-stream", true)
			if !otherFirst {
				resolveNext()
			}
			// Decoded once both were decided: the next request's, though the
			// stream's window (resolved first, or not) spans it too.
			decidedLate := httpMockAt(s0.Add(320 * time.Millisecond))
			mgr.AddMock(decidedLate)
			mgr.FlushOwnedWindows()

			sent := sentMocks(out)
			for _, mk := range []*models.Mock{own, gap, nextOwn, after} {
				if !hasMock(sent, mk) {
					t.Fatalf("a mock was not recorded: %s", mk.Spec.ReqTimestampMock.Sub(s0))
				}
			}
			streamIDs, nextIDs := map[string]bool{}, map[string]bool{}
			for {
				select {
				case e := <-maps:
					for _, id := range e.MockIDs {
						switch e.TestName {
						case "test-stream":
							streamIDs[id] = true
						case "test-next":
							nextIDs[id] = true
						}
					}
					continue
				default:
				}
				break
			}
			for what, mk := range map[string]*models.Mock{"its own call before its headers": own, "a call made while it sent its body, with no other request in flight": gap, "a call made after the next request was answered": after} {
				if !streamIDs[mk.Name] || nextIDs[mk.Name] {
					t.Errorf("%s went to stream=%v next=%v, want the stream only", what, streamIDs[mk.Name], nextIDs[mk.Name])
				}
			}
			if !nextIDs[nextOwn.Name] || streamIDs[nextOwn.Name] {
				t.Errorf("the next request's own call went to next=%v stream=%v, want the next request only", nextIDs[nextOwn.Name], streamIDs[nextOwn.Name])
			}
			if !hasMock(sent, decidedLate) || !nextIDs[decidedLate.Name] || streamIDs[decidedLate.Name] {
				t.Errorf("the next request's mock decoded once both were decided: recorded=%v next=%v stream=%v, want it the next request's", hasMock(sent, decidedLate), nextIDs[decidedLate.Name], streamIDs[decidedLate.Name])
			}
			if nextLate != nil {
				if !hasMock(sent, nextLate) || !nextIDs[nextLate.Name] || streamIDs[nextLate.Name] {
					t.Errorf("the next request's late mock: recorded=%v next=%v stream=%v, want it the next request's", hasMock(sent, nextLate), nextIDs[nextLate.Name], streamIDs[nextLate.Name])
				}
			}
			if n := mgr.OpenWindows(); n != 0 {
				t.Fatalf("%d windows left open", n)
			}
		})
	}
}

// The same for what the hold keeps: a mock a reaper held for the requests in
// flight, requested within another request's claim, stays held for that
// request when the yielded window takes what was held for it.
func TestYieldedWindowLeavesAHeldMockToTheRequestThatClaimsIt(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	s0 := time.Now().Add(-3 * time.Second)
	stream := mgr.OpenWindow(s0, nil)
	own := httpMockAt(s0.Add(10 * time.Millisecond))
	stream.Yield(s0.Add(100 * time.Millisecond))
	gap := httpMockAt(s0.Add(150 * time.Millisecond))
	next := mgr.OpenWindow(s0.Add(200*time.Millisecond), nil)
	nextOwn := httpMockAt(s0.Add(300 * time.Millisecond))
	for _, mk := range []*models.Mock{own, gap, nextOwn} {
		mgr.AddMock(mk)
	}
	// A duplicate decided meanwhile: its prune holds all three for the
	// requests in flight.
	mgr.DeleteMocksStrictlyBefore(time.Now())
	if n := heldCount(mgr); n != 3 {
		t.Fatalf("fixture: %d mocks held, want 3", n)
	}

	stream.Keep()
	mgr.ResolveKept(stream, s0, s0.Add(time.Second), "test-stream", true)
	sent := sentMocks(out)
	if !hasMock(sent, own) || !hasMock(sent, gap) {
		t.Fatalf("the stream was recorded without what was held for it: own=%v gap=%v", hasMock(sent, own), hasMock(sent, gap))
	}
	if hasMock(sent, nextOwn) {
		t.Fatal("the stream took a held mock requested within the next request's claim")
	}
	if n := heldCount(mgr); n != 1 {
		t.Fatalf("%d mocks held after the stream was decided, want the next request's 1", n)
	}

	next.Keep()
	mgr.ResolveKept(next, next.Start(), s0.Add(600*time.Millisecond), "test-next", true)
	if !hasMock(sentMocks(out), nextOwn) {
		t.Fatal("the next request was recorded without its held mock")
	}
	if ids := mappedIDs(maps, "test-next"); !ids[nextOwn.Name] {
		t.Fatal("test-next's mapping lacks its mock")
	}
	if n := heldCount(mgr); n != 0 {
		t.Fatalf("%d mocks left held", n)
	}
}

// A mock that lands in a resolved yielded window's span after its resolve goes
// to it through the late paths (FlushOwnedWindows here) only where no request
// still in flight claims it: one requested within that request's claim waits
// for it.
func TestLateMockInAYieldedWindowWaitsForTheRequestThatClaimsIt(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	s0 := time.Now().Add(-2 * time.Second)
	stream := mgr.OpenWindow(s0, nil)
	stream.Yield(s0.Add(100 * time.Millisecond))
	next := mgr.OpenWindow(s0.Add(200*time.Millisecond), nil)
	stream.Keep()
	mgr.ResolveKept(stream, s0, s0.Add(time.Second), "test-stream", true)

	gapLate := httpMockAt(s0.Add(150 * time.Millisecond)) // the stream's, decoded after it was decided
	nextOwn := httpMockAt(s0.Add(300 * time.Millisecond)) // the next request's, still in flight
	mgr.AddMock(gapLate)
	mgr.AddMock(nextOwn)
	mgr.FlushOwnedWindows()
	sent := sentMocks(out)
	if !hasMock(sent, gapLate) {
		t.Fatal("the stream's late mock, which no other request claims, was not late-binned to it")
	}
	if hasMock(sent, nextOwn) {
		t.Fatal("a flush handed the stream a mock requested within the next request's claim, and the next request is still in flight")
	}
	if ids := mappedIDs(maps, "test-stream"); !ids[gapLate.Name] || ids[nextOwn.Name] {
		t.Fatalf("test-stream's mapping: late=%v next's=%v, want only its own", ids[gapLate.Name], ids[nextOwn.Name])
	}

	next.Keep()
	mgr.ResolveKept(next, next.Start(), s0.Add(600*time.Millisecond), "test-next", true)
	if !hasMock(sentMocks(out), nextOwn) {
		t.Fatal("the next request was recorded without its mock")
	}
}

// A window still open claims nothing after its own yield: a late mock in a
// resolved yielded window's shared part, made while another stream that had
// given the lock back earlier was still being sent, is late-binned to the
// resolved one (neither claims that time; the first decided takes it).
func TestLateMockInAYieldedWindowIsNotClaimedByAnotherThatYielded(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 16)
	maps := make(chan models.TestMockMapping, 16)
	mgr := openWindowManager(out, maps)

	s0 := time.Now().Add(-2 * time.Second)
	older := mgr.OpenWindow(s0.Add(-50*time.Millisecond), nil) // another stream, still being sent
	older.Yield(s0.Add(-40 * time.Millisecond))
	stream := mgr.OpenWindow(s0, nil)
	stream.Yield(s0.Add(100 * time.Millisecond))
	stream.Keep()
	mgr.ResolveKept(stream, s0, s0.Add(time.Second), "test-stream", true)

	late := httpMockAt(s0.Add(150 * time.Millisecond))
	mgr.AddMock(late)
	mgr.FlushOwnedWindows()
	if !hasMock(sentMocks(out), late) {
		t.Fatal("a late mock in the stream's shared part stayed behind for a window that had yielded before it")
	}
	if ids := mappedIDs(maps, "test-stream"); !ids[late.Name] {
		t.Fatal("test-stream's mapping lacks its late mock")
	}
	older.Close()
}

// What a yielded window's resolve leaves to a request that claims it goes back
// to it when that request ends without taking it, however it ends: not
// captured, a duplicate, given up (by memory pressure, or by the hold's budget;
// only a request something is held for is given up), or kept with the mock
// after its last byte (its yield came after the stream's resolve). Older or
// younger than the stale horizon, the stream's resolve holds it for the
// request that claims it, owed back to the stream; it is pending for the
// stream's test case until it is handed on, and then it is the stream's: never
// dropped as cleanup, which recorded the stream without it with nothing
// counted.
func TestYieldedWindowGetsBackWhatItsClaimantDidNotTake(t *testing.T) {
	t.Parallel()
	for _, ending := range []string{"not captured", "a duplicate", "given up by memory pressure", "given up by the hold's budget", "kept, the mock after its last byte"} {
		for _, held := range []bool{true, false} {
			name := ending + map[bool]string{true: "/old", false: "/young"}[held]
			t.Run(name, func(t *testing.T) {
				out := make(chan *models.Mock, 16)
				maps := make(chan models.TestMockMapping, 16)
				mgr := openWindowManager(out, maps)

				s0 := time.Now().Add(-2 * time.Second)
				if held {
					s0 = time.Now().Add(-20 * time.Second)
				}
				at := func(ms int) time.Time { return s0.Add(time.Duration(ms) * time.Millisecond) }
				stream := mgr.OpenWindow(s0, nil)
				stream.Yield(at(100))
				claimant := mgr.OpenWindow(at(200), nil)
				c := httpMockAt(at(300))
				if ending == "kept, the mock after its last byte" {
					c = httpMockAt(at(700))
				}
				mgr.AddMock(c)
				stream.Keep()
				mgr.ResolveKept(stream, s0, at(1000), "test-stream", true)
				if hasMock(sentMocks(out), c) {
					t.Fatal("fixture: the stream took a mock its claimant claimed")
				}
				if n := heldCount(mgr); n != 1 {
					t.Fatalf("the stream's resolve holds %d mocks for the request that claims them, want 1", n)
				}
				if !mgr.PendingIn(s0, at(1000)) {
					t.Fatal("the stream's test case is not held back by a mock that may yet be its")
				}

				switch ending {
				case "not captured":
					claimant.Close()
				case "a duplicate":
					claimant.Close()
					mgr.ResolveRange(at(200), at(600), "test-0", false, true)
				case "given up by memory pressure":
					mgr.SetMemoryPressure(true)
					mgr.SetMemoryPressure(false)
					if claimant.Keep() {
						t.Fatal("fixture: memory pressure did not give the claimant up")
					}
					claimant.Close()
				case "given up by the hold's budget":
					// Five quarters of the budget held for it.
					for i := 0; i < 5; i++ {
						mk := httpMockAt(at(400 + i))
						mk.Spec.HTTPResp = &models.HTTPResp{Body: megaBody}
						mgr.AddMock(mk)
					}
					mgr.DeleteMocksStrictlyBefore(time.Now())
					if claimant.Keep() {
						t.Fatal("fixture: the hold's budget did not give the claimant up")
					}
					claimant.Close()
				case "kept, the mock after its last byte":
					claimant.Yield(at(600))
					claimant.Keep()
					mgr.ResolveKept(claimant, at(200), at(600), "test-claimant", true)
				}
				if ending == "not captured" || ending == "given up by memory pressure" {
					// Owed now, handed on by the next flush: pending till then.
					if hasMock(sentMocks(out), c) || !mgr.PendingIn(s0, at(1000)) {
						t.Fatal("the stream's test case is not held back by the mock owed to it")
					}
				}
				mgr.FlushOwnedWindows() // the periodic flush

				if !hasMock(sentMocks(out), c) {
					t.Fatalf("the mock was not recorded once its claimant ended without it (held now: %d)", heldCount(mgr))
				}
				if ids := mappedIDs(maps, "test-stream"); !ids[c.Name] {
					t.Fatal("the mock went back, but not to the stream's test case")
				}
				if mgr.PendingIn(s0, at(1000)) {
					t.Fatal("the stream's test case is still held back once its mock was handed on")
				}
				if n := heldCount(mgr); n != 0 {
					t.Fatalf("%d mocks left held", n)
				}
			})
		}
	}
}

// What a yielded window gets back goes back to it as soon as no request in
// flight claims it, not once every request older than it has ended: a window
// still open that gave the lock back before the mock (a long stream being
// sent since before) claims nothing of it, and its own kept resolve would
// leave it. Held on behind such a window, the mock held the stream's test
// case back (PendingIn) for as long as that window stayed open, hours for a
// long stream, and counted against the hold's budgets, which could give that
// window up for mocks it could never take. A mock another request in flight
// still claims stays held for it, and the hold's counts follow. Whether the
// stream's resolve left the mocks in time or a late path found them later,
// young or old; and whether the first claimant ended without capture or was
// kept with the mock after its last byte.
func TestWhatAWindowGetsBackIsNotPinnedByAnOlderWindowThatCannotTakeIt(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"left in time", "decoded late, young", "decoded late, old"} {
		for _, ending := range []string{"not captured", "kept, the mock after its last byte"} {
			t.Run(how+"/"+ending, func(t *testing.T) {
				out := make(chan *models.Mock, 16)
				maps := make(chan models.TestMockMapping, 16)
				mgr := openWindowManager(out, maps)

				s0 := time.Now().Add(-2 * time.Second)
				if how == "decoded late, old" {
					s0 = time.Now().Add(-time.Minute)
				}
				at := func(ms int) time.Time { return s0.Add(time.Duration(ms) * time.Millisecond) }
				long := mgr.OpenWindow(at(-50), nil) // a long stream: it gave the lock back before the stream started
				long.Yield(at(-40))
				defer long.Close()
				stream := mgr.OpenWindow(s0, nil)
				stream.Yield(at(100))
				claimant := mgr.OpenWindow(at(200), nil)
				next := mgr.OpenWindow(at(400), nil) // the request after it, still in flight
				c := httpMockAt(at(300))             // within the claimant's claim only
				d := httpMockAt(at(500))             // within the next request's claim too
				if how == "left in time" {
					mgr.AddMock(c)
					mgr.AddMock(d)
				}
				stream.Keep()
				mgr.ResolveKept(stream, s0, at(1000), "test-stream", true)
				if how != "left in time" {
					mgr.AddMock(c)
					mgr.AddMock(d)
					mgr.FlushOwnedWindows()
				}
				if sent := sentMocks(out); hasMock(sent, c) || hasMock(sent, d) || heldCount(mgr) != 2 {
					t.Fatalf("fixture: the mocks are not held for the requests that claim them (held %d)", heldCount(mgr))
				}

				switch ending {
				case "not captured":
					claimant.Close()
				case "kept, the mock after its last byte":
					claimant.Yield(at(250))
					claimant.Keep()
					mgr.ResolveKept(claimant, at(200), at(600), "test-claimant", true)
				}
				mgr.FlushOwnedWindows() // the periodic flush

				sent := sentMocks(out)
				if !hasMock(sent, c) {
					t.Fatalf("the mock was not recorded once no request in flight claimed it: still held (%d) behind a window that cannot take it", heldCount(mgr))
				}
				if hasMock(sent, d) {
					t.Fatal("a mock the next request still claims was handed to the stream")
				}
				if ids := mappedIDs(maps, "test-stream"); !ids[c.Name] {
					t.Fatal("the mock went back, but not to the stream's test case")
				}
				if n := heldCount(mgr); n != 1 {
					t.Fatalf("%d mocks held, want the one the next request claims", n)
				}
				checkHoldCounts(t, mgr)

				next.Close() // not captured either
				mgr.FlushOwnedWindows()
				if !hasMock(sentMocks(out), d) {
					t.Fatal("the second mock was not recorded once the next request ended without it")
				}
				if mgr.PendingIn(s0, at(1000)) {
					t.Fatal("the stream's test case is held back while an unrelated window stays open")
				}
				if n := heldCount(mgr); n != 0 {
					t.Fatalf("%d mocks left held", n)
				}
				checkHoldCounts(t, mgr)
			})
		}
	}
}

// A window that yields stops claiming what comes after its yield there and
// then, not when it ends: what it was the last to claim there, held to go
// back to a window decided before (heldMock.owner), goes back at once. The
// synchronous loop stamps a stream's headers and only then yields, under the
// manager's lock; a call made after the stamp can be decoded, and an earlier
// stream decided, before the yield lands. That stream's resolve holds the
// call for the later stream, which still claims on, owed back to it; the
// yield then leaves it to nobody in flight. Every request after it starts
// after the call (the lock), so no window's end would look at it: kept held,
// it held the earlier stream's test case back (PendingIn) and counted against
// the hold's budgets until the later stream ended, hours for a long one.
func TestALateYieldGivesBackWhatItNoLongerClaims(t *testing.T) {
	t.Parallel()
	out := make(chan *models.Mock, 64)
	maps := make(chan models.TestMockMapping, 64)
	mgr := openWindowManager(out, maps)
	s0 := time.Now().Add(-2 * time.Second)
	at := func(ms int) time.Time { return s0.Add(time.Duration(ms) * time.Millisecond) }

	a := mgr.OpenWindow(s0, nil) // a stream
	a.Yield(at(100))             // its headers: the lock goes back
	c := mgr.OpenWindow(at(200), nil)
	defer c.Close()
	// The next stream's headers are stamped at +300; its yield has not landed.
	call := httpMockAt(at(310))
	mgr.AddMock(call)
	a.Keep()
	mgr.ResolveKept(a, s0, at(400), "test-a", true) // its last byte at +400
	if hasMock(sentMocks(out), call) || heldCount(mgr) != 1 {
		t.Fatalf("fixture: the call is not held for the stream that still claims it (held %d)", heldCount(mgr))
	}
	c.Yield(at(300)) // the yield lands, with the headers' stamp

	// Later requests come and go under the lock while the stream is sent.
	for i := 0; i < 2; i++ {
		d := mgr.OpenWindow(at(500+100*i), nil)
		d.Keep()
		mgr.ResolveKept(d, at(500+100*i), at(550+100*i), "test-d", false)
	}
	mgr.FlushOwnedWindows()
	if !hasMock(sentMocks(out), call) {
		t.Fatalf("a call no request in flight claims stayed held (%d) while the stream that yielded before it was sent", heldCount(mgr))
	}
	if ids := mappedIDs(maps, "test-a"); !ids[call.Name] {
		t.Fatal("the call went back, but not to the stream decided first over it")
	}
	if mgr.PendingIn(s0, at(400)) {
		t.Fatal("the first stream's test case is held back while the second is sent")
	}
	checkHoldCounts(t, mgr)
}

// checkHoldCounts fails t unless the hold's counts are the hold's: its size,
// what it published to its pool, and how many held mocks have an owner.
func checkHoldCounts(t *testing.T, mgr *SyncMockManager) {
	t.Helper()
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	var all int64
	shared := 0
	for _, h := range mgr.held {
		all += h.size
		if h.owner != nil {
			shared++
		}
	}
	if all != mgr.heldBytes || mgr.pooled != mgr.heldBytes || shared != mgr.heldShared {
		t.Fatalf("the hold counts %d bytes, published %d, %d with an owner; it holds %d, %d",
			mgr.heldBytes, mgr.pooled, mgr.heldShared, all, shared)
	}
}

// What a yielded window left is owed back to it however long the request that
// claimed it stays in flight: it is recorded with the mock, not looked up in
// the ring of resolved windows, which turns over every 8192 resolves.
func TestYieldedWindowGetsBackWhatItLeftAfterTheRingTurnsOver(t *testing.T) {
	t.Parallel()
	for _, other := range []bool{false, true} {
		for _, old := range []bool{true, false} {
			name := map[bool]string{true: "old", false: "young"}[old]
			if other {
				name += "/another stream still being sent"
			}
			t.Run(name, func(t *testing.T) {
				out := make(chan *models.Mock, 16)
				maps := make(chan models.TestMockMapping, 16)
				mgr := openWindowManager(out, maps)

				s0 := time.Now().Add(-2 * time.Second)
				if old {
					s0 = time.Now().Add(-time.Hour)
				}
				at := func(ms int) time.Time { return s0.Add(time.Duration(ms) * time.Millisecond) }
				var older *Window
				if other {
					// Being sent since before the stream; it gave the lock
					// back before the stream started, so it claims nothing
					// of the stream's, and is decided last.
					older = mgr.OpenWindow(at(-50), nil)
					older.Yield(at(-40))
				}
				stream := mgr.OpenWindow(s0, nil)
				stream.Yield(at(100))
				upload := mgr.OpenWindow(at(200), nil) // a long upload: it claims on
				c := httpMockAt(at(300))
				mgr.AddMock(c)
				stream.Keep()
				mgr.ResolveKept(stream, s0, at(1000), "test-stream", true)
				if !streamInRingBehindAClaim(mgr, c) {
					t.Fatal("fixture: the ring does not hold the stream's window, or the upload does not claim the mock")
				}

				// The ring turns over while the upload is in flight, the
				// resolves an hour on from the mock.
				turnRingOver(mgr)
				if streamInRingBehindAClaim(mgr, c) {
					t.Fatal("fixture: the stream's window is still in the ring")
				}
				upload.Close() // not captured
				if other {
					older.Keep()
					mgr.ResolveKept(older, at(-50), at(2000), "test-older", true)
				}
				mgr.FlushOwnedWindows()
				if !hasMock(sentMocks(out), c) {
					t.Fatal("the mock the stream left was dropped once the ring had let go of the stream's window")
				}
				ids := map[string]string{}
				for len(maps) > 0 {
					e := <-maps
					for _, id := range e.MockIDs {
						ids[id] = e.TestName
					}
				}
				if ids[c.Name] != "test-stream" {
					t.Fatalf("the mock went to %q, want the stream's test case, which left it", ids[c.Name])
				}
			})
		}
	}
}

// streamInRingBehindAClaim reports whether a resolved kept window in the ring
// covers mk in its part after its yield while a window still open claims it
// (ownerOrSharedLocked's behind): what a late path asks of the ring.
func streamInRingBehindAClaim(mgr *SyncMockManager, mk *models.Mock) bool {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	_, _, _, behind := mgr.ownerOrSharedLocked(mk.Spec.ReqTimestampMock)
	return behind
}

// turnRingOver resolves maxRecentWindows+1 kept windows an hour on from now,
// so the ring of resolved windows lets go of every window resolved before.
func turnRingOver(mgr *SyncMockManager) {
	k0 := time.Now().Add(time.Hour)
	for i := 0; i <= maxRecentWindows; i++ {
		k := k0.Add(time.Duration(i) * time.Millisecond)
		mgr.ResolveRange(k, k.Add(500*time.Microsecond), "test-k", true, false)
	}
}

// A mock decoded after a yielded window was decided, requested in its part
// after its yield while another request claims that time, goes back to it as
// one its resolve left in time does: the first late path that finds it (the
// periodic flush, or any resolve) holds it for the request that claims it,
// recorded with the window it goes back to, young as it is. Kept in the buffer
// instead, it was looked up in the ring of resolved windows again on every
// pass; once 8192 resolves had turned the ring over, the claimant ending
// without it left it to nobody (it stayed buffered until the stale cutoff
// dropped it), and a stream decided later that spans it too took it.
func TestYieldedWindowGetsBackALateMockAfterTheRingTurnsOver(t *testing.T) {
	t.Parallel()
	for _, first := range []string{"a flush", "a resolve"} {
		for _, later := range []bool{false, true} {
			name := "found first by " + first
			if later {
				name += "/another stream decided after the ring turned over"
			}
			t.Run(name, func(t *testing.T) {
				out := make(chan *models.Mock, 16)
				maps := make(chan models.TestMockMapping, 16)
				mgr := openWindowManager(out, maps)

				s0 := time.Now().Add(-2 * time.Second)
				at := func(ms int) time.Time { return s0.Add(time.Duration(ms) * time.Millisecond) }
				var e *Window
				if later {
					// Being sent since before the stream, and given the lock
					// back before it started: its part after its yield spans
					// the mock too, and it is decided after the stream.
					e = mgr.OpenWindow(at(-50), nil)
					e.Yield(at(-40))
				}
				stream := mgr.OpenWindow(s0, nil)
				stream.Yield(at(100))
				upload := mgr.OpenWindow(at(200), nil) // a long upload: it claims on
				stream.Keep()
				mgr.ResolveKept(stream, s0, at(1000), "test-stream", true)
				c := httpMockAt(at(300)) // decoded after the stream was decided
				mgr.AddMock(c)
				if !streamInRingBehindAClaim(mgr, c) {
					t.Fatal("fixture: the ring does not hold the stream's window, or the upload does not claim the mock")
				}
				switch first {
				case "a flush":
					mgr.FlushOwnedWindows()
				case "a resolve":
					k := time.Now().Add(time.Hour)
					mgr.ResolveRange(k, k.Add(500*time.Microsecond), "test-k0", true, false)
				}
				if hasMock(sentMocks(out), c) {
					t.Fatal("fixture: a late path handed the stream a mock the upload claims, and the upload is still in flight")
				}

				turnRingOver(mgr)
				if streamInRingBehindAClaim(mgr, c) {
					t.Fatal("fixture: the stream's window is still in the ring")
				}
				if later {
					e.Keep()
					mgr.ResolveKept(e, at(-50), at(2000), "test-e", true)
				}
				upload.Close() // not captured
				mgr.FlushOwnedWindows()

				if !hasMock(sentMocks(out), c) {
					t.Fatalf("the late mock was not recorded once its claimant ended without it, the ring having let go of the stream's window (held %d)", heldCount(mgr))
				}
				ids := map[string]string{}
				for len(maps) > 0 {
					m := <-maps
					for _, id := range m.MockIDs {
						ids[id] = m.TestName
					}
				}
				if ids[c.Name] != "test-stream" {
					t.Fatalf("the late mock went to %q, want the stream's test case, decided first over it", ids[c.Name])
				}
				if n := heldCount(mgr); n != 0 {
					t.Fatalf("%d mocks left held", n)
				}
			})
		}
	}
}

// Two yielded windows span a mock no request in flight claims any more: the
// one decided first has it, by the in-time path as by the late ones. Stream E
// yields, stream A yields, B takes the lock and c is made while it holds it; A
// is decided first and leaves c to B; B ends without it (not captured); E is
// decided last. c goes back to A, which was held back for it (PendingIn), not
// to E, which a late copy of c would not go to either.
func TestWhatAWindowLeftGoesBackToItNotToOneDecidedAfter(t *testing.T) {
	t.Parallel()
	for _, held := range []bool{true, false} {
		t.Run(map[bool]string{true: "old", false: "young"}[held], func(t *testing.T) {
			out := make(chan *models.Mock, 16)
			maps := make(chan models.TestMockMapping, 16)
			mgr := openWindowManager(out, maps)

			s0 := time.Now().Add(-2 * time.Second)
			if held {
				s0 = time.Now().Add(-20 * time.Second)
			}
			at := func(ms int) time.Time { return s0.Add(time.Duration(ms) * time.Millisecond) }
			e := mgr.OpenWindow(s0, nil)
			e.Yield(at(50))
			a := mgr.OpenWindow(at(60), nil)
			a.Yield(at(100))
			b := mgr.OpenWindow(at(200), nil)
			c := httpMockAt(at(300))
			mgr.AddMock(c)

			a.Keep()
			mgr.ResolveKept(a, at(60), at(1000), "test-a", true)
			if !mgr.PendingIn(at(60), at(1000)) {
				t.Fatal("fixture: A is not held back for the mock it left to B")
			}
			b.Close() // not captured
			e.Keep()
			mgr.ResolveKept(e, s0, at(1000), "test-e", true)
			mgr.FlushOwnedWindows()

			if !hasMock(sentMocks(out), c) {
				t.Fatal("the mock was not recorded")
			}
			aIDs, eIDs := map[string]bool{}, map[string]bool{}
			for len(maps) > 0 {
				m := <-maps
				for _, id := range m.MockIDs {
					switch m.TestName {
					case "test-a":
						aIDs[id] = true
					case "test-e":
						eIDs[id] = true
					}
				}
			}
			if !aIDs[c.Name] || eIDs[c.Name] {
				t.Fatalf("the mock went to A=%v E=%v, want A, decided first, only", aIDs[c.Name], eIDs[c.Name])
			}
			if mgr.PendingIn(at(60), at(1000)) {
				t.Fatal("A is still held back once its mock was handed on")
			}
		})
	}
}

// What other requests claim, merged: open windows from their start to their
// yield (unbounded while they have not yielded), resolved kept windows over
// all of their span, the part after their yield too (decided first); nothing
// that an open window had given up before the span asked about, nor a resolved
// duplicate's.
func TestClaimsMergeWhatOtherRequestsHeldTheLockFor(t *testing.T) {
	t.Parallel()
	mgr := openWindowManager(make(chan *models.Mock, 1), make(chan models.TestMockMapping, 1))
	s0 := time.Now().Add(-time.Minute)
	at := func(ms int) time.Time { return s0.Add(time.Duration(ms) * time.Millisecond) }

	self := mgr.OpenWindow(at(0), nil)
	self.Yield(at(100))
	mgr.OpenWindow(at(200), nil).Yield(at(300)) // claims [200, 300]
	mgr.OpenWindow(at(250), nil).Yield(at(400)) // overlaps it: [200, 400]
	mgr.OpenWindow(at(50), nil).Yield(at(90))   // yielded before 100: claims nothing after it
	mgr.OpenWindow(at(900), nil)                // not yielded: [900, ...)
	mgr.ResolveRange(at(500), at(600), "k", true, false)
	mgr.ResolveRange(at(650), at(700), "d", false, false)  // a duplicate claims nothing
	mgr.ResolveRange(at(950), at(1000), "k2", true, false) // inside the unbounded claim from 900
	mgr.mu.Lock()
	mgr.recentWindows = append(mgr.recentWindows, resolvedWindow{start: at(720), end: at(850), keep: true, sharedFrom: at(760).UnixNano()}) // all of it
	c := mgr.claimsLocked(self, at(100), at(2000))
	mgr.mu.Unlock()

	for ms, want := range map[int]bool{
		95: false, 150: false, 200: true, 299: true, 350: true, 400: true, 401: false,
		500: true, 600: true, 620: false, 680: false, 720: true, 760: true, 800: true,
		850: true, 851: false, 899: false, 900: true, 975: true, 1200: true, 5000: true,
	} {
		if got := c.covers(at(ms)); got != want {
			t.Errorf("covers(%dms) = %v, want %v", ms, got, want)
		}
	}
}

// What a yielded window's kept resolve costs over one that did not yield: one
// pass over the open windows and the resolved ones for the claims (a full ring
// of 8192 here, the last 1000 of them, 0.5 ms each, inside the window after its
// yield), sorting what it finds, and a binary search for each mock after its
// yield (100, in the gaps between those windows, all taken).
func BenchmarkYieldedKeptResolve(b *testing.B) {
	for _, yielded := range []bool{false, true} {
		b.Run(map[bool]string{false: "notYielded", true: "yielded"}[yielded], func(b *testing.B) {
			out := make(chan *models.Mock, 256)
			mgr := openWindowManager(out, make(chan models.TestMockMapping, 1))
			s0 := time.Now().Add(-time.Hour)
			for i := 0; i < maxRecentWindows; i++ {
				at := s0.Add(time.Duration(i) * time.Millisecond)
				mgr.ResolveRange(at, at.Add(500*time.Microsecond), "r", true, false)
			}
			mgr.mu.Lock()
			ring := append([]resolvedWindow(nil), mgr.recentWindows...)
			mgr.mu.Unlock()
			start := s0.Add(time.Duration(maxRecentWindows-1000) * time.Millisecond)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				mgr.mu.Lock()
				mgr.recentWindows = append(mgr.recentWindows[:0], ring...)
				mgr.mu.Unlock()
				w := mgr.OpenWindow(start, nil)
				if yielded {
					w.Yield(start.Add(100 * time.Microsecond))
				}
				for j := 0; j < 100; j++ {
					mgr.AddMock(httpMockAt(start.Add(time.Duration(j)*10*time.Millisecond + 700*time.Microsecond)))
				}
				w.Claim()
				b.StartTimer()
				mgr.ResolveKept(w, start, start.Add(time.Second), "s", false)
				b.StopTimer()
				if n := len(sentMocks(out)); n != 100 {
					b.Fatalf("the resolve took %d mocks, want 100", n)
				}
				b.StartTimer()
			}
		})
	}
}

// What a window's end costs with a large hold: 30,000 mocks held for a long
// stream still in flight (a duplicate flood's leftovers), with no held mock
// that has an owner, and with one that a request in flight claims. While one
// has an owner, a window's end looks at the held mocks requested since that
// window started for those no open window claims any more
// (oweUnclaimedLocked), not at the whole hold.
func BenchmarkWindowEndWithTheHoldFull(b *testing.B) {
	for _, owned := range []bool{false, true} {
		b.Run(map[bool]string{false: "noOwnerHeld", true: "oneOwnerHeld"}[owned], func(b *testing.B) {
			mgr := openWindowManager(make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16))
			s0 := time.Now().Add(-time.Minute)
			long := mgr.OpenWindow(s0.Add(-time.Second), nil) // the long stream the hold is kept for
			defer long.Close()
			mgr.mu.Lock()
			for i := 0; i < 30000; i++ {
				mgr.holdLocked(heldMock{mock: httpMockAt(s0.Add(time.Duration(i) * time.Microsecond)), size: 2048})
			}
			if owned {
				mgr.holdLocked(heldMock{mock: httpMockAt(s0.Add(time.Second)), size: 2048, owner: &resolvedWindow{keep: true}})
			}
			mgr.mu.Unlock()
			claimant := mgr.OpenWindow(s0.Add(500*time.Millisecond), nil) // claims the one with an owner
			defer claimant.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w := mgr.OpenWindow(time.Now(), nil)
				w.Close()
			}
			b.StopTimer()
			if n := heldCount(mgr); n != 30000+map[bool]int{false: 0, true: 1}[owned] {
				b.Fatalf("%d held, want the hold untouched", n)
			}
		})
	}
}

// The owner lookup the late paths make for every buffered mock allocates
// nothing: it returns the windows by value, and copies none out. A mock held
// behind a claim gets the record of the window it goes back to from the pass
// that holds it (TestMocksHeldBehindAClaimShareOneRecord).
func TestOwnerLookupAllocatesNothing(t *testing.T) {
	mgr := openWindowManager(make(chan *models.Mock, 1), make(chan models.TestMockMapping, 1))
	s0 := time.Now().Add(-time.Minute)
	for i := 0; i < 64; i++ {
		k := s0.Add(time.Duration(i) * time.Millisecond)
		mgr.ResolveRange(k, k.Add(500*time.Microsecond), "k", i%2 == 0, false)
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	for _, at := range []time.Time{s0.Add(10 * time.Millisecond), s0.Add(11 * time.Millisecond), s0.Add(10*time.Millisecond + 700*time.Microsecond)} {
		if n := testing.AllocsPerRun(100, func() { mgr.ownerOrSharedLocked(at) }); n != 0 {
			t.Fatalf("the owner lookup allocates %.0f times", n)
		}
	}
}

// The mocks a pass holds behind a claim, for the window they go back to
// (heldMock.owner), share one record of that window: the mocks a yielded
// resolve leaves to the request that claims them, and those a late path finds
// behind a claim (decoded after the window was decided, young as they are).
// So what the records cost is one per window a pass holds mocks for, not one
// per mock. Once held, such a mock is not looked at again: a later flush
// allocates nothing for it.
func TestMocksHeldBehindAClaimShareOneRecord(t *testing.T) {
	for _, found := range []string{"left by the resolve", "found late by a flush"} {
		t.Run(found, func(t *testing.T) {
			mgr := openWindowManager(make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16))
			s0 := time.Now().Add(-2 * time.Second)
			at := func(ms int) time.Time { return s0.Add(time.Duration(ms) * time.Millisecond) }
			stream := mgr.OpenWindow(s0, nil)
			stream.Yield(at(100))
			claimant := mgr.OpenWindow(at(200), nil)
			defer claimant.Close()
			addTen := func() {
				for i := 0; i < 10; i++ {
					mgr.AddMock(httpMockAt(at(300 + i))) // young, within the claimant's claim
				}
			}
			if found == "left by the resolve" {
				addTen()
			}
			stream.Keep()
			mgr.ResolveKept(stream, s0, at(1000), "test-stream", true)
			if found == "found late by a flush" {
				addTen()
				mgr.FlushOwnedWindows()
			}

			mgr.mu.Lock()
			records := map[*resolvedWindow]bool{}
			for _, h := range mgr.held {
				if h.owner != nil {
					records[h.owner] = true
					if h.owner.testName != "test-stream" {
						t.Errorf("a held mock goes back to %q, want test-stream", h.owner.testName)
					}
				}
			}
			held, buffered := len(mgr.held), len(mgr.buffer)
			mgr.mu.Unlock()
			if held != 10 || buffered != 0 {
				t.Fatalf("%d held and %d buffered, want all 10 held for the claimant, with the window they go back to", held, buffered)
			}
			if len(records) != 1 {
				t.Fatalf("the 10 held mocks carry %d records of the window they go back to, want 1", len(records))
			}
			if n := testing.AllocsPerRun(20, mgr.FlushOwnedWindows); n != 0 {
				t.Fatalf("a flush with 10 mocks held behind a claim allocates %.0f times", n)
			}
		})
	}
}
