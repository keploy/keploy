package manager

import (
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/stall"
)

// unreadAt is a per-test answer at at; tagged as one the client never read
// (models.Mock.IsUndelivered) when unread.
func unreadAt(at time.Time, unread bool) *models.Mock {
	m := httpMockAt(at)
	if unread {
		m.MarkUndelivered()
	}
	return m
}

// eachProvenSwitch runs row with the proven switch off, then on.
func eachProvenSwitch(t *testing.T, row func(t *testing.T)) {
	for _, on := range []string{"0", "1"} {
		t.Run(provenswitch.Env+"="+on, func(t *testing.T) {
			t.Setenv(provenswitch.Env, on)
			row(t)
		})
	}
}

// A --sync recording lists no answer the client never read in a test's
// mapping, under either switch, on any path that maps: its window owns it and
// sends it, named as its other mocks are, while the answer read beside it is
// mapped; a duplicate's window prunes both. The paths: the window's own
// resolve; a late decode found by a kept resolve, by a duplicate's (the
// owner's late bin, and the retro bin), by the flush and by the prune; what
// was held for a kept window in flight, which its resolve takes; what was
// held for a request that ended without it, owed back to the kept window it
// lies in; and a startup answer a request in flight claims. Mapped, the
// report would list it as a call the test was expected to make and did not.
func TestASyncMappingListsNoAnswerTheClientNeverRead(t *testing.T) {
	eachProvenSwitch(t, func(t *testing.T) {
		// The window's own resolve.
		for _, keep := range []bool{true, false} {
			out := make(chan *models.Mock, 16)
			maps := make(chan models.TestMockMapping, 16)
			mgr := lateMockTestManager(out, maps, false)
			at := time.Now().Add(-time.Second)
			read, unread := unreadAt(at, false), unreadAt(at, true)
			mgr.AddMock(read)
			mgr.AddMock(unread)
			mgr.ResolveRange(at.Add(-time.Millisecond), at.Add(time.Millisecond), "test-1", keep, true)
			sent, mapped := sentMocks(out), mappedIDs(maps, "test-1")
			switch {
			case keep && (!hasMock(sent, read) || !hasMock(sent, unread) || !mapped[read.Name] || len(mapped) != 1):
				t.Fatalf("a kept window sent %d mocks and mapped %v; want both sent, the answer read (%s) alone mapped", len(sent), mapped, read.Name)
			case !keep && (len(sent) != 0 || len(mapped) != 0):
				t.Fatalf("a duplicate's window sent %d mocks and mapped %v; want both pruned with its other mocks", len(sent), mapped)
			}
		}
		// A late decode, found by each path that finds one, the resolves by a
		// kept request's and by a duplicate's: a kept resolve whose window
		// contains it takes it as its own, a duplicate's late-bins it to the
		// kept window that owns it.
		for _, path := range []string{"ResolveRange", "ResolveRange, window containing it", "FlushOwnedWindows", "DeleteMocksStrictlyBefore"} {
			for _, finderKeep := range []bool{true, false} {
				out := make(chan *models.Mock, 16)
				maps := make(chan models.TestMockMapping, 16)
				mgr := lateMockTestManager(out, maps, false)
				base := time.Now().Add(-time.Second)
				mgr.ResolveRange(base, base.Add(5*time.Millisecond), "test-1", true, true)
				read, unread := unreadAt(base.Add(2*time.Millisecond), false), unreadAt(base.Add(2*time.Millisecond), true)
				mgr.AddMock(read)
				mgr.AddMock(unread)
				findLateMock(mgr, base, path, finderKeep)
				sent, mapped := sentMocks(out), anyMapped(maps)
				if !hasMock(sent, read) || !hasMock(sent, unread) || !mapped[read.Name] || mapped[unread.Name] {
					t.Fatalf("%s, finder kept %v: sent %d mocks and mapped %v; want both sent, the answer read (%s) mapped, the one never read (%s) not", path, finderKeep, len(sent), mapped, read.Name, unread.Name)
				}
			}
		}
		// Held for a kept window in flight (a duplicate's prune held both
		// while it was open): its resolve takes both.
		{
			out := make(chan *models.Mock, 16)
			maps := make(chan models.TestMockMapping, 16)
			mgr := openWindowManager(out, maps)
			s0 := time.Now().Add(-3 * time.Second)
			w := mgr.OpenWindow(s0, nil)
			read, unread := unreadAt(s0.Add(10*time.Millisecond), false), unreadAt(s0.Add(10*time.Millisecond), true)
			mgr.AddMock(read)
			mgr.AddMock(unread)
			mgr.DeleteMocksStrictlyBefore(time.Now())
			if n := heldCount(mgr); n != 2 {
				t.Fatalf("fixture: %d mocks held for the window in flight, want 2", n)
			}
			w.Keep()
			mgr.ResolveKept(w, s0, s0.Add(time.Second), "test-1", true)
			w.Close()
			sent, mapped := sentMocks(out), mappedIDs(maps, "test-1")
			if !hasMock(sent, read) || !hasMock(sent, unread) || !mapped[read.Name] || mapped[unread.Name] {
				t.Fatalf("held for a kept window: sent %d mocks and mapped %v; want both sent, the answer read (%s) mapped, the one never read (%s) not", len(sent), mapped, read.Name, unread.Name)
			}
		}
		// Owed: held for a request in flight after a kept window's yield,
		// in that window's part after it; the request ends without them, so
		// they are owed back to the kept window, and the flush hands them on.
		{
			out := make(chan *models.Mock, 16)
			maps := make(chan models.TestMockMapping, 16)
			mgr := openWindowManager(out, maps)
			start := time.Now().Add(-3 * time.Second)
			k := mgr.OpenWindow(start, nil)
			k.Yield(start.Add(5 * time.Millisecond))
			other := mgr.OpenWindow(start.Add(8*time.Millisecond), nil)
			read, unread := unreadAt(start.Add(10*time.Millisecond), false), unreadAt(start.Add(10*time.Millisecond), true)
			mgr.AddMock(read)
			mgr.AddMock(unread)
			k.Keep()
			mgr.ResolveKept(k, start, start.Add(20*time.Millisecond), "test-k", true)
			k.Close()
			mgr.mu.Lock()
			heldForK := len(mgr.held) == 2 && mgr.held[0].owner != nil && mgr.held[1].owner != nil
			mgr.mu.Unlock()
			if !heldForK {
				t.Fatal("fixture: the two answers are not held for the request in flight, owed back to test-k")
			}
			other.Close()
			mgr.mu.Lock()
			owed := len(mgr.owed)
			mgr.mu.Unlock()
			if owed != 2 {
				t.Fatalf("fixture: %d answers owed back to test-k once the request ended, want 2", owed)
			}
			mgr.FlushOwnedWindows()
			sent, mapped := sentMocks(out), mappedIDs(maps, "test-k")
			if !hasMock(sent, read) || !hasMock(sent, unread) || !mapped[read.Name] || mapped[unread.Name] {
				t.Fatalf("owed to a kept window: sent %d mocks and mapped %v; want both sent, the answer read (%s) mapped, the one never read (%s) not", len(sent), mapped, read.Name, unread.Name)
			}
		}
		// A startup answer sent while a request was in flight: that
		// request's resolve takes the answer read into its mapping, not the
		// one never read.
		out := make(chan *models.Mock, 16)
		maps := make(chan models.TestMockMapping, 16)
		mgr := lateMockTestManager(out, maps, true)
		mgr.resolvedTestCount = 2
		start := time.Now().Add(-time.Second)
		win := mgr.OpenWindow(start, nil)
		read, unread := unreadAt(start.Add(10*time.Millisecond), false), unreadAt(start.Add(10*time.Millisecond), true)
		mgr.AddMock(read)
		mgr.AddMock(unread)
		if !read.TestModeInfo.IsStartup || !unread.TestModeInfo.IsStartup {
			t.Fatal("fixture: the answers are not startup mocks")
		}
		mgr.FlushOwnedWindows()
		if sent := sentMocks(out); !hasMock(sent, read) || !hasMock(sent, unread) {
			t.Fatalf("the flush sent %d of the two startup answers; want both", len(sent))
		}
		win.Keep()
		mgr.ResolveKept(win, start, time.Now(), "test-3", true)
		win.Close()
		if mapped := mappedIDs(maps, "test-3"); !mapped[read.Name] || mapped[unread.Name] {
			t.Fatalf("test-3's mapping %v; want the answer read (%s), not the one never read (%s)", mapped, read.Name, unread.Name)
		}
	})
}

