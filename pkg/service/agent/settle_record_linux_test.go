//go:build linux

package agent

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

// errBytesUnknown is what a proxy answers when it cannot tell whether the
// bytes the application wrote to a connection reached it (the wait for the
// paired bytes: an application socket gone with a reset whose sent bytes
// cannot be read, a kept socket whose end cannot).
var errBytesUnknown = errors.New("keploy cannot tell what the application had sent on a connection")

// unknownAt makes the proxy's wait for the paired bytes answer
// errBytesUnknown at the settles the test names, by their order (1: the
// first settle), and settle at the others.
func unknownAt(p *settledProxy, at ...int) {
	var n atomic.Int32
	p.bytesWait = func(context.Context, uint64) error {
		i := int(n.Add(1))
		for _, a := range at {
			if a == i {
				return errBytesUnknown
			}
		}
		return nil
	}
}

// liveWindow is the latest window of id, read as the run goes on (windowOf
// reads the run's final windows, which ends it).
func liveWindow(t *testing.T, a *Agent, id string) models.ScopeWindow {
	t.Helper()
	ws, err := a.GetScopeWindows(context.Background())
	require.NoError(t, err)
	for i := len(ws) - 1; i >= 0; i-- {
		if ws[i].Name == id {
			return ws[i]
		}
	}
	t.Fatalf("no window %q in %+v", id, ws)
	return models.ScopeWindow{}
}

// unsettledOf is the calls_unsettled charge on the window of id, if any.
func unsettledOf(t *testing.T, a *Agent, id string) (models.CaptureLoss, bool) {
	t.Helper()
	w := liveWindow(t, a, id)
	if w.Evidence == nil {
		return models.CaptureLoss{}, false
	}
	for _, c := range w.Evidence.CaptureIncomplete {
		if c.Kind == models.CaptureLossUnsettled {
			return c, true
		}
	}
	return models.CaptureLoss{}, false
}

// unsettledCount is the agent's count of calls_unsettled, which the CLI's
// capture watch reads (GetCaptureLosses).
func unsettledCount(t *testing.T, a *Agent) uint64 {
	t.Helper()
	l, err := a.CaptureLosses(context.Background())
	require.NoError(t, err)
	for _, c := range l {
		if c.Kind == models.CaptureLossUnsettled {
			return c.Count
		}
	}
	return 0
}

// A record never fails a test for what keploy could not tell at its begin or
// its end: the begin opens the test and the end answers it as the test said,
// and the test is charged calls_unsettled, a judged loss of its own, which
// the pass that judges each test decides (it replays the test). The next
// test is not charged it.
func TestARecordsSettleThatCannotTellIsTheTestsEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		at      []int // the settles that cannot tell: 1 TestA's begin, 2 its end
		outcome string
		what    string
		count   uint64
	}{
		{"at the begin", []int{1}, models.ScopeOutcomePassed, "before o::TestA began: " + errBytesUnknown.Error(), 1},
		{"at the end", []int{2}, models.ScopeOutcomePassed, "o::TestA: " + errBytesUnknown.Error(), 1},
		{"at both", []int{1, 2}, models.ScopeOutcomePassed, "o::TestA: " + errBytesUnknown.Error(), 2},
		{"of a test that failed", []int{2}, models.ScopeOutcomeFailed, "o::TestA: " + errBytesUnknown.Error(), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p := wireAgent(t, models.MODE_RECORD)
			claimSelf(t, a)
			unknownAt(&p.settledProxy, tc.at...)
			ctx := context.Background()
			_, serr := a.ProvenBegin(ctx, begin("TestA"))
			require.Nil(t, serr, "a record's begin opens the test")
			e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: tc.outcome})
			require.Nil(t, serr, "a record's end answers the test")
			require.Empty(t, e.Action, e.Reason)

			w := liveWindow(t, a, "o::TestA")
			require.Equal(t, tc.outcome, w.Outcome, "the test's outcome is its own")
			require.NotNil(t, w.Evidence)
			require.False(t, w.Evidence.Unread, "read: the replay that judges it decides")
			c, ok := unsettledOf(t, a, "o::TestA")
			require.True(t, ok, "charged: %+v", w.Evidence)
			require.Equal(t, tc.count, c.Count)
			require.Equal(t, tc.what, c.What)
			require.Contains(t, w.Evidence.Reason, "could not tell")
			require.Equal(t, tc.count, unsettledCount(t, a), "the CLI's capture watch reads it too")

			_ = endPassed(t, a, "TestB")
			_, ok = unsettledOf(t, a, "o::TestB")
			require.False(t, ok, "the next test is not charged TestA's")
		})
	}
}

