package manager

import (
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A test case sent with no request time is revoked by a later loss of a
// test's mock requested before its end, as that request may be its own,
// however long before its end it was made; one that ended before the request
// is not, nor is one whose window starts after it.
func TestASentTestCaseWithNoRequestTimeIsRevokedByALossBeforeItsEnd(t *testing.T) {
	m := New(nil)
	fullLane(t, m)
	t0 := time.Now().Add(-time.Minute)
	sentFirstAt(m, t0)
	at := t0.Add(10 * time.Second)
	for _, c := range []struct {
		name       string
		start, end time.Time
	}{
		{"open-late", time.Time{}, at.Add(30 * time.Second)},
		{"open-at", time.Time{}, at},
		{"open-early", time.Time{}, at.Add(-time.Second)},
		{"windowed-after", at.Add(time.Second), at.Add(2 * time.Second)},
	} {
		if !m.SendingTestCase(c.name, c.start, c.end) {
			t.Fatalf("setup: %s was left out with nothing lost", c.name)
		}
	}
	queueAt(t, outQueueMaxBytes)
	sendNow(m, asyncAt("c1", at), "")
	for name, revoked := range map[string]bool{"open-late": true, "open-at": true, "open-early": false, "windowed-after": false} {
		if got := m.WasMockDroppedForTC(name); got != revoked {
			t.Errorf("%s: revoked %v, want %v", name, got, revoked)
		}
	}
}

// A test case with no request time counts toward the recording's first test
// case at its end, the latest its request can have been: a start-up mock
// requested before its end is before the first, one after it is not. A
// revoke by name the client applies takes it out of the first.
func TestATestCaseWithNoRequestTimeCountsTowardTheFirstAtItsEnd(t *testing.T) {
	m := New(nil)
	m.SetRevokeCapable(true)
	e := time.Now().Add(-time.Minute)
	if !m.SendingTestCase("open", time.Time{}, e) {
		t.Fatal("setup: a test case was left out with nothing lost")
	}
	if !m.beforeFirstTestCase(e.Add(-time.Second), "") {
		t.Fatal("a request before the end of the only test case, which has no request time, is not before the first")
	}
	if m.beforeFirstTestCase(e.Add(time.Second), "") {
		t.Fatal("a request after the end of the only test case, which has no request time, is before the first")
	}
	if !m.SendingTestCase("later", e.Add(time.Second), e.Add(2*time.Second)) {
		t.Fatal("setup: a test case was left out with nothing lost")
	}
	m.RevokeTestCase("open")
	if !m.beforeFirstTestCase(e.Add(500*time.Millisecond), "") || m.beforeFirstTestCase(e.Add(time.Second), "") {
		t.Fatal("the first did not move to the next test case once the one with no request time was revoked")
	}
}

// A test case with no request time is left out at its verdict by a test's
// mock the stale cutoff dropped before its end, and the line says it had no
// request time; one that ended before the drop's request is sent.
func TestATestCaseWithNoRequestTimeIsLeftOutByACutBeforeItsEnd(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	at := time.Now().Add(-time.Minute)
	mk := asyncAt("c1", at)
	mk.TestModeInfo.IsStartup = false // a test's, not one re-read as any test's
	m.leaveOutSpan(mk, at, at, &m.cutLost, m.revokeEpoch.Load(), staleCutoffWhy, func(string) {})
	if m.SendingTestCase("open", time.Time{}, at.Add(time.Minute)) {
		t.Fatal("a test case with no request time, ending after a cut's request, was sent")
	}
	if !m.SendingTestCase("open-before", time.Time{}, at.Add(-time.Second)) {
		t.Fatal("a test case with no request time, ending before a cut's request, was left out")
	}
	lines := logs.FilterField(zap.String("test_case", "open")).All()
	if len(lines) != 1 {
		t.Fatalf("%d lines name the test case left out, want 1", len(lines))
	}
	if got := lines[0].ContextMap()["window_start"]; got != noRequestTime {
		t.Fatalf("window_start %q, want %q", got, noRequestTime)
	}
}

// The test cases with no request time a list holds are sorted by end, so
// the first is the earliest end whatever the order they were sent in; a
// revoke reaches those that end at or after its request, wherever its window
// bound (to) is; one the client keeps still counts toward the first; one
// revoked by name is the one taken out of two that end at once; one with no
// name is one it does not name.
func TestASentListOfTestCasesWithNoRequestTime(t *testing.T) {
	e := time.Now().Add(-time.Minute)
	t.Run("first", func(t *testing.T) {
		var l sentList
		l.addOpen("b", e.Add(time.Second))
		l.addOpen("a", e)
		if at, ok, _ := l.first(); !ok || !at.Equal(e) {
			t.Fatalf("first %v (%v), want %v", at, ok, e)
		}
	})
	t.Run("revoke", func(t *testing.T) {
		var l sentList
		l.addOpen("early", e.Add(-time.Second))
		l.addOpen("late", e.Add(time.Second))
		var got []string
		if n := l.revokeReaching(e, e, true, func(name string) { got = append(got, name) }); n != 1 || len(got) != 1 || got[0] != "late" {
			t.Fatalf("revoked %d: %v, want [late]", n, got)
		}
		if at, ok, _ := l.first(); !ok || !at.Equal(e.Add(-time.Second)) {
			t.Fatalf("first %v (%v) after the revoke, want the one left", at, ok)
		}
	})
	t.Run("kept", func(t *testing.T) {
		var l sentList
		l.addOpen("o", e)
		if n := l.revokeReaching(e.Add(-time.Second), e.Add(-time.Second), false, func(string) {}); n != 1 {
			t.Fatalf("revoked %d, want 1", n)
		}
		if at, ok, _ := l.first(); !ok || !at.Equal(e) {
			t.Fatalf("first %v (%v): a revoked test case the client keeps does not count", at, ok)
		}
	})
	t.Run("by name", func(t *testing.T) {
		var l sentList
		l.addOpen("o1", e)
		l.addOpen("o2", e)
		l.forget("o2", true)
		if len(l.open) != 1 || l.open[0].name != "o1" {
			t.Fatalf("held %+v after o2 was revoked, want o1", l.open)
		}
	})
	t.Run("no name", func(t *testing.T) {
		var l sentList
		l.addOpen("", e)
		if len(l.open) != 0 || !l.unnamedReaching(e.Add(-time.Second)).Equal(e) {
			t.Fatalf("held %+v, reaching %v: one with no name is not one it does not name", l.open, l.unnamedReaching(e.Add(-time.Second)))
		}
	})
}

// A list holds no more than maxSentTCs test cases, with a request time or
// without, whichever kind fills it.
func TestASentListWithNoRequestTimeStaysInItsBound(t *testing.T) {
	e := time.Now().Add(-time.Hour)
	t.Run("open fills it", func(t *testing.T) {
		var l sentList
		for i := 0; i <= maxSentTCs; i++ {
			l.addOpen(fmt.Sprint("o-", i), e.Add(time.Duration(i)*time.Millisecond))
		}
		if n := len(l.tcs) + len(l.open); n > maxSentTCs {
			t.Fatalf("holds %d, bound %d", n, maxSentTCs)
		}
	})
	t.Run("open then windowed", func(t *testing.T) {
		var l sentList
		for i := 0; i < maxSentTCs; i++ {
			l.addOpen(fmt.Sprint("o-", i), e.Add(time.Duration(i)*time.Millisecond))
		}
		l.add("w", e, e.Add(time.Second))
		if n := len(l.tcs) + len(l.open); n > maxSentTCs {
			t.Fatalf("holds %d, bound %d", n, maxSentTCs)
		}
	})
}

// A test case with no request time holds every request of its own recording
// that a loss reached before its end, from the earliest, whatever the order
// the losses came in, and none of an older recording's.
func TestATestCaseWithNoRequestTimeHoldsOnlyItsRecordingsLosses(t *testing.T) {
	t0 := time.Now().Add(-time.Minute)
	a, b := t0.Add(10*time.Second), t0.Add(20*time.Second)
	for _, order := range [][]time.Time{{a, b}, {b, a}} {
		t.Run(fmt.Sprintf("first lost at +%v", order[0].Sub(t0)), func(t *testing.T) {
			m := New(nil)
			fullLane(t, m)
			sentFirstAt(m, t0)
			queueAt(t, outQueueMaxBytes)
			for i, at := range order {
				sendNow(m, asyncAt(fmt.Sprint("c", i), at), "")
			}
			if m.SendingTestCase("between", time.Time{}, a.Add(time.Second)) {
				t.Fatal("a test case with no request time, ending after the earlier loss's request, was sent")
			}
			if !m.SendingTestCase("before", time.Time{}, a.Add(-time.Second)) {
				t.Fatal("a test case with no request time, ending before both losses' requests, was left out")
			}
		})
	}
	t.Run("a new recording", func(t *testing.T) {
		m := New(nil)
		fullLane(t, m)
		sentFirstAt(m, t0)
		queueAt(t, outQueueMaxBytes)
		sendNow(m, asyncAt("c1", a), "")
		m.SetOutputChannel(make(chan *models.Mock, 1))
		if !m.SendingTestCase("new", time.Time{}, time.Now()) {
			t.Fatal("a test case with no request time was left out for a loss of an older recording")
		}
	})
}