// anyMapped is every name the mappings sent on maps list, whichever test's.
func anyMapped(maps chan models.TestMockMapping) map[string]bool {
	ids := map[string]bool{}
	for {
		select {
		case e := <-maps:
			for _, id := range e.MockIDs {
				ids[id] = true
			}
		default:
			return ids
		}
	}
}

// A drop of an answer the client never read suppresses no test case, under
// either switch: no test needs it, so the test case its window owns is still
// recorded, and OnDrop is not told of it, so a capture that leaves out the
// test cases that ran while a dropped mock was requested (enterprise's
// proxyless capture wires OnDrop to RecordOrphanWindow over its span) leaves
// out none for it either. The answer read beside it, dropped the same way,
// suppresses its test case, by name and by its span, as before. Every drop
// path: the output channel already closed, the byte bound of what waits
// behind another, a full channel whose consumer stalls, and AddMock after the
// channel closed (a connection that hands its last answer over at the
// recording's teardown). AddMock takes no owner, so on that path the answer
// read is told and leaves out the test cases inside its span, and none is
// suppressed by name.
func TestADroppedAnswerTheClientNeverReadSuppressesNoTestCase(t *testing.T) {
	eachProvenSwitch(t, func(t *testing.T) {
		for _, unread := range []bool{true, false} {
			for _, path := range []string{"channel closed", "byte bound", "consumer stalled", addedAfterClose} {
				at := time.Now().Add(-time.Second)
				mk := unreadAt(at, unread)
				mk.Spec.ResTimestampMock = at.Add(500 * time.Millisecond) // a long poll's span
				var told atomic.Int32
				mgr := droppedOn(t, path, mk, "test-7", func(mgr *SyncMockManager) {
					// As enterprise's proxyless capture wires it.
					mgr.OnDrop(func(m *models.Mock) {
						told.Add(1)
						mgr.RecordOrphanWindow(m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock)
					})
				})
				if got, want := mgr.WasMockDroppedForTC("test-7"), !unread && path != addedAfterClose; got != want {
					t.Fatalf("unread %v, %s: test-7 suppressed %v; want %v", unread, path, got, want)
				}
				if n := told.Load(); (n == 0) != unread {
					t.Fatalf("unread %v, %s: OnDrop told %d times; want it told only of an answer read", unread, path, n)
				}
				// A test case that ran inside the answer's span, not its owner.
				if orphaned, _ := mgr.WasMockOrphanedInWindow(at.Add(100*time.Millisecond), at.Add(200*time.Millisecond)); orphaned == unread {
					t.Fatalf("unread %v, %s: a test case inside the answer's span left out %v; want it left out only for an answer read", unread, path, orphaned)
				}
			}
		}
	})
}

