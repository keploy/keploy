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

// pastTheCap is a manager of a recording whose stream sent "first", over
// [t0, t0+1s], then maxSentTCs+10 more from t0+1min on, 1 ms apart, to a
// client that deletes the test cases it revokes: the sent list halved, and
// let go "first" and test-0 to test-(maxSentTCs/2-2). With lostRead, a
// start-up read at t0+2s was lost before them, as a test's (it is after the
// first test case).
func pastTheCap(t *testing.T, t0 time.Time, lostRead bool) (*SyncMockManager, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	m.SetRevokeCapable(true)
	m.SetOutputChannel(make(chan *models.Mock, 1))
	if !m.SendingTestCase("first", t0, t0.Add(time.Second)) {
		t.Fatal("setup: first left out")
	}
	if lostRead {
		m.dropOut(asyncAt("c1", t0.Add(2*time.Second)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		if n := leftOutLines(logs); n != 0 {
			t.Fatalf("setup: %d shared ERRORs for a test's loss", n)
		}
	}
	for i := 0; i < maxSentTCs+10; i++ {
		at := t0.Add(time.Minute + time.Duration(i)*time.Millisecond)
		if !m.SendingTestCase(fmt.Sprintf("test-%d", i), at, at.Add(time.Microsecond)) {
			t.Fatalf("setup: test-%d left out", i)
		}
	}
	if !m.sentTCs.halved || m.sentTCs.firstLetGo != "first" {
		t.Fatalf("setup: halved %v, first let go %q, want the sent list halved with first let go", m.sentTCs.halved, m.sentTCs.firstLetGo)
	}
	return m, logs
}

// Past the sent list's cap, a revoke by name of a test case it does not
// hold, one never sent (under the sync capture a test's mock is usually
// dropped while its test case is still in flight) or one let go that did
// not start first, cannot move the recording's first test case: the first
// stays known, the start-up read lost earlier as a test's is not re-read,
// no sent test case is revoked for it, and a test's mock 90 minutes in is
// still a test's.
func TestARevokeOfANeverSentNamePastTheCapLeavesTheFirstKnown(t *testing.T) {
	t0 := time.Now().Add(-2 * time.Hour)
	for _, c := range []struct{ what, name string }{
		{"a mock of a test case never sent dropped", "test-never-sent"},
		{"one let go that did not start first given up", "test-10"},
	} {
		t.Run(c.what, func(t *testing.T) {
			m, logs := pastTheCap(t, t0, true)
			before := len(revokesQueued(m))
			if c.name == "test-never-sent" {
				m.dropOut(addMockTo("c9", false), c.name, m.revokeEpoch.Load(), string(MockDroppedOutputFull))
			} else {
				m.RevokeTestCase(c.name)
			}
			if first, ok, exact := m.sentTCs.first(); !ok || !exact || !first.Equal(t0) {
				t.Fatalf("first %v (known %v, exact %v), want %v, first's start", first, ok, exact, t0)
			}
			if n := len(rereadLines(logs)); n != 0 {
				t.Fatalf("%d re-read ERRORs, want none: the first did not move", n)
			}
			if got := len(revokesQueued(m)) - before; got != 1 {
				t.Fatalf("%d names queued to revoke, want 1, the one named", got)
			}
			if m.servedToAnyTest(asyncAt("c2", t0.Add(90*time.Minute)), "") {
				t.Fatal("a test's mock 90 minutes after the first test case counts as one any test may need")
			}
		})
	}
}

// Past the cap, a revoke of the test case let go that started first moves
// the first to one it no longer names: the first is then known only to be
// no later than the earliest it still names, a sent test case still in the
// recording. A mock requested at or after that bound is not before the
// first; one before it may be, so a start-up read lost earlier as a test's,
// before the bound, is re-read as one any test may need, and the ERROR says
// the first is no later than the bound.
func TestARevokeOfTheFirstLetGoBoundsTheFirstByTheEarliestStillNamed(t *testing.T) {
	t0 := time.Now().Add(-2 * time.Hour)
	bound := t0.Add(time.Minute + time.Duration(maxSentTCs/2-1)*time.Millisecond) // test-(maxSentTCs/2-1), the earliest still named
	t.Run("the bound", func(t *testing.T) {
		m, logs := pastTheCap(t, t0, false)
		m.RevokeTestCase("first")
		if first, ok, exact := m.sentTCs.first(); !ok || exact || !first.Equal(bound) {
			t.Fatalf("first %v (known %v, exact %v), want a bound at %v", first, ok, exact, bound)
		}
		if !m.servedToAnyTest(asyncAt("c2", bound.Add(-time.Nanosecond)), "") {
			t.Fatal("a start-up mock requested before the bound is not one any test may need")
		}
		for _, at := range []time.Time{bound, t0.Add(90 * time.Minute)} {
			if m.servedToAnyTest(asyncAt("c2", at), "") {
				t.Fatalf("a start-up mock requested at %v, at or after the bound %v, counts as one any test may need", at, bound)
			}
		}
		if n := len(rereadLines(logs)); n != 0 {
			t.Fatalf("%d re-read ERRORs with no start-up loss to re-read", n)
		}
	})
	t.Run("a start-up loss before it", func(t *testing.T) {
		m, logs := pastTheCap(t, t0, true)
		m.RevokeTestCase("first")
		got := rereadLines(logs)
		if want := "no later than " + bound.Format(time.RFC3339Nano); len(got) != 1 || got[0]["first_test_case_now"] != want {
			t.Fatalf("re-read ERRORs %v, want one whose first_test_case_now is %q", got, want)
		}
		if got[0]["left_out_from"] != t0.Add(2*time.Second).Format(time.RFC3339Nano) {
			t.Fatalf("the re-read left out from %v, want the lost read's request", got[0]["left_out_from"])
		}
	})
}

// A revoke the client does not apply (it cannot delete: an older CLI, a
// store with no delete, the DaemonSet's push stream) leaves the test case in
// the recording, so it still counts toward the first, and a start-up read
// lost after it stays a test's; a revoke the client applies moves the first
// past the read, which is then re-read as one any test may need. "a" is sent
// over [t0, t0+1s] and "b" over [t0+5s, t0+6s]; the read at t0+2s is lost
// as a test's; then a test's mock over a's window (t0+0.5s) is lost.
func TestAKeptRevokeDoesNotMoveTheFirst(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	for _, deletes := range []bool{false, true} {
		t.Run(fmt.Sprintf("client deletes %v", deletes), func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			m := New(zap.New(core))
			m.SetRevokeCapable(deletes)
			m.SetOutputChannel(make(chan *models.Mock, 1))
			m.SendingTestCase("a", t0, t0.Add(time.Second))
			m.SendingTestCase("b", t0.Add(5*time.Second), t0.Add(6*time.Second))
			m.dropOut(asyncAt("c1", t0.Add(2*time.Second)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
			m.dropOut(asyncAt("c2", t0.Add(500*time.Millisecond)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
			later := m.SendingTestCase("later", t0.Add(3*time.Second), t0.Add(4*time.Second))
			rereads := rereadLines(logs)
			if !deletes {
				if f := m.firstSent.Load(); f == nil || !f.Equal(t0) {
					t.Fatalf("first %v, want %v: a's start, kept by the client", f, t0)
				}
				if len(rereads) != 0 || !later {
					t.Fatalf("%d re-read ERRORs, later sent %v: a kept revoke moved the first", len(rereads), later)
				}
				return
			}
			if len(rereads) != 1 || rereads[0]["first_test_case_now"] != t0.Add(5*time.Second).Format(time.RFC3339Nano) || later {
				t.Fatalf("re-reads %v, later sent %v: want one naming b as the first, and later left out", rereads, later)
			}
		})
	}
}

// A revoke by name the client does not apply, of the first test case's own
// mock dropped or of its window given up, leaves the first where it is;
// one it applies moves it (the re-read rows).
func TestAKeptRevokeByNameDoesNotMoveTheFirst(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	for _, how := range []string{"its own mock dropped", "its window given up"} {
		t.Run(how, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			m := New(zap.New(core))
			m.SetOutputChannel(make(chan *models.Mock, 1))
			m.SendingTestCase("a", t0, t0.Add(time.Second))
			m.SendingTestCase("b", t0.Add(5*time.Second), t0.Add(6*time.Second))
			m.dropOut(asyncAt("c1", t0.Add(2*time.Second)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
			if how == "its own mock dropped" {
				m.dropOut(callAt("c2", t0.Add(500*time.Millisecond)), "a", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
			} else {
				m.RevokeTestCase("a")
			}
			if f := m.firstSent.Load(); f == nil || !f.Equal(t0) {
				t.Fatalf("first %v, want %v: a's start, kept by the client", f, t0)
			}
			if n := len(rereadLines(logs)); n != 0 {
				t.Fatalf("%d re-read ERRORs for a revoke the client keeps", n)
			}
		})
	}
}

// A loss of a mock any test may need revokes the test cases sent whose
// window reaches past its request; a client that cannot delete keeps them,
// so the first test case stays theirs, and a start-up mock requested before
// the span, over the first test case's window, stays that test's.
func TestASharedLossKeptByTheClientDoesNotMoveTheFirst(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	m.SetOutputChannel(make(chan *models.Mock, 1))
	t0 := time.Now().Add(-time.Hour)
	m.SendingTestCase("a", t0, t0.Add(time.Second))
	session := addMockTo("s1", true)
	session.Spec.ReqTimestampMock, session.Spec.ResTimestampMock = t0.Add(500*time.Millisecond), t0.Add(600*time.Millisecond)
	session.DeriveLifetime()
	m.dropOut(session, "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
	if f := m.firstSent.Load(); f == nil || !f.Equal(t0) {
		t.Fatalf("first %v, want %v: a's start, kept by the client", f, t0)
	}
	if m.servedToAnyTest(asyncAt("c1", t0.Add(200*time.Millisecond)), "") {
		t.Fatal("a start-up mock over the kept first test case's window counts as one any test may need")
	}
	if n := leftOutLines(logs); n != 1 {
		t.Fatalf("%d shared ERRORs, want 1, the session mock's", n)
	}
}

// Test cases revoked and kept by the client keep the earliest start among
// them as the first; a revoke the client keeps of the one let go that
// started first leaves the first known.
func TestKeptRevokesKeepTheEarliestStart(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	var l sentList
	l.add("a", t0, t0.Add(time.Second))
	l.add("b", t0.Add(5*time.Second), t0.Add(6*time.Second))
	if n := l.revokeReaching(t0, time.Time{}, false, func(string) {}); n != 2 {
		t.Fatalf("%d revoked, want both", n)
	}
	if first, ok, exact := l.first(); !ok || !exact || !first.Equal(t0) {
		t.Fatalf("first %v (known %v, exact %v), want %v, a's, kept", first, ok, exact, t0)
	}
	var c sentList
	c.noteUnnamed("let-go", t0, t0.Add(time.Second))
	c.halved = true
	c.add("named", t0.Add(time.Minute), t0.Add(time.Minute))
	c.forget("let-go", false)
	if first, ok, exact := c.first(); !ok || !exact || !first.Equal(t0) {
		t.Fatalf("after a kept revoke of the first let go: first %v (known %v, exact %v), want %v", first, ok, exact, t0)
	}
	c.forget("let-go", true)
	if _, _, exact := c.first(); exact {
		t.Fatal("an applied revoke of the first let go left the first exact")
	}
}

// Of two test cases sent with the same start, a revoke by name takes out
// the one named, so a loss over their window still revokes the other.
func TestARevokeByNameOfOneOfTwoSharingAStartTakesOutThatOne(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	var l sentList
	l.add("a", t0, t0.Add(time.Second))
	l.add("b", t0, t0.Add(time.Second))
	l.forget("b", true)
	var revoked []string
	l.revokeReaching(t0, t0, true, func(name string) { revoked = append(revoked, name) })
	if len(revoked) != 1 || revoked[0] != "a" {
		t.Fatalf("a loss over both windows revoked %v, want [a]: the revoke of b took out another", revoked)
	}
}

// A test case sent with no name is never revoked, so a revoke with no name
// leaves the first it holds; the first is the earliest start of all, named
// or not.
func TestARevokeWithNoNameLeavesTheFirstKnown(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	var l sentList
	l.add("", t0, t0.Add(time.Second))
	l.add("x", t0.Add(5*time.Second), t0.Add(6*time.Second))
	l.forget("", true)
	if first, ok, exact := l.first(); !ok || !exact || !first.Equal(t0) {
		t.Fatalf("first %v (known %v, exact %v), want %v, the nameless one's", first, ok, exact, t0)
	}
	l.add("y", t0.Add(-time.Second), t0)
	if first, _, _ := l.first(); !first.Equal(t0.Add(-time.Second)) {
		t.Fatalf("first %v, want %v, the named one that started before the nameless one", first, t0.Add(-time.Second))
	}
}
