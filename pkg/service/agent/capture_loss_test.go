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

// blindHooks count PID-map refusals but a count of theirs cannot be told
// (the UDP watch, its epoch or a report it could not read).
type blindHooks struct {
	countingHooks
	err error
}

func (h blindHooks) CaptureLossesErr() error { return h.err }

// A count the hooks could not start is unknown, never 0: the agent reads no
// capture losses (not "unsupported", which leaves every test as it was), so a
// session marked while it is fails every replayed test as one keploy cannot
// judge, as a mark that could not be read does.
func TestACaptureLossCountThatCouldNotStartIsUnknownNeverNone(t *testing.T) {
	ctx := context.Background()
	a, _ := wireAgent(t, models.MODE_TEST)
	a.Hooks = blindHooks{err: errors.New("no trampoline and no tracepoints")}
	_, err := a.CaptureLosses(ctx)
	require.Error(t, err)
	require.NotErrorIs(t, err, models.ErrCaptureLossesUnsupported, "unsupported leaves every test as it was")

	require.NoError(t, a.resetSession(ctx))
	claimSelf(t, a)
	e := endPassed(t, a, "TestA")
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.Contains(t, e.Reason, "no trampoline and no tracepoints")

	a.Hooks = blindHooks{}
	_, err = a.CaptureLosses(ctx)
	require.NoError(t, err, "a count that started")
}

// unixHooks are hooks that count PID-map refusals (none) and saw the
// application call over Unix sockets.
type unixHooks struct {
	countingHooks
	losses models.CaptureLosses
}

func (h unixHooks) CaptureLosses() models.CaptureLosses { return h.losses }

// Decision 20: a call over a Unix socket keploy cannot capture leaves the test
// open at the time, and every later one, without a verdict, never failed: the
// replayed test's outcome stands, and its window carries NotJudged. One the
// application made before the session began, as a connection pool opened at
// startup does, counts too. A real loss beside it still fails the test.
func TestACallKeployCannotCaptureLeavesTestsNotJudged(t *testing.T) {
	db := models.CaptureLosses{{Kind: models.CaptureLossUnixSocket, Count: 1, What: "/var/run/postgresql/.s.PGSQL.5432"}}

	// Opened at startup, before the session: every test is not judged.
	a, _ := wireAgent(t, models.MODE_TEST)
	a.Hooks = unixHooks{losses: db}
	claimSelf(t, a)
	for _, name := range []string{"TestA", "TestB"} {
		e := endPassed(t, a, name)
		require.Empty(t, e.Action, "%s: never failed for what keploy cannot capture", name)
	}
	for _, name := range []string{"o::TestA", "o::TestB"} {
		w := windowOf(t, a, name)
		require.Equal(t, models.ScopeOutcomePassed, w.Outcome)
		require.NotNil(t, w.Evidence)
		require.Equal(t, db, w.Evidence.NotJudged)
		require.Empty(t, w.Evidence.CaptureIncomplete)
		require.Contains(t, w.Evidence.Reason, "not judged")
		require.Contains(t, w.Evidence.Reason, "/var/run/postgresql/.s.PGSQL.5432")
	}

	// Opened mid-run: the tests before it are judged, untouched.
	a, _ = wireAgent(t, models.MODE_TEST)
	h := &switchingHooks{}
	a.Hooks = h
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestBefore").Action)
	h.losses = db
	require.Empty(t, endPassed(t, a, "TestAfter").Action)
	require.Nil(t, windowOf(t, a, "o::TestBefore").Evidence, "a test before the call is judged as it was")
	require.Equal(t, db, windowOf(t, a, "o::TestAfter").Evidence.NotJudged)

	// An allow-listed call (nscd) never reaches the count: every test judged.
	// (The hooks drop it: TestUnixAllowlist.)

	// A real loss beside it fails the test.
	a, _ = wireAgent(t, models.MODE_TEST)
	a.Hooks = unixHooks{losses: append(models.CaptureLosses{{Kind: models.CaptureLossParserStall, Count: 1}}, db...)}
	claimSelf(t, a)
	_, serr := a.ProvenBegin(context.Background(), begin("TestC"))
	require.Nil(t, serr)
	a.Hooks = unixHooks{losses: append(models.CaptureLosses{{Kind: models.CaptureLossParserStall, Count: 2}}, db...)}
	e, serr := a.ProvenEnd(context.Background(), models.ScopeReq{Name: "TestC", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action, "the parser stall is a real loss")
}

// switchingHooks are unixHooks whose count the test changes between tests.
type switchingHooks struct {
	countingHooks
	losses models.CaptureLosses
}

func (h *switchingHooks) CaptureLosses() models.CaptureLosses { return h.losses }

// drainingHooks record whether their reports were drained before their
// losses were read.
type drainingHooks struct {
	countingHooks
	err     error
	drained *bool
	order   *[]string
}

func (h drainingHooks) DrainCaptureLosses(context.Context) error {
	*h.order = append(*h.order, "drain")
	*h.drained = true
	return h.err
}

func (h drainingHooks) CaptureLosses() models.CaptureLosses {
	*h.order = append(*h.order, "read")
	return nil
}

// A read of the capture losses drains the hooks' reports first, so a call the
// application made before the read is in it; a drain that fails leaves the
// read unknown (not "unsupported", which leaves every test as it was).
func TestTheAgentDrainsTheHooksReportsBeforeItReadsThem(t *testing.T) {
	ctx := context.Background()
	a, _ := wireAgent(t, models.MODE_TEST)
	var order []string
	drained := false
	a.Hooks = drainingHooks{drained: &drained, order: &order}
	_, err := a.CaptureLosses(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"drain", "read"}, order)

	a.Hooks = drainingHooks{err: errors.New("the report ended"), drained: &drained, order: &order}
	_, err = a.CaptureLosses(ctx)
	require.Error(t, err)
	require.NotErrorIs(t, err, models.ErrCaptureLossesUnsupported)
}

// Unix sockets keploy cannot see leave every test not judged, never failed:
// the replayed test's outcome stands, its window carries unix_unseen.
func TestUnixSocketsKeployCannotSeeLeaveTestsNotJudged(t *testing.T) {
	unseen := models.CaptureLosses{{Kind: models.CaptureLossUnixUnseen, Count: 1, What: "no trampoline"}}
	a, _ := wireAgent(t, models.MODE_TEST)
	a.Hooks = unixHooks{losses: unseen}
	claimSelf(t, a)
	for _, name := range []string{"TestA", "TestB"} {
		require.Empty(t, endPassed(t, a, name).Action, "%s: never failed for what keploy cannot see", name)
	}
	for _, name := range []string{"o::TestA", "o::TestB"} {
		w := windowOf(t, a, name)
		require.Equal(t, models.ScopeOutcomePassed, w.Outcome)
		require.NotNil(t, w.Evidence)
		require.Equal(t, unseen, w.Evidence.NotJudged)
		require.Contains(t, w.Evidence.Reason, "not judged")
	}
}
