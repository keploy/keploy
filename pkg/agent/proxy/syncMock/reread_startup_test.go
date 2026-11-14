package manager

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// rereadLines returns the ERRORs that say a start-up mock lost earlier as a
// test's was re-read as one any test may need.
func rereadLines(logs *observer.ObservedLogs) []map[string]interface{} {
	var out []map[string]interface{}
	for _, e := range logs.All() {
		if f := e.ContextMap(); f["reread"] != nil && e.Level == zapcore.ErrorLevel {
			out = append(out, f)
		}
	}
	return out
}

// twoSent is a manager of a recording whose stream sent "a", over [t0,
// t0+1s], its first test case, and "b", over [t0+5s, t0+6s], to a client
// that deletes the test cases it revokes, and its logs.
func twoSent(t *testing.T, t0 time.Time) (*SyncMockManager, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	m.SetRevokeCapable(true)
	m.SetOutputChannel(make(chan *models.Mock, 1))
	for name, w := range map[string][2]time.Time{"a": {t0, t0.Add(time.Second)}, "b": {t0.Add(5 * time.Second), t0.Add(6 * time.Second)}} {
		if !m.SendingTestCase(name, w[0], w[1]) {
			t.Fatalf("setup: %s was left out with no mock lost", name)
		}
	}
	return m, logs
}

// callAt is a mock no test owns and not of the start-up window, called over
// [at, at+100ms].
func callAt(conn string, at time.Time) *models.Mock {
	mk := addMockTo(conn, false)
	mk.Spec.ReqTimestampMock, mk.Spec.ResTimestampMock = at, at.Add(100*time.Millisecond)
	return mk
}

