//go:build linux

package agent

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
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
	// A refusal of either map: an excluded one's process outlives the
	// application, but one an earlier session of this agent counted is not
	// this run's either.
	for _, refusals := range []models.PIDMapRefusals{{Target: 9}, {Excluded: 9}} {
		a, _ := wireAgent(t, models.MODE_TEST)
		a.Hooks = countingHooks{refusals: refusals}
		require.NoError(t, a.resetSession(context.Background()))
		claimSelf(t, a)
		e := endPassed(t, a, "TestA")
		require.Empty(t, e.Action, "%+v before the session failed its first test: %s", refusals, e.Reason)
	}
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

// lossReporter is a downstream build's own capture-loss count
// (coreAgent.RegisterCaptureLossReporter).
type lossReporter struct{ losses *models.CaptureLosses }

func (r lossReporter) CaptureLosses() models.CaptureLosses { return *r.losses }

// A downstream build's own count is the agent's too: a read of the agent's
// losses has it beside the proxy's and the hooks'. A kernel's count of skipped
// runs (a call program's, a connection program's) is evidence only, never a
// verdict (the founder's rule, 2026-10-21): every replayed test, during the
// skips and after them, is judged as it would be without them, as on a kernel
// without the count (5.10).
func TestADownstreamBuildsLossesAreTheAgentsToo(t *testing.T) {
	ctx := context.Background()
	var own models.CaptureLosses
	t.Cleanup(coreAgent.RegisterCaptureLossReporter(lossReporter{&own}))
	a, _ := wireAgent(t, models.MODE_TEST)
	a.Hooks = countingHooks{}
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestBefore").Action)
	own = models.CaptureLosses{{Kind: models.CaptureLossKernelSkipCall, Count: 2, What: "keploy_tcp_recvmsg_exit"}}
	got, err := a.CaptureLosses(ctx)
	require.NoError(t, err)
	require.Equal(t, own, got)
	require.Empty(t, endPassed(t, a, "TestDuring").Action, "a skip is never a failure")
	require.Empty(t, endPassed(t, a, "TestAfter").Action)
	own = append(own, models.CaptureLoss{Kind: models.CaptureLossKernelSkip, Count: 1, What: "keploy_tcp_connect"})
	require.Empty(t, endPassed(t, a, "TestConnSkipped").Action)
	require.Empty(t, endPassed(t, a, "TestLater").Action)
	for _, name := range []string{"o::TestBefore", "o::TestDuring", "o::TestAfter", "o::TestConnSkipped", "o::TestLater"} {
		require.Nil(t, windowOf(t, a, name).Evidence, "%s: a kernel's skip count is evidence, never a verdict: the test is judged as without it", name)
	}
}

// blindReporter is a downstream build's count that could not be read.
type blindReporter struct{ lossReporter }

func (blindReporter) CaptureLossesErr() error {
	return errors.New("the ring's drop count could not be read")
}

// Two sources that count one kind are one count: a mark is read by kind, so a
// second entry of a kind would hide the first's growth, or charge a test
// with a count that did not grow. And a downstream count that cannot be read
// leaves the agent's losses unknown, never 0.
func TestTwoSourcesOfOneKindAreOneCount(t *testing.T) {
	ctx := context.Background()
	own := models.CaptureLosses{{Kind: models.CaptureLossKernelSkip, Count: 5, What: "keploy_tcp_recvmsg 5"}, {Kind: models.CaptureLossDropped, Count: 9}}
	t.Cleanup(coreAgent.RegisterCaptureLossReporter(lossReporter{&own}))
	a, p := wireAgent(t, models.MODE_TEST)
	a.Proxy = lossProxy{wireProxy: p, losses: models.CaptureLosses{{Kind: models.CaptureLossDropped, Count: 3}}}
	a.Hooks = unixHooks{losses: models.CaptureLosses{{Kind: models.CaptureLossKernelSkip, Count: 2, What: "k_unix_stream_connect 2"}}}
	got, err := a.CaptureLosses(ctx)
	require.NoError(t, err)
	require.Equal(t, models.CaptureLosses{
		{Kind: models.CaptureLossDropped, Count: 12},
		{Kind: models.CaptureLossKernelSkip, Count: 7, What: "k_unix_stream_connect 2; keploy_tcp_recvmsg 5"},
	}, got)
	// Nothing grew: nothing is charged.
	require.Empty(t, got.Charged(models.SameBases(got), got))
	own[0].Count++
	again, err := a.CaptureLosses(ctx)
	require.NoError(t, err)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossKernelSkip, Count: 1, What: "k_unix_stream_connect 2; keploy_tcp_recvmsg 5"}}, again.Charged(models.SameBases(got), got))

	// With hooks and a proxy that count nothing, the reporter's count is
	// the agent's: never "unsupported".
	a.Proxy, a.Hooks = p, uncountedHooks{}
	got, err = a.CaptureLosses(ctx)
	require.NoError(t, err)
	require.Equal(t, own, got)

	unregister := coreAgent.RegisterCaptureLossReporter(blindReporter{lossReporter{&own}})
	t.Cleanup(unregister)
	_, err = a.CaptureLosses(ctx)
	require.ErrorContains(t, err, "could not be read")
	unregister()
	_, err = a.CaptureLosses(ctx)
	require.NoError(t, err, "unregistered, it is read no more")
}

