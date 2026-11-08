package manager

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// unnamedWarns counts the WARNs that say a test's mock was lost over sent
// test cases the stream cannot name, and returns the last one's fields.
func unnamedWarns(logs *observer.ObservedLogs) (int, map[string]interface{}) {
	n, last := 0, map[string]interface{}(nil)
	for _, e := range logs.All() {
		if f := e.ContextMap(); f["unnamed_sent_through"] != nil && e.Level == zapcore.WarnLevel {
			n, last = n+1, f
		}
	}
	return n, last
}

// The async capture hands the stream its test cases with no name: the
// client names each as it saves it. A test's mock no test owns, lost over the
// window of one the stream sent so, revokes it by the name a revoke gives it,
// its request time (models.RevokeNameByRequest), when the client finds it by
// that; with a client that does not, or for one whose window start the client
// cannot read (zero), no revoke can name it, and the loss says at WARN that it
// may stay recorded without the mock. (The recording's first test case, sent
// before, keeps the lost mock a test's: a loss that moved the first past it
// would re-read it as one any test may need.) A test case sent with no name counts as
// the recording's first like any other, and a revoke by name of one never
// sent leaves the first known.
func TestATestsMockLostOverATestCaseSentWithNoNameRevokesItByItsRequestTime(t *testing.T) {
	lose := func(t *testing.T, m *SyncMockManager, clk interface{ Advance(time.Duration) }, at time.Time) {
		t.Helper()
		mk := asyncAt("c1", at)
		mk.Spec.ResTimestampMock = at.Add(time.Second)
		lost := make(chan bool, 1)
		m.send(mk, "", func(d bool) { lost <- !d })
		clk.Advance(time.Hour) // the consumer took nothing for the bound
		if !<-lost {
			t.Fatal("a mock no consumer took was delivered")
		}
	}
	t.Run("a client that finds it by its request time", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		m := New(zap.New(core))
		m.SetRevokeCapable(true)
		m.SetRevokeByRequest(true)
		_, clk := fullLane(t, m)
		t0 := time.Now().Add(-time.Minute)
		sentFirstAt(m, t0)
		at := t0.Add(10 * time.Second)
		over := at.Add(-time.Second)
		if !m.SendingTestCase("", over, at.Add(time.Second)) || !m.SendingTestCase("", at.Add(2*time.Second), at.Add(3*time.Second)) {
			t.Fatal("a test case sent with no name was left out with no mock lost")
		}
		lose(t, m, clk, at)
		if got := revokesQueued(m); len(got) != 1 || got[0] != models.RevokeNameByRequest(over) {
			t.Fatalf("revoked %v, want only %q: the test case over the lost call, by its request time", got, models.RevokeNameByRequest(over))
		}
		if n, f := unnamedWarns(logs); n != 0 {
			t.Fatalf("a loss whose test case it revoked by its request time says one may stay: %v", f)
		}
	})
	for _, c := range []struct {
		name     string
		byReq    bool
		start    func(at time.Time) time.Time
		lostThru func(at time.Time) time.Time
	}{
		{"a client that does not find it by its request time", false, func(at time.Time) time.Time { return at.Add(-time.Second) }, func(at time.Time) time.Time { return at.Add(time.Second) }},
		{"a test case with no window start", true, func(time.Time) time.Time { return time.Time{} }, func(at time.Time) time.Time { return at.Add(time.Second) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			m := New(zap.New(core))
			m.SetRevokeCapable(true)
			m.SetRevokeByRequest(c.byReq)
			_, clk := fullLane(t, m)
			t0 := time.Now().Add(-time.Minute)
			sentFirstAt(m, t0)
			at := t0.Add(10 * time.Second)
			if !m.SendingTestCase("", c.start(at), at.Add(time.Second)) {
				t.Fatal("a test case sent with no name was left out with no mock lost")
			}
			lose(t, m, clk, at)
			if got := revokesQueued(m); len(got) != 0 {
				t.Fatalf("revoked %v of a test case no revoke can name", got)
			}
			n, f := unnamedWarns(logs)
			if n != 1 || f["unnamed_sent_through"] != c.lostThru(at).Format(time.RFC3339Nano) {
				t.Fatalf("%d WARNs say a sent test case may stay without the mock (last %v), want 1 through its window end", n, f)
			}
		})
	}
	t.Run("a test case of several managers", func(t *testing.T) {
		m := New(nil)
		m.SetRevokeCapable(true)
		m.SetRevokeByRequest(true)
		other := New(nil)
		_, clk := fullLane(t, m)
		t0 := time.Now().Add(-time.Minute)
		sentFirstAt(m, t0)
		at := t0.Add(10 * time.Second)
		over := at.Add(-time.Second)
		if !SendingTestCaseOf([]*SyncMockManager{m, other}, "", over, at.Add(time.Second)) {
			t.Fatal("a test case sent with no name was left out with no mock lost")
		}
		lose(t, m, clk, at)
		if got := revokesQueued(m); len(got) != 1 || got[0] != models.RevokeNameByRequest(over) {
			t.Fatalf("revoked %v, want only %q", got, models.RevokeNameByRequest(over))
		}
		other.sharedLossMu.Lock()
		named := len(other.sentTCs.tcs)
		other.sharedLossMu.Unlock()
		if named != 0 {
			t.Fatalf("a manager whose client does not revoke by request time names %d test cases sent with no name, want 0", named)
		}
	})
	t.Run("the first test case", func(t *testing.T) {
		m := New(nil)
		t0 := time.Now().Add(-time.Minute)
		m.SendingTestCase("", t0, t0.Add(time.Millisecond))
		if m.servedToAnyTest(asyncAt("c1", t0.Add(time.Second)), "") {
			t.Fatal("a start-up mock after the only test case sent, one with no name, is read as one any test may need")
		}
		m.RevokeTestCase("never-sent")
		if m.servedToAnyTest(asyncAt("c1", t0.Add(time.Second)), "") {
			t.Fatal("a revoke by name of a test case never sent made the first test case unknown")
		}
		if !m.servedToAnyTest(asyncAt("c1", t0.Add(-time.Second)), "") {
			t.Fatal("a start-up mock before the first test case, one sent with no name, is not one any test may need")
		}
	})
}
