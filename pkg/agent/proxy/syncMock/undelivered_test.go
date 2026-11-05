package manager

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
// mapped; a duplicate's window prunes both. Mapped, the report would list it
// as a call the test was expected to make and did not.
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
		// A late decode, found by each path that finds one.
		for _, path := range []string{"ResolveRange", "ResolveRange, window containing it", "FlushOwnedWindows", "DeleteMocksStrictlyBefore"} {
			out := make(chan *models.Mock, 16)
			maps := make(chan models.TestMockMapping, 16)
			mgr := lateMockTestManager(out, maps, false)
			base := time.Now().Add(-time.Second)
			mgr.ResolveRange(base, base.Add(5*time.Millisecond), "test-1", true, true)
			read, unread := unreadAt(base.Add(2*time.Millisecond), false), unreadAt(base.Add(2*time.Millisecond), true)
			mgr.AddMock(read)
			mgr.AddMock(unread)
			findLateMock(mgr, base, path, true)
			sent, mapped := sentMocks(out), anyMapped(maps)
			if !hasMock(sent, read) || !hasMock(sent, unread) || !mapped[read.Name] || mapped[unread.Name] {
				t.Fatalf("%s: sent %d mocks and mapped %v; want both sent, the answer read (%s) mapped, the one never read (%s) not", path, len(sent), mapped, read.Name, unread.Name)
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