// failingReadReporter is a downstream count whose read fails, and which says
// so only once it has read, as the hooks' skip watch does.
type failingReadReporter struct{ read *bool }

func (r failingReadReporter) CaptureLosses() models.CaptureLosses {
	*r.read = true
	return nil
}

func (r failingReadReporter) CaptureLossesErr() error {
	if *r.read {
		return errors.New("this read of the count failed")
	}
	return nil
}

// A downstream count's error is asked for after its read: the failure of the
// read under way is this read's, never missed for an earlier one's state.
func TestADownstreamCountsFailedReadIsThisReadsError(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_TEST)
	read := false
	t.Cleanup(coreAgent.RegisterCaptureLossReporter(failingReadReporter{&read}))
	_, err := a.CaptureLosses(context.Background())
	require.ErrorContains(t, err, "this read of the count failed")
}

// A TLS client's handshake that failed against keploy's CA fails the
// replayed test running then, through the proxy's count.
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
	require.Contains(t, e.Reason, "1 TLS handshake(s) with keploy's proxy failed")
}

// One connection's lost bytes are charged to one test: the test the loss
// landed in. A test that ran clean after it is not failed with it, and is not
// set aside: keploy answered 502 Connection: close on a read the test had no
// recording for, one such connection's loss failed that test and the next,
// which made no failing call, and the read's move was never seen. One between
// two tests is the next test's, as a miss between tests is. The run's count
// holds each loss once.
func TestLostBytesAreChargedToOneTest(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode models.Mode
		// fails: the test charged fails (a replay); a record keeps the
		// evidence on the window alone.
		fails bool
	}{
		{"replay", models.MODE_TEST, true},
		{"record", models.MODE_RECORD, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p := wireAgent(t, tc.mode)
			lp := lossProxy{wireProxy: p}
			a.Proxy = lp
			a.proven.losses = a.markCaptureLosses(context.Background())
			claimSelf(t, a)
			lost := func(n uint64) {
				lp.losses = models.CaptureLosses{{Kind: models.CaptureLossAppBytesLost, Count: n, What: "from [::ffff:127.0.0.1]:50912"}}
				a.Proxy = lp
			}
			charged := func(name string, want bool) {
				t.Helper()
				w := windowOf(t, a, "o::"+name)
				var got models.CaptureLosses
				if want {
					got = models.CaptureLosses{{Kind: models.CaptureLossAppBytesLost, Count: 1, What: "from [::ffff:127.0.0.1]:50912"}}
				}
				if w.Evidence == nil {
					require.False(t, want, "%s holds no evidence", name)
					return
				}
				require.Equal(t, got, w.Evidence.CaptureIncomplete, name)
			}
			action := ""
			if tc.fails {
				action = models.ScopeActionFail
			}
			ctx := context.Background()
			_, serr := a.ProvenBegin(ctx, begin("TestNext"))
			require.Nil(t, serr)
			lost(1)
			e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestNext", Outcome: "failed"})
			require.Nil(t, serr)
			require.Equal(t, action, e.Action, "the test the loss landed in")
			require.Empty(t, endPassed(t, a, "TestLast").Action, "a later test is not charged with it")

			lost(2) // between two tests
			require.Equal(t, action, endPassed(t, a, "TestAfter").Action, "a loss between tests is the next test's")
			require.Empty(t, endPassed(t, a, "TestEnd").Action)

			charged("TestNext", true)
			charged("TestLast", false)
			charged("TestAfter", true)
			charged("TestEnd", false)
		})
	}
}