// A replay's begin or end that cannot tell fails the test as before: the
// begin refuses it, the end fails it unread. Nothing is charged.
func TestAReplaysSettleThatCannotTellStillFailsTheTest(t *testing.T) {
	a, p := exactAgent(t, nil, map[string][]string{"o::TestA": {}, "o::TestB": {}}, map[string][]string{})
	ctx := context.Background()
	unknownAt(&p.settledProxy, 1, 3)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.NotNil(t, serr, "a replay's begin keploy cannot settle is refused")
	require.Equal(t, http.StatusInternalServerError, serr.Status)
	require.Contains(t, serr.Message, "keploy cannot judge o::TestA: "+errBytesUnknown.Error())

	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.NotNil(t, serr)
	w := liveWindow(t, a, "o::TestB")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.True(t, w.Evidence.Unread)
	require.Contains(t, w.Evidence.Reason, "could not tell that every call of o::TestB reached it: "+errBytesUnknown.Error())
	_, ok := unsettledOf(t, a, "o::TestB")
	require.False(t, ok)
	require.Zero(t, unsettledCount(t, a))
}

// A record's settle that could not count the calls read no drops: the test's
// end compares none with it, so packets dropped before the test began are
// not its. One that could not tell only the bytes did read them: packets
// dropped while the test ran are its, charged at its end.
func TestARecordsBeginThatCannotTellKeepsTheDropsItRead(t *testing.T) {
	t.Run("a begin that could not count", func(t *testing.T) {
		a, p := wireAgent(t, models.MODE_RECORD)
		claimSelf(t, a)
		_ = endPassed(t, a, "TestA")
		p.smu.Lock()
		p.drops = 3 // dropped between the tests
		p.smu.Unlock()
		h := a.Hooks.(*countHooks)
		h.mu.Lock()
		h.err = models.ErrProxyConnsUnsupported
		h.mu.Unlock()
		ctx := context.Background()
		_, serr := a.ProvenBegin(ctx, begin("TestB"))
		require.Nil(t, serr)
		h.mu.Lock()
		h.err = nil
		h.mu.Unlock()
		_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", CanSkip: true, Outcome: models.ScopeOutcomePassed})
		require.Nil(t, serr)
		c, ok := unsettledOf(t, a, "o::TestB")
		require.True(t, ok)
		require.Equal(t, uint64(1), c.Count, "the begin's, and no drops: %s", c.What)
		require.Contains(t, c.What, "before o::TestB began")
	})
	t.Run("a begin that could not tell the bytes", func(t *testing.T) {
		a, p := wireAgent(t, models.MODE_RECORD)
		claimSelf(t, a)
		p.smu.Lock()
		p.drops = 3 // dropped before the test
		p.smu.Unlock()
		unknownAt(&p.settledProxy, 1)
		ctx := context.Background()
		_, serr := a.ProvenBegin(ctx, begin("TestA"))
		require.Nil(t, serr)
		p.smu.Lock()
		p.drops = 4 // dropped while it ran
		p.smu.Unlock()
		_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
		require.Nil(t, serr)
		c, ok := unsettledOf(t, a, "o::TestA")
		require.True(t, ok)
		require.Equal(t, uint64(2), c.Count, "the begin's, and the drop at the end")
		require.Contains(t, c.What, "the kernel dropped 1 packet(s) at keploy's proxy listener while o::TestA ran")
	})
}

// A begin or an end whose request has gone (its context done) is not a
// settle that could not tell: it is answered as before, and charges nothing.
func TestARecordsSettleForARequestThatHasGoneChargesNothing(t *testing.T) {
	for _, at := range []string{"begin", "end"} {
		t.Run(at, func(t *testing.T) {
			a, p := wireAgent(t, models.MODE_RECORD)
			claimSelf(t, a)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var n atomic.Int32
			p.bytesWait = func(ctx context.Context, _ uint64) error {
				if (n.Add(1) == 1) == (at == "begin") {
					cancel()
					return ctx.Err()
				}
				return nil
			}
			_, serr := a.ProvenBegin(ctx, begin("TestA"))
			if at == "begin" {
				require.NotNil(t, serr)
			} else {
				require.Nil(t, serr)
				_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
				require.NotNil(t, serr)
			}
			require.Zero(t, unsettledCount(t, a))
		})
	}
}

