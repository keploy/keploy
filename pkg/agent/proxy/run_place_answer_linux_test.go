//go:build linux

package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// placedRelease stands in for the release enterprise registers
// (models.RegisterAnswerRelease): a moment inside a window is due once the
// first window that holds it opened, one in the gap after a test once that
// test ended, one before every window at once, and every one at the set's
// end.
func placedRelease(p models.RunPlace, t time.Time) bool {
	if p.SetEnd || p.Windows.Len() == 0 {
		return true
	}
	i := p.Windows.ReleaseIndex(t)
	if i < 0 {
		return true
	}
	anchor := models.RunOpen(i)
	if !p.Windows.Contains(t) {
		anchor = models.RunEnd(i)
	}
	return anchor <= p.Reached
}

// placedMock is a per-test HTTP recording of GET target, its call made at
// req and its answer's last byte written at res.
func placedMock(name, target string, req, res time.Time) *models.Mock {
	m := r3Mock(name, req, "GET", target, "", name)
	m.Spec.ResTimestampMock = res
	m.TestModeInfo = models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}
	return m
}

// placedSet is a staged set the rows replay, as keploy test does under the
// proven switch (each test's exact view opens it, the pool's view after it
// ends it) or with the switch off (each test's staging of its window opens
// it, and its end is the facet's EndRunTest). As there, a test's begin and
// its end first wait until no call is in the proxy's hands (tr): a call the
// application made in a test is matched in that test's view, however late
// its parser reads it.
type placedSet struct {
	mm      *MockManager
	tr      *inflight
	windows []models.TestWindow
	mocks   []*models.Mock
	proven  bool
}

func stagePlacedSet(t *testing.T, proven bool, windows []models.TestWindow, mocks ...*models.Mock) *placedSet {
	t.Helper()
	sw := ""
	if proven {
		sw = "1"
	}
	t.Setenv(provenswitch.Env, sw)
	t.Cleanup(models.RegisterAnswerRelease(models.HTTP, placedRelease))
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows(windows)
	mm.SetMocksWithWindow(mocks, nil, models.BaseTime, placeAt(3600))
	return &placedSet{mm: mm, tr: &inflight{}, windows: windows, mocks: mocks, proven: proven}
}

// settle waits, as keploy test does before a test's begin and its end, until
// no call is in the proxy's hands: none unread, none read and not answered
// or held.
func (s *placedSet) settle(t *testing.T, what string) {
	t.Helper()
	settledWithin(t, s.tr, func() bool { return s.tr.Pending(true) == 0 }, what)
}

// open opens test's window, serving owned as its own recordings under the
// switch.
func (s *placedSet) open(t *testing.T, test string, owned ...string) {
	t.Helper()
	s.settle(t, "before "+test+"'s begin")
	if s.proven {
		var own []*models.Mock
		for _, n := range owned {
			m := roundMock(s.mm, n)
			if m == nil {
				t.Fatalf("%s is not staged", n)
			}
			own = append(own, m)
		}
		s.mm.ServeExactTest(test, own)
		return
	}
	for _, w := range s.windows {
		if w.TestCase == test {
			s.mm.SetMocksWithWindow(s.mocks, nil, w.Start, w.End)
			return
		}
	}
	t.Fatalf("no window for %s", test)
}

// end ends test, the one open.
func (s *placedSet) end(t *testing.T, test string) {
	t.Helper()
	s.settle(t, "before "+test+"'s end")
	if s.proven {
		s.mm.ServeExactPool("k", nil)
		return
	}
	s.mm.EndRunTest(test)
}

// placedCall sends GET target from app through the real HTTP parser on
// agentEnd, reading s's store, and returns once the application has written
// the call; the channel then gives, once the application has read the whole
// answer, its body, after its status when that is not 200 OK ("" for none).
func (s *placedSet) placedCall(t *testing.T, ctx context.Context, app, agentEnd net.Conn, target string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = app.Close(); _ = agentEnd.Close() })
	go func() {
		_ = httpint.New(zap.NewNop()).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, s.mm, models.OutgoingOptions{})
		_ = agentEnd.Close()
	}()
	out := make(chan string, 1)
	if _, err := app.Write([]byte(httpGet(target))); err != nil {
		t.Fatalf("the application's call %s: %v", target, err)
	}
	go func() {
		got := ""
		if resp, err := http.ReadResponse(bufio.NewReader(app), nil); err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			got = string(body)
			if resp.StatusCode != http.StatusOK {
				got = resp.Status + ": " + got
			}
		}
		out <- got
	}()
	return out
}

// call is the application's call of target in the test open, on a
// connection the proxy accepted (s.tr), whose parser marks a held answer's
// wait idle.
func (s *placedSet) call(t *testing.T, target string) <-chan string {
	t.Helper()
	app, conn := acceptedPair(t, s.tr)
	return s.placedCall(t, integrations.WithRequestHolder(context.Background(), conn), app, conn, target)
}

func placedNotYet(t *testing.T, got <-chan string, what string) {
	t.Helper()
	select {
	case a := <-got:
		t.Fatalf("%s: the application read %q", what, a)
	case <-time.After(200 * time.Millisecond):
	}
}