// A TLS client's rejection of keploy's certificate is the test's it happened
// in, not a taint of every later test: the client's other calls are captured,
// or rejected, on their own. One between two tests is the next test's, as a
// miss between tests is. It failed the open test and every later one.
func TestATLSRejectionFailsOnlyTheTestItHappenedIn(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	lp := lossProxy{wireProxy: p}
	a.Proxy = lp
	a.proven.losses = a.markCaptureLosses(context.Background())
	claimSelf(t, a)
	rejected := func(n uint64) {
		lp.losses = models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: n, What: "to 10.0.0.1:443"}}
		a.Proxy = lp
	}
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestDuring"))
	require.Nil(t, serr)
	rejected(1)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestDuring", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action, "the test the rejection happened in fails")
	require.Empty(t, endPassed(t, a, "TestAfter").Action, "a later test is not charged with it")

	rejected(2) // between two tests
	require.Equal(t, models.ScopeActionFail, endPassed(t, a, "TestNext").Action, "a rejection between tests is the next test's")
	require.Empty(t, endPassed(t, a, "TestLast").Action)

	w := windowOf(t, a, "o::TestNext")
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.1:443"}}, w.Evidence.CaptureIncomplete, "one rejection, the one it was charged")
	require.Nil(t, windowOf(t, a, "o::TestAfter").Evidence)
}

// A failed handshake after one test process's last test and before the next
// one claims the run is the run's (its own verdict counts every loss while
// the command ran), as the calls it misses then are: not the next process's
// first test's.
func TestATLSRejectionBeforeATestProcessClaimsIsNotItsFirstTests(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	lp := lossProxy{wireProxy: p}
	a.Proxy = lp
	a.proven.losses = a.markCaptureLosses(context.Background())
	ctx := context.Background()
	first := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "a"}, first.Process.Pid)
	require.Nil(t, serr)
	require.Empty(t, endPassed(t, a, "TestFirst").Action)
	kill(t, first)

	lp.losses = models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.1:443"}}
	a.Proxy = lp
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestSecond").Action, "the next process's first test is not charged a loss before its claim")
	require.Nil(t, windowOf(t, a, "o::TestSecond").Evidence)
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
		require.Equal(t, db, w.Evidence.CannotCapture)
		require.Empty(t, w.Evidence.CaptureIncomplete)
		require.Contains(t, w.Evidence.Reason, "not judged")
		require.Contains(t, w.Evidence.Reason, "/var/run/postgresql/.s.PGSQL.5432")
	}

	// Opened before the session's mark (a pool opened as the application
	// started, the session's reset after it): the mark has it, and every test
	// is still not judged, as what keploy cannot capture is counted from the
	// hooks' load.
	a, _ = wireAgent(t, models.MODE_TEST)
	a.Hooks = unixHooks{losses: db}
	require.NoError(t, a.resetSession(context.Background()))
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestMarked").Action)
	require.Equal(t, db, windowOf(t, a, "o::TestMarked").Evidence.CannotCapture,
		"a Unix-socket call from before the session's mark is still every test's")

	// Opened mid-run: the tests before it are judged, untouched.
	a, _ = wireAgent(t, models.MODE_TEST)
	h := &switchingHooks{}
	a.Hooks = h
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestBefore").Action)
	h.losses = db
	require.Empty(t, endPassed(t, a, "TestAfter").Action)
	require.Nil(t, windowOf(t, a, "o::TestBefore").Evidence, "a test before the call is judged as it was")
	require.Equal(t, db, windowOf(t, a, "o::TestAfter").Evidence.CannotCapture)

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
	unseen := models.CaptureLosses{{Kind: models.CaptureLossUnixUnseen, Count: 1, What: "keploy's report stopped: ring broken"}}
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
		require.Equal(t, unseen, w.Evidence.CannotCapture)
		require.Contains(t, w.Evidence.Reason, "not judged")
	}
}

