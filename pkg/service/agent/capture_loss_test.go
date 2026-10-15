//go:build linux

package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"go.keploy.io/server/v3/pkg/models"
)

func endPassed(t *testing.T, a *Agent, name string) models.ScopeEndResp {
	t.Helper()
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin(name))
	require.Nil(t, serr)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: name, Outcome: "passed"})
	require.Nil(t, serr)
	return e
}

// A process the kernel refused a place in the PID map made calls keploy never
// saw: the test open when it happened fails, and so does every test after it
// in the run (design v14 §3.2, taint). The test before it does not.
func TestACaptureLossFailsTheOpenTestAndEveryLaterOne(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestBefore").Action, "nothing lost yet")

	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestDuring"))
	require.Nil(t, serr)
	a.Hooks = countingHooks{refusals: models.PIDMapRefusals{Target: 3, LastTargetErr: -7, TargetCapacity: 10240}}
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestDuring", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "keploy: capture incomplete: the kernel refused 3 insert(s) into keploy's PID map target_namespace_pids")
	require.Contains(t, e.Reason, "the map had no room for it")
	require.NotContains(t, e.Reason, "quarantine", "keploy's own loss is not the test's to quarantine")
	require.NotContains(t, e.Reason, "re-record", "nor to re-record")

	after := endPassed(t, a, "TestAfter")
	require.Equal(t, models.ScopeActionFail, after.Action, "the taint charges every test that begins after the loss")

	// The windows last: reading them ends the run.
	w := windowOf(t, a, "o::TestDuring")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossPIDMap, Count: 3, What: "target_namespace_pids, E2BIG: the map had no room for it, usually because it is full"}}, w.Evidence.CaptureIncomplete)
	require.Equal(t, models.ScopeOutcomeFailed, windowOf(t, a, "o::TestAfter").Outcome)
	require.Nil(t, windowOf(t, a, "o::TestBefore").Evidence, "a test that ended before the loss is not charged")
}

// A loss between two tests charges the next test, not the one that ended.
func TestACaptureLossBetweenTestsChargesTheNext(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestA").Action)
	a.Hooks = countingHooks{refusals: models.PIDMapRefusals{Excluded: 1}}
	require.Equal(t, models.ScopeActionFail, endPassed(t, a, "TestB").Action)
	require.Nil(t, windowOf(t, a, "o::TestA").Evidence)
	ev := windowOf(t, a, "o::TestB").Evidence
	require.Contains(t, ev.Reason, "excluded_pids")
	// Its own kind and wording: a process that was not excluded, not one whose
	// calls were not intercepted.
	require.Equal(t, models.CaptureLossPIDMapExcluded, ev.CaptureIncomplete[0].Kind)
	require.Contains(t, ev.Reason, "predates the run was not excluded")
	require.NotContains(t, ev.Reason, "were not intercepted")
}

// What was lost before the run's session began is not the run's.
func TestACaptureLossBeforeTheSessionIsNotTheRuns(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	a.Hooks = countingHooks{refusals: models.PIDMapRefusals{Target: 9}}
	require.NoError(t, a.resetSession(context.Background()))
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestA").Action)
}

// A record never fails a test: the loss goes on the window, for judging, and
// the test's own outcome stands.
func TestARecordKeepsACaptureLossOnTheWindowAndNeverFails(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	claimSelf(t, a)
	a.Hooks = countingHooks{refusals: models.PIDMapRefusals{Target: 2}}
	e := endPassed(t, a, "TestA")
	require.Empty(t, e.Action)
	w := windowOf(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomePassed, w.Outcome)
	require.NotNil(t, w.Evidence)
	require.Len(t, w.Evidence.CaptureIncomplete, 1)
	require.Contains(t, w.Evidence.Reason, "capture incomplete")
}