func placedWritten(t *testing.T, got <-chan string, want, what string) {
	t.Helper()
	select {
	case a := <-got:
		if a != want {
			t.Fatalf("%s: the application read %q, want %q", what, a, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the answer was not written", what)
	}
}

// settledWithin waits, by the tracker's change events, until cond holds,
// and fails the row when it does not within 10 s: a settle that would hang.
func settledWithin(t *testing.T, tr *inflight, cond func() bool, what string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		changed := tr.Changed()
		if cond() {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("%s: the connection did not settle", what)
		}
	}
}

// An answer recorded in test-3's window, for a call made in test-1, is
// written at test-3's open though test-3 calls none of its own recordings:
// the place moves on the windows the replayer opens, never on a call the
// application may not make, so the answer is out by test-3's end at the
// latest.
func TestHeldAnswer_WrittenAtItsWindowThoughThatTestCallsNothing(t *testing.T) {
	bothSwitches(t, func(t *testing.T, proven bool) {
		s := stagePlacedSet(t, proven, placeWindows(),
			placedMock("mock-held", "/held", placeAt(2), placeAt(45)),
			placedMock("mock-own3", "/own3", placeAt(42), placeAt(43)))
		s.open(t, "test-1", "mock-held")
		got := s.call(t, "/held")
		placedNotYet(t, got, "test-3's answer in test-1")
		s.end(t, "test-1")
		s.open(t, "test-2")
		placedNotYet(t, got, "test-3's answer in test-2")
		s.end(t, "test-2")
		placedNotYet(t, got, "test-3's answer after test-2's end")
		s.open(t, "test-3", "mock-own3")
		placedWritten(t, got, "mock-held", "at test-3's open, which calls nothing")
		s.end(t, "test-3")
	})
}

// With overlapping windows, an answer recorded at a moment two windows hold
// is written when the first of them opens: not held to the later window's
// open, nor to the first one's end.
func TestHeldAnswer_InOverlappingWindowsIsWrittenAtTheFirstOpen(t *testing.T) {
	bothSwitches(t, func(t *testing.T, proven bool) {
		windows := []models.TestWindow{
			{TestCase: "test-1", Start: placeAt(0), End: placeAt(10)},
			{TestCase: "test-2", Start: placeAt(20), End: placeAt(40)},
			{TestCase: "test-3", Start: placeAt(30), End: placeAt(50)},
		}
		s := stagePlacedSet(t, proven, windows, placedMock("mock-held", "/held", placeAt(2), placeAt(35)))
		s.open(t, "test-1", "mock-held")
		got := s.call(t, "/held")
		placedNotYet(t, got, "an answer test-2's window holds, in test-1")
		s.end(t, "test-1")
		placedNotYet(t, got, "an answer test-2's window holds, after test-1's end")
		s.open(t, "test-2")
		placedWritten(t, got, "mock-held", "at test-2's open, the first window that holds it")
		s.end(t, "test-2")
	})
}

// A held answer leaves its connection settled: a test's end and a test's
// begin, which wait until the calls in the proxy's hands are answered, do
// not wait on it, nor on the moment a place change wakes its parser. Its
// call counts as consumed in the test that made it, before it is written.
func TestHeldAnswer_LeavesItsConnectionSettledAndItsCallItsTests(t *testing.T) {
	bothSwitches(t, func(t *testing.T, proven bool) {
		s := stagePlacedSet(t, proven, placeWindows(), placedMock("mock-held", "/held", placeAt(2), placeAt(25)))
		tr := &inflight{}
		app, conn := acceptedPair(t, tr)
		s.open(t, "test-1", "mock-held")
		got := s.placedCall(t, integrations.WithRequestHolder(context.Background(), conn), app, conn, "/held")
		settledWithin(t, tr, func() bool { return conn.idle.Load() == 1 && tr.Pending(true) == 0 }, "a held answer in test-1")
		if pending, unknown := tr.State(true); pending != 0 || len(unknown) != 0 {
			t.Fatalf("a held answer in test-1: state %d %v, want settled", pending, unknown)
		}
		if !proven {
			if _, ok := r3States(s.mm)["mock-held"]; !ok {
				t.Fatal("the held answer's call is not consumed in test-1")
			}
		}
		s.end(t, "test-1")
		if proven {
			test, consumed, _, _ := s.mm.ExactTestOutcome()
			if test != "test-1" || !slices.Contains(consumed, "mock-held") {
				t.Fatalf("test-1's outcome: %s consumed %v, want mock-held", test, consumed)
			}
		}
		// The end woke the parser, whose connection is busy until it finds
		// nothing due and waits idle again: a settle after the end waits that
		// long, not until the answer is written.
		settledWithin(t, tr, func() bool { return conn.idle.Load() == 1 && tr.Pending(true) == 0 }, "after test-1's end")
		placedNotYet(t, got, "test-2's answer after test-1's end")
		s.open(t, "test-2")
		placedWritten(t, got, "mock-held", "at test-2's open")
		settledWithin(t, tr, func() bool { return tr.Pending(true) == 0 }, "after the answer was written")
		if n := conn.idle.Load(); n != 0 {
			t.Fatalf("idle marks left after the answer was written: %d", n)
		}
		s.end(t, "test-2")
	})
}