// An agent that counts no capture loss cannot charge the test with it: its
// record fails the test that keploy could not settle, as before.
func TestARecordThatCountsNoCaptureLossFailsWhatItCannotSettle(t *testing.T) {
	a, p := wireAgent(t, models.MODE_RECORD)
	a.Hooks = uncountedHooks{}
	a.proven.losses = a.markCaptureLosses(context.Background())
	require.ErrorIs(t, a.proven.losses.err, models.ErrCaptureLossesUnsupported)
	claimSelf(t, a)
	unknownAt(&p.settledProxy, 2)
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.NotNil(t, serr)
	w := liveWindow(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.True(t, w.Evidence.Unread)
}

// A record's settle that could not tell the bytes on a connection goes on to
// the calls in flight, and answers once none is, with what it could not tell
// and the drops it read: a call the proxy still holds is the test's own. A
// replay's answers at once, the proxy's error as it was.
func TestARecordsSettleGoesOnPastBytesItCannotTell(t *testing.T) {
	a, _, p := settleAgent()
	a.config = &config.Config{}
	a.config.Agent.Mode = models.MODE_RECORD
	p.bytesWait = func(context.Context, uint64) error { return errBytesUnknown }
	p.smu.Lock()
	p.drops = 7
	p.smu.Unlock()
	p.inWork = make(chan struct{})
	p.set(0, 1) // a call in the proxy's hands
	type answer struct {
		drops uint64
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		d, err := a.settle(context.Background(), false)
		done <- answer{d, err}
	}()
	select {
	case <-p.inWork: // past the bytes, at the calls in flight
	case got := <-done:
		t.Fatalf("settled before it looked at the calls in flight: %+v", got)
	}
	// It looks again at each change, and goes on waiting while a call is in
	// flight: a settle that answered after one look would answer first.
	looks := make(chan struct{}, 1)
	p.smu.Lock()
	p.looks = looks
	p.smu.Unlock()
	p.set(0, 1)
	select {
	case <-looks:
	case got := <-done:
		t.Fatalf("settled with a call in flight: %+v", got)
	}
	p.set(0, 0)
	got := <-done
	require.ErrorIs(t, got.err, errBytesUnknown)
	require.True(t, dropsRead(got.err))
	require.Equal(t, uint64(7), got.drops)

	a.config.Agent.Mode = models.MODE_TEST
	p.set(0, 1)
	_, err := a.settle(context.Background(), true)
	require.Same(t, errBytesUnknown, err, "a replay's: the proxy's error, as it was")
	require.False(t, dropsRead(err))
}

// Only a record (models.MODE_RECORD) takes what its settle cannot tell as
// the test's evidence, and goes on past bytes it cannot tell: an agent in no
// mode settles, and fails what it cannot settle, as a replay does.
func TestOnlyARecordTakesWhatItCannotSettleAsEvidence(t *testing.T) {
	for _, tc := range []struct {
		mode     models.Mode
		evidence bool
	}{{models.MODE_RECORD, true}, {models.MODE_TEST, false}, {models.MODE_OFF, false}} {
		t.Run(string(tc.mode), func(t *testing.T) {
			a, _ := wireAgent(t, tc.mode)
			if got := a.unsettledIsEvidence(context.Background()); got != tc.evidence {
				t.Fatalf("unsettledIsEvidence = %v, want %v", got, tc.evidence)
			}
			s, _, p := settleAgent()
			s.config = a.config
			p.bytesWait = func(context.Context, uint64) error { return errBytesUnknown }
			_, err := s.settle(context.Background(), false)
			var bu pairedBytesUnknown
			if errors.As(err, &bu) != tc.evidence {
				t.Fatalf("settle = %v: went on past the bytes %v, want %v", err, !tc.evidence, tc.evidence)
			}
			if !tc.evidence {
				require.Same(t, errBytesUnknown, err, "the proxy's error, as it was")
			}
		})
	}
}