// A test keploy cannot tell was fully captured is not judged as if it were:
// a failed read, or hooks that cannot count, fail it in a replay and are said
// on the window in a record.
func TestACaptureLossCountThatCannotBeReadFailsTheReplayedTest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hooks func(a *Agent)
		says  string
	}{
		{"a failed read", func(a *Agent) {
			a.Hooks = countingHooks{err: errors.New("bad file descriptor")}
		}, "bad file descriptor"},
		{"hooks that counted at the start and no longer can", func(a *Agent) { a.Hooks = uncountedHooks{} }, "cannot tell whether it captured every call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := wireAgent(t, models.MODE_TEST)
			claimSelf(t, a)
			tc.hooks(a)
			e := endPassed(t, a, "TestA")
			require.Equal(t, models.ScopeActionFail, e.Action)
			require.Contains(t, e.Reason, "keploy could not tell whether every call of o::TestA was captured")
			require.Contains(t, e.Reason, tc.says)
			require.Equal(t, models.ScopeOutcomeFailed, windowOf(t, a, "o::TestA").Outcome)
			require.True(t, windowOf(t, a, "o::TestA").Evidence.Unread, "no verdict of its own, as unread evidence")

			r, _ := wireAgent(t, models.MODE_RECORD)
			claimSelf(t, r)
			tc.hooks(r)
			require.Empty(t, endPassed(t, r, "TestA").Action)
			require.Contains(t, windowOf(t, r, "o::TestA").Evidence.Reason, tc.says)
		})
	}
}

// A session whose start could not read the counts: every test of it is one
// keploy cannot judge.
func TestACaptureLossMarkThatCouldNotBeReadFailsTheReplayedTests(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	a.Hooks = countingHooks{err: errors.New("not loaded")}
	require.NoError(t, a.resetSession(context.Background()))
	a.Hooks = countingHooks{}
	claimSelf(t, a)
	e := endPassed(t, a, "TestA")
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "at the start of the run")
}

// Hooks that never count (other platforms', which release 1 does not prove)
// leave every test as it was, as the PID-map check leaves a session.
func TestHooksThatNeverCountLeaveTheTestsAsTheyWere(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	a.Hooks = uncountedHooks{}
	require.NoError(t, a.resetSession(context.Background()))
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestA").Action)
}

// lossProxy reports the calls it could not capture, as the proxy does.
type lossProxy struct {
	*wireProxy
	losses models.CaptureLosses
}

func (p lossProxy) CaptureLosses() models.CaptureLosses { return p.losses }

// The agent's losses are the hooks' and the proxy's together; a proxy that
// counts is enough for an agent whose hooks cannot.
func TestCaptureLossesAreTheHooksAndTheProxys(t *testing.T) {
	ctx := context.Background()
	tls := models.CaptureLoss{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.1:443"}
	a, p := wireAgent(t, models.MODE_TEST)
	a.Proxy = lossProxy{wireProxy: p, losses: models.CaptureLosses{tls}}
	a.Hooks = countingHooks{refusals: models.PIDMapRefusals{Target: 2}}
	got, err := a.CaptureLosses(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, tls, got[0])
	require.Equal(t, models.CaptureLossPIDMap, got[1].Kind)
	require.Equal(t, uint64(2), got[1].Count)

	a.Hooks = uncountedHooks{}
	got, err = a.CaptureLosses(ctx)
	require.NoError(t, err, "a proxy that counts is enough")
	require.Equal(t, models.CaptureLosses{tls}, got)

	a.Proxy = p
	_, err = a.CaptureLosses(ctx)
	require.ErrorIs(t, err, models.ErrCaptureLossesUnsupported)
}

// A TLS handshake a client aborted against keploy's CA fails the replayed
// test running then, through the proxy's count.
func TestATLSAbortFailsTheReplayedTest(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	lp := lossProxy{wireProxy: p}
	a.Proxy = lp
	a.proven.losses = a.markCaptureLosses(context.Background())
	claimSelf(t, a)
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	lp.losses = models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.1:443"}}
	a.Proxy = lp
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "TLS handshake(s) with keploy's proxy failed")
}