// addedAfterClose is droppedOn's path for a mock AddMock is handed after
// CloseOutChan.
const addedAfterClose = "AddMock after close"

// droppedOn has a manager, its OnDrop wired by wire, drop mock, sent for
// owner (or handed to AddMock, which takes no owner), on path, and returns
// the manager once every send is done.
func droppedOn(t *testing.T, path string, mock *models.Mock, owner string, wire func(*SyncMockManager)) *SyncMockManager {
	t.Helper()
	var mgr *SyncMockManager
	switch path {
	case addedAfterClose:
		mgr = &SyncMockManager{buffer: make([]*models.Mock, 0, defaultMockBufferCapacity)}
		wire(mgr)
		mgr.SetOutputChannel(make(chan *models.Mock, 1))
		mgr.CloseOutChan()
		before := mgr.outChanClosedDrops.Load()
		mgr.AddMock(mock)
		if got := mgr.outChanClosedDrops.Load() - before; got != 1 {
			t.Fatalf("%s: AddMock counted %d drops on the closed channel; want 1", path, got)
		}
	case "channel closed":
		mgr = &SyncMockManager{buffer: make([]*models.Mock, 0, defaultMockBufferCapacity)}
		wire(mgr)
		mgr.SetOutputChannel(make(chan *models.Mock, 1))
		mgr.CloseOutChan()
		if sendOwnedAndWait(t, mgr, mock, owner) {
			t.Fatalf("%s: the answer was delivered; want it dropped", path)
		}
	case "byte bound":
		// As TestAMockBehindTheDrainersHandIsBoundedByBytes: the process's
		// queue at its bound, and a mock in the drainer's hand.
		ch := make(chan *models.Mock, 1)
		ch <- &models.Mock{Name: "filler"}
		clk := &stall.ManualClock{}
		mgr = New(nil)
		mgr.outClock, mgr.outStall = clk, time.Hour
		wire(mgr)
		mgr.SetOutputChannel(ch)
		processOutQueued.Add(outQueueMaxBytes)
		defer processOutQueued.Add(-outQueueMaxBytes)
		inHand := make(chan bool, 1)
		mgr.send(&models.Mock{Name: "in-hand"}, "", func(d bool) { inHand <- d })
		waitClockPending(t, clk)
		if sendOwnedAndWait(t, mgr, mock, owner) {
			t.Fatalf("%s: the answer was delivered; want it dropped", path)
		}
		<-ch // the consumer takes again: the filler, then the mock in hand
		select {
		case d := <-inHand:
			if !d {
				t.Fatalf("%s: the mock in the drainer's hand was dropped", path)
			}
		case <-time.After(time.Minute): // only turns a hang into a failure
			t.Fatalf("%s: the mock in the drainer's hand never came", path)
		}
		<-ch
		waitOutQueue(t, mgr)
	case "consumer stalled":
		mgr = fullOutChanManager(t)
		wire(mgr)
		if sendOwnedAndWait(t, mgr, mock, owner) {
			t.Fatalf("%s: the answer was delivered; want it dropped", path)
		}
		waitOutQueue(t, mgr)
	default:
		t.Fatalf("no drop path %q", path)
	}
	return mgr
}