// A failed handshake is charged only to the test it happened in, so what no
// test was charged with (UnchargedCaptureLosses) counts each test's own: two
// tests' handshakes are both charged, not only the larger of the two; one
// before a test process's claim, which the claim keeps from its first test,
// is charged to none, and is the run's.
func TestUnchargedCaptureLossesCountEachTestsOwnHandshakes(t *testing.T) {
	ctx := context.Background()
	a, p := wireAgent(t, models.MODE_TEST)
	lp := lossProxy{wireProxy: p}
	a.Proxy = lp
	a.proven.losses = a.markCaptureLosses(ctx)
	rejected := func(n uint64) {
		lp.losses = models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: n, What: "to 10.0.0.1:443"}}
		a.Proxy = lp
	}
	first := child(t)
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "a"}, first.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestOne"))
	require.Nil(t, serr)
	rejected(1)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestOne", Outcome: "passed"})
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestTwo"))
	require.Nil(t, serr)
	rejected(2)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestTwo", Outcome: "passed"})
	require.Nil(t, serr)
	un, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "each test was charged its own handshake")

	kill(t, first)
	rejected(3) // before the next process claims the run
	claimSelf(t, a)
	require.Empty(t, endPassed(t, a, "TestThree").Action)
	un, err = a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: 1, What: "to 10.0.0.1:443"}}, un, "the handshake before the claim was charged to no test")
}

// A client that abandoned its TLS handshake without a word once keploy's
// certificate was out may have rejected it (Node.js does so): the test it
// happened in is not judged -- never failed, the abandon on its window -- and
// a later test is not charged with it.
func TestAnAbandonedHandshakeLeavesOnlyItsTestNotJudged(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	lp := lossProxy{wireProxy: p}
	a.Proxy = lp
	a.proven.losses = a.markCaptureLosses(context.Background())
	claimSelf(t, a)
	ctx := context.Background()
	abandoned := models.CaptureLosses{{Kind: models.CaptureLossTLSAbandoned, Count: 1, What: "to 10.0.0.1:443"}}
	_, serr := a.ProvenBegin(ctx, begin("TestDuring"))
	require.Nil(t, serr)
	lp.losses = abandoned
	a.Proxy = lp
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestDuring", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, e.Action, "not judged is never a failure")
	require.Empty(t, endPassed(t, a, "TestAfter").Action)

	w := windowOf(t, a, "o::TestDuring")
	require.Equal(t, models.ScopeOutcomePassed, w.Outcome)
	require.NotNil(t, w.Evidence)
	require.Equal(t, abandoned, w.Evidence.CannotCapture)
	require.Empty(t, w.Evidence.CaptureIncomplete)
	require.Contains(t, w.Evidence.Reason, "NODE_EXTRA_CA_CERTS")
	require.Nil(t, windowOf(t, a, "o::TestAfter").Evidence, "a later test is not charged with it")
	un, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "it was charged to its test")
}

// stalledIndex is a container index whose read of the container at addr
// stalled: the daemon did not answer it (containerkey's stallResolver).
type stalledIndex struct {
	addr string
	c    containerkey.Container
}

func (r stalledIndex) Lookup(dst netip.AddrPort) (containerkey.Container, bool, error) {
	return r.c, dst.String() == r.addr, nil
}
func (stalledIndex) Fingerprint(context.Context, string) (string, bool) { return "", false }
func (stalledIndex) Pending(netip.AddrPort) ([]string, bool)            { return nil, true }
func (stalledIndex) AwaitReadsOf(context.Context, []string)             {}
func (stalledIndex) Bind(context.Context)                               {}
func (stalledIndex) Watch(context.Context, *zap.Logger)                 {}
func (stalledIndex) Endpoint() string                                   { return "unix:///fake.sock" }
func (stalledIndex) FingerprintsOrStall(context.Context, string) (string, string, bool, string) {
	return "", "", false, "pg (postgres:16)"
}