// A start-up mock (IsStartup) no test owns, requested after the recording's
// first test case, is a test's when it is lost: only the test cases over its
// call are left out. Once a revoke takes the first test case out, so that the
// first the recording holds starts after the mock's request, it is a
// start-up mock of the recording as it now stands, which a replay serves to
// any test from its start-up tier: its loss is re-read as one any test may
// need, said at ERROR once, naming it, and every test case whose window
// reaches past its request is left out, those sent revoked. Each way a
// revoke moves the first test case re-reads it: a test's loss over the
// first, the first's own mock dropped, its window given up, and the start-up
// mock's own loss over it.
func TestAStartUpMockLostAfterTheFirstTestCaseIsReReadWhenARevokeMovesTheFirstPastIt(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	read := asyncAt("c1", t0.Add(2*time.Second))
	lose := func(t *testing.T, m *SyncMockManager, logs *observer.ObservedLogs) {
		t.Helper()
		m.dropOut(read, "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		if n := len(rereadLines(logs)); n != 0 || leftOutLines(logs) != 0 {
			t.Fatalf("the start-up read lost after the first test case said %d re-reads, %d shared losses: it is a test's", n, leftOutLines(logs))
		}
		if !m.SendingTestCase("between", t0.Add(3*time.Second), t0.Add(4*time.Second)) {
			t.Fatal("a test case after the start-up read lost as a test's was left out")
		}
	}
	check := func(t *testing.T, m *SyncMockManager, logs *observer.ObservedLogs, first string) {
		t.Helper()
		got := rereadLines(logs)
		if len(got) != 1 {
			t.Fatalf("%d re-read ERRORs, want 1", len(got))
		}
		f := got[0]
		if f["left_out_from"] != read.Spec.ReqTimestampMock.Format(time.RFC3339Nano) || f["connID"] != "c1" || f["mock_kind"] != string(read.Kind) || f["first_test_case_now"] != first {
			t.Fatalf("the re-read ERROR says %v", f)
		}
		if _, ok := f["dropsSoFar"]; ok {
			t.Fatalf("the re-read ERROR gives the drop's place of a later one: %v", f)
		}
		revoked := map[string]bool{}
		for _, n := range revokesQueued(m) {
			revoked[n] = true
		}
		if !revoked["b"] || !revoked["between"] {
			t.Fatalf("revoked %v: b and between reach past the re-read mock's request", revokesQueued(m))
		}
		if m.SendingTestCase("later", t0.Add(3*time.Second), t0.Add(4*time.Second)) {
			t.Fatal("a test case checked after the re-read whose window reaches past its request was sent")
		}
		if !m.SendingTestCase("early", t0.Add(1200*time.Millisecond), t0.Add(1500*time.Millisecond)) {
			t.Fatal("a test case that ended before the re-read mock's request was left out")
		}
		// A revoke after it re-reads it no more: the span reaches its
		// request.
		m.RevokeTestCase("early")
		if n := len(rereadLines(logs)); n != 1 {
			t.Fatalf("%d re-read ERRORs once a later revoke moved the first again, want 1", n)
		}
	}
	t.Run("a test's loss over the first", func(t *testing.T) {
		m, logs := twoSent(t, t0)
		lose(t, m, logs)
		m.dropOut(callAt("c2", t0.Add(500*time.Millisecond)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		check(t, m, logs, t0.Add(3*time.Second).Format(time.RFC3339Nano))
	})
	t.Run("the first's own mock dropped", func(t *testing.T) {
		m, logs := twoSent(t, t0)
		lose(t, m, logs)
		m.dropOut(callAt("c2", t0.Add(500*time.Millisecond)), "a", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		check(t, m, logs, t0.Add(3*time.Second).Format(time.RFC3339Nano))
	})
	t.Run("the first's window given up", func(t *testing.T) {
		m, logs := twoSent(t, t0)
		lose(t, m, logs)
		m.RevokeTestCase("a")
		check(t, m, logs, t0.Add(3*time.Second).Format(time.RFC3339Nano))
	})
	t.Run("its own loss over the first", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		m := New(zap.New(core))
		m.SetRevokeCapable(true)
		m.SetOutputChannel(make(chan *models.Mock, 1))
		m.SendingTestCase("a", t0, t0.Add(3*time.Second))
		m.dropOut(read, "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		got := rereadLines(logs)
		if len(got) != 1 || got[0]["first_test_case_now"] != "none known" || got[0]["revoked_sent"] != int64(0) {
			t.Fatalf("the start-up read whose own loss took out the only test case sent said %v, want one re-read with no first known", got)
		}
		if m.SendingTestCase("after", t0.Add(4*time.Second), t0.Add(5*time.Second)) {
			t.Fatal("a test case after the re-read start-up read was sent")
		}
	})
}

// Only a start-up mock is re-read: a test's mock of no start-up window, lost
// after the first test case, stays a test's when a revoke moves the first
// past its request. The earliest-requested start-up loss is the one re-read,
// whatever order they were lost in, and a new recording re-reads none of the
// last one's.
func TestOnlyTheEarliestStartUpLossOfTheRecordingIsReRead(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	t.Run("not a start-up mock", func(t *testing.T) {
		m, logs := twoSent(t, t0)
		m.dropOut(callAt("c1", t0.Add(2*time.Second)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		m.RevokeTestCase("a")
		if n := len(rereadLines(logs)); n != 0 || leftOutLines(logs) != 0 {
			t.Fatalf("a test's mock of no start-up window said %d re-reads, %d shared losses", n, leftOutLines(logs))
		}
		if got := revokesQueued(m); len(got) != 1 || got[0] != "a" {
			t.Fatalf("revoked %v, want [a]", got)
		}
	})
	for name, order := range map[string][2]time.Duration{
		"the earlier lost last":  {3 * time.Second, 2 * time.Second},
		"the earlier lost first": {2 * time.Second, 3 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			m, logs := twoSent(t, t0)
			for i, d := range order {
				m.dropOut(asyncAt(fmt.Sprintf("c%d", i), t0.Add(d)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
			}
			m.RevokeTestCase("a")
			got := rereadLines(logs)
			if len(got) != 1 || got[0]["left_out_from"] != t0.Add(2*time.Second).Format(time.RFC3339Nano) {
				t.Fatalf("re-read %v, want one from the earlier request", got)
			}
		})
	}
	t.Run("a new recording", func(t *testing.T) {
		m, logs := twoSent(t, t0)
		m.dropOut(asyncAt("c1", t0.Add(2*time.Second)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		m.SetOutputChannel(make(chan *models.Mock, 1))
		m.SendingTestCase("a2", t0, t0.Add(time.Second))
		m.SendingTestCase("b2", t0.Add(5*time.Second), t0.Add(6*time.Second))
		m.RevokeTestCase("a2")
		if n := len(rereadLines(logs)); n != 0 {
			t.Fatalf("a new recording re-read %d of the last one's start-up losses", n)
		}
	})
}

// A stale cutoff's drop that revokes the recording's first test case moves
// the first past a start-up mock lost earlier as a test's, as a capacity drop
// does: the loss is re-read as one any test may need, and that is said at
// ERROR, once. Without the line the re-read would still leave out every later
// test case, and nothing would say why.
func TestACutDropThatMovesTheFirstSaysTheReRead(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	m, logs := twoSent(t, t0)
	m.dropOut(asyncAt("c1", t0.Add(2*time.Second)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
	if n := len(rereadLines(logs)); n != 0 {
		t.Fatalf("%d re-reads before the first moved", n)
	}
	// A test's call inside "a", the first test case, falls to the stale
	// cutoff: "a" is revoked, and the first moves past the start-up read.
	m.cutDropped([]*models.Mock{callAt("c2", t0.Add(500*time.Millisecond))}, m.revokeEpoch.Load(), true, "test-x", false)
	revoked := map[string]bool{}
	for _, name := range revokesQueued(m) {
		revoked[name] = true
	}
	if !revoked["a"] {
		t.Fatalf("revoked %v, want a, whose window holds the cut call's request", revokesQueued(m))
	}
	got := rereadLines(logs)
	if len(got) != 1 || got[0]["left_out_from"] != t0.Add(2*time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("re-read %v once the cut drop moved the first past the start-up read, want one ERROR from its request", got)
	}
}

// keptLines returns the lines, at level, that say a test's mock lost revoked
// sent test cases a client that cannot delete them keeps.
func keptLines(logs *observer.ObservedLogs, level zapcore.Level) []map[string]interface{} {
	var out []map[string]interface{}
	for _, e := range logs.All() {
		if e.Message == keptSentMsg && e.Level == level {
			out = append(out, e.ContextMap())
		}
	}
	return out
}

// A test's mock no test owns, lost over test cases the stream sent, revokes
// them: a client that negotiated the revoke deletes them, and nothing more is
// said. One that cannot delete them keeps them, recorded without the mock:
// that is said at WARN, with how many, for the first such loss and the
// sampled ones (the rest at Debug, with the count so far), and never as a
// loss that leaves out the test cases after it. A loss that revokes none
// says nothing, nor does one that names none it cannot delete. A new
// recording counts from none.
func TestATestsMockLostSaysTheSentTestCasesAClientThatCannotDeleteKeeps(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	setup := func(capable bool) (*SyncMockManager, *observer.ObservedLogs) {
		core, logs := observer.New(zapcore.DebugLevel)
		m := New(zap.New(core))
		m.SetRevokeCapable(capable)
		m.SetOutputChannel(make(chan *models.Mock, 1))
		for i := 0; i < 3; i++ {
			at := t0.Add(time.Duration(i) * 10 * time.Second)
			m.SendingTestCase(fmt.Sprintf("tc-%d", i), at, at.Add(time.Second))
		}
		return m, logs
	}
	t.Run("a client that deletes them", func(t *testing.T) {
		m, logs := setup(true)
		m.dropOut(callAt("c1", t0.Add(500*time.Millisecond)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		if got := revokesQueued(m); len(got) != 1 || got[0] != "tc-0" {
			t.Fatalf("revoked %v, want [tc-0]", got)
		}
		if n := len(keptLines(logs, zapcore.WarnLevel)) + len(keptLines(logs, zapcore.DebugLevel)); n != 0 {
			t.Fatalf("%d lines say a client that deletes revoked test cases keeps them", n)
		}
	})
	t.Run("a client that cannot", func(t *testing.T) {
		m, logs := setup(false)
		m.dropOut(callAt("c1", t0.Add(30*time.Second+500*time.Millisecond)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		if n := len(keptLines(logs, zapcore.WarnLevel)) + len(keptLines(logs, zapcore.DebugLevel)); n != 0 || len(logs.FilterMessageSnippet("syncMock: a test's mock was dropped").All()) != 0 {
			t.Fatalf("a loss over no test case sent said %d kept lines", n)
		}
		m.dropOut(callAt("c1", t0.Add(500*time.Millisecond)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		w := keptLines(logs, zapcore.WarnLevel)
		if len(w) != 1 || w[0]["revoked_sent"] != int64(1) || w[0]["kept_sent_so_far"] != uint64(1) || w[0]["such_losses_so_far"] != uint64(1) || w[0]["client_deletes_revoked"] != false || w[0]["connID"] != "c1" {
			t.Fatalf("the WARN says %v", w)
		}
		if got := revokesQueued(m); len(got) != 0 {
			t.Fatalf("queued %v to revoke for a client that cannot delete them", got)
		}
		// A long call whose answer reaches tc-2's window revokes only tc-1,
		// whose window holds its request.
		long := callAt("c2", t0.Add(10*time.Second+500*time.Millisecond))
		long.Spec.ResTimestampMock = t0.Add(26 * time.Second)
		m.dropOut(long, "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		d := keptLines(logs, zapcore.DebugLevel)
		if len(keptLines(logs, zapcore.WarnLevel)) != 1 || len(d) != 1 || d[0]["revoked_sent"] != int64(1) || d[0]["kept_sent_so_far"] != uint64(2) || d[0]["such_losses_so_far"] != uint64(2) {
			t.Fatalf("the second such loss said WARN %v, Debug %v: want it at Debug, with 2 kept so far", keptLines(logs, zapcore.WarnLevel), d)
		}
		if leftOutLines(logs) != 0 {
			t.Fatal("a test's mock lost said the recording leaves out the test cases after it")
		}
		m.SetOutputChannel(make(chan *models.Mock, 1))
		m.SendingTestCase("next", t0.Add(40*time.Second), t0.Add(41*time.Second))
		m.dropOut(callAt("c1", t0.Add(40*time.Second+500*time.Millisecond)), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		if w := keptLines(logs, zapcore.WarnLevel); len(w) != 2 || w[1]["kept_sent_so_far"] != uint64(1) || w[1]["such_losses_so_far"] != uint64(1) {
			t.Fatalf("a new recording's first such loss said %v, want it counted from none", w)
		}
	})
	t.Run("one it cannot name, to a client that deletes them", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		m := New(zap.New(core))
		m.SetRevokeCapable(true)
		m.SetOutputChannel(make(chan *models.Mock, 1))
		at := t0.Add(time.Minute)
		for i := 0; i <= maxSentTCs; i++ {
			m.SendingTestCase(fmt.Sprintf("tc-%d", i), at.Add(-time.Second), at.Add(time.Second))
		}
		m.dropOut(callAt("c1", at), "", m.revokeEpoch.Load(), string(MockDroppedOutputFull))
		if len(logs.FilterField(zap.Int("sent_named_max", maxSentTCs)).All()) != 1 {
			t.Fatal("setup: the loss did not say the sent test cases it no longer names")
		}
		if n := len(keptLines(logs, zapcore.WarnLevel)) + len(keptLines(logs, zapcore.DebugLevel)); n != 0 {
			t.Fatalf("%d lines say a client that deletes revoked test cases keeps them", n)
		}
		if leftOutLines(logs) != 0 {
			t.Fatal("a test's mock lost said the recording leaves out the test cases after it")
		}
	})
}

// The kept WARN's text says the sent test cases stay recorded and may fail at
// replay, and does not say they are revoked from the recording.
func TestTheKeptWarnSaysTheTestCasesStay(t *testing.T) {
	if !strings.Contains(keptSentMsg, "stay recorded without it") || !strings.Contains(keptSentMsg, "no matching mock") {
		t.Fatalf("the kept WARN says %q", keptSentMsg)
	}
}