// In keploy mock record, a mock stamped after its test ended (the read of
// its container's configuration stalled past the test's end) charges that
// test, by the scope its request was sent in: its window says it is not
// judged, naming the container, and the next test, open as the stall came,
// is untouched, as is the run's. The count of the kind charges nothing.
func TestARecordChargesALateUnreadMockToItsOwnTestAndNoOther(t *testing.T) {
	saved := containerkey.Default
	t.Cleanup(func() { containerkey.Default = saved })
	containerkey.Default = containerkey.NewKeyer(stalledIndex{addr: "172.17.0.7:5432", c: containerkey.Container{ID: "pg", Image: "postgres:16", Port: 5432}})
	ctx := context.Background()
	a, p := wireAgent(t, models.MODE_RECORD)
	require.NoError(t, a.resetSession(ctx))
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	sent := time.Now() // TestA's request to pg
	containerkey.Default.Name(nil, "c1", "172.17.0.7:5432")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	// The stall resolves now, with TestB open: TestA's mock is stamped.
	m := &models.Mock{ConnectionID: "c1", Spec: models.MockSpec{ReqTimestampMock: sent}}
	containerkey.Default.Stamp(m)
	require.Equal(t, "container pg (postgres:16)", m.Spec.Metadata[containerkey.UnreadKey])
	// The count of the kind grew too, as the use was counted: no test is
	// charged by it.
	a.Proxy = lossProxy{wireProxy: p, losses: models.CaptureLosses{{Kind: models.CaptureLossContainerUnread, Count: 1, What: "container pg (postgres:16)"}}}
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: "passed"})
	require.Nil(t, serr)
	uncharged, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, uncharged, "the run is charged nothing: the mock is its own test's")
	// One of the time outside tests is the run's.
	outside := &models.Mock{ConnectionID: "c1", Spec: models.MockSpec{ReqTimestampMock: time.Now()}}
	containerkey.Default.Stamp(outside)
	uncharged, err = a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossContainerUnread, Count: 1, What: "container pg (postgres:16)"}}, uncharged, "a mock of the time outside tests is the run's")

	wa := windowOf(t, a, "o::TestA")
	require.Equal(t, models.ScopeOutcomePassed, wa.Outcome, "a record never fails a test")
	require.NotNil(t, wa.Evidence)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossContainerUnread, Count: 1, What: "container pg (postgres:16)"}}, wa.Evidence.CannotCapture)
	require.Contains(t, wa.Evidence.Reason, "not judged")
	require.Contains(t, wa.Evidence.Reason, "pg (postgres:16)")
	wb := windowOf(t, a, "o::TestB")
	require.True(t, wb.Evidence == nil || len(wb.Evidence.CannotCapture) == 0, "the next test is untouched: %+v", wb.Evidence)
}

// A mock of the open test stamped while it is open, its container's read
// stalled, is on the test's window as it closes.
func TestARecordChargesAnUnreadMockOfTheOpenTestAsItCloses(t *testing.T) {
	saved := containerkey.Default
	t.Cleanup(func() { containerkey.Default = saved })
	containerkey.Default = containerkey.NewKeyer(stalledIndex{addr: "172.17.0.7:5432", c: containerkey.Container{ID: "pg", Image: "postgres:16", Port: 5432}})
	ctx := context.Background()
	a, _ := wireAgent(t, models.MODE_RECORD)
	require.NoError(t, a.resetSession(ctx))
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	containerkey.Default.Name(nil, "c1", "172.17.0.7:5432")
	containerkey.Default.Stamp(&models.Mock{ConnectionID: "c1", Spec: models.MockSpec{ReqTimestampMock: time.Now()}})
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	w := windowOf(t, a, "o::TestA")
	require.NotNil(t, w.Evidence)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossContainerUnread, Count: 1, What: "container pg (postgres:16)"}}, w.Evidence.CannotCapture)
}

// A test run twice (a retry) is two attempts: a mock of the first stamped
// late, as the second is open, charges the first attempt's window, never the
// second's, which is the test's recording.
func TestARecordChargesALateUnreadMockToItsOwnAttemptNotARetry(t *testing.T) {
	saved := containerkey.Default
	t.Cleanup(func() { containerkey.Default = saved })
	containerkey.Default = containerkey.NewKeyer(stalledIndex{addr: "172.17.0.7:5432", c: containerkey.Container{ID: "pg", Image: "postgres:16", Port: 5432}})
	ctx := context.Background()
	a, _ := wireAgent(t, models.MODE_RECORD)
	require.NoError(t, a.resetSession(ctx))
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	sent := time.Now() // the first attempt's request to pg
	containerkey.Default.Name(nil, "c1", "172.17.0.7:5432")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "failed"})
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestA")) // the retry
	require.Nil(t, serr)
	containerkey.Default.Stamp(&models.Mock{ConnectionID: "c1", Spec: models.MockSpec{ReqTimestampMock: sent}})
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	ws, err := a.FinalScopeWindows(ctx)
	require.NoError(t, err)
	var attempts []models.ScopeWindow
	for _, w := range ws {
		if w.Name == "o::TestA" {
			attempts = append(attempts, w)
		}
	}
	require.Len(t, attempts, 2)
	require.NotNil(t, attempts[0].Evidence, "the first attempt's mock charges the first attempt")
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossContainerUnread, Count: 1, What: "container pg (postgres:16)"}}, attempts[0].Evidence.CannotCapture)
	require.True(t, attempts[1].Evidence == nil || len(attempts[1].Evidence.CannotCapture) == 0, "the retry is untouched: %+v", attempts[1].Evidence)
}
