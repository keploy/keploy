//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// givingProxy is a wire proxy that keeps the test variables a replay binds:
// it records each given it was handed for the test it serves.
type givingProxy struct {
	*wireProxy
	gave    []models.VariableBinding
	giveErr error
}

func (p *givingProxy) GiveExactTest(test string, b models.VariableBinding) error {
	p.mu.Lock()
	serving := p.exactTest
	p.mu.Unlock()
	if test != serving {
		return fmt.Errorf("%s is not the test keploy serves now", test)
	}
	if p.giveErr != nil {
		return p.giveErr
	}
	p.gave = append(p.gave, b)
	return nil
}

func giving(a *Agent, p *wireProxy) *givingProxy {
	g := &givingProxy{wireProxy: p}
	a.Proxy = g
	return g
}

func give(name, value, source string) models.GivenReq {
	return models.GivenReq{Name: name, Value: value, Source: source}
}

// In a record a test's givens are numbered per name in the order it gave
// them, kept with its window, and bound nowhere; a given outside a test, or
// one keploy cannot hold, is refused.
func TestARecordedTestsGivensAreKeptWithItsWindow(t *testing.T) {
	a, p := wireAgent(t, models.MODE_RECORD)
	g := giving(a, p)
	ctx := context.Background()
	claimSelf(t, a)

	_, serr := a.ProvenGive(ctx, give("app", "app-1", "a_test.go:9"))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotOpen, serr.Code)
	require.Equal(t, http.StatusUnprocessableEntity, serr.Status)
	require.Contains(t, serr.Message, "app (a_test.go:9) while no test was open")

	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	var vars []string
	for _, r := range []models.GivenReq{give("app", "app-1", "a_test.go:20"), give("nonce", "1761609600123456789", "a_test.go:21"), give("app", "app-2", "a_test.go:22")} {
		resp, serr := a.ProvenGive(ctx, r)
		require.Nil(t, serr)
		require.Equal(t, "ok", resp.Status)
		require.Equal(t, models.ScopeProtocolVersion, resp.V)
		vars = append(vars, resp.Variable)
	}
	require.Equal(t, []string{"given:app", "given:nonce", "given:app#2"}, vars)
	for _, bad := range []models.GivenReq{give("app.name", "v", ""), give("", "v", ""), give("app", "", "")} {
		_, serr := a.ProvenGive(ctx, bad)
		require.NotNil(t, serr, "%+v", bad)
		require.Equal(t, models.ScopeErrBadRequest, serr.Code, "%+v", bad)
	}
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, g.gave, "a record binds no variable")

	_, serr = a.ProvenGive(ctx, give("app", "app-3", ""))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotOpen, serr.Code, "a test's givens end with it")

	// The next test numbers its own from the first.
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	resp, serr := a.ProvenGive(ctx, give("app", "app-4", ""))
	require.Nil(t, serr)
	require.Equal(t, "given:app", resp.Variable)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: "passed"})
	require.Nil(t, serr)

	require.Equal(t, []models.GivenValue{
		{Name: "given:app", Value: "app-1", Source: "a_test.go:20"},
		{Name: "given:nonce", Value: "1761609600123456789", Source: "a_test.go:21"},
		{Name: "given:app#2", Value: "app-2", Source: "a_test.go:22"},
	}, windowOf(t, a, "o::TestA").Given)
	require.Equal(t, []models.GivenValue{{Name: "given:app", Value: "app-4"}}, windowOf(t, a, "o::TestB").Given)
}

// In a replay each given binds in the open test's scope with the value its
// recording gave under the same variable; one the recording did not give
// binds with none. One the proxy refuses is refused, and not kept.
func TestAReplayedGivenBindsWithItsRecordedValue(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	g := giving(a, p)
	ctx := context.Background()
	require.NoError(t, a.SetScopeGiven(ctx, map[string][]models.GivenValue{
		"orders::TestA": {{Name: "given:app", Value: "app-1", Source: "a_test.go:20"}},
		"orders::TestB": {},
	}))
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)

	resp, serr := a.ProvenGive(ctx, give("app", "app-9", "a_test.go:20"))
	require.Nil(t, serr)
	require.Equal(t, "given:app", resp.Variable)
	resp, serr = a.ProvenGive(ctx, give("app", "app-10", "a_test.go:30"))
	require.Nil(t, serr)
	require.Equal(t, "given:app#2", resp.Variable)
	require.Equal(t, []models.VariableBinding{
		{Name: "given:app", Value: "app-9", Recorded: "app-1", Mock: "test orders::TestA", Place: "a_test.go:20", Given: true},
		{Name: "given:app#2", Value: "app-10", Mock: "test orders::TestA", Place: "a_test.go:30", Given: true},
	}, g.gave)

	g.giveErr = errors.New("variable given:other is bound already")
	_, serr = a.ProvenGive(ctx, give("other", "x", ""))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrInternal, serr.Code)
	require.Contains(t, serr.Message, "could not hold given:other for orders::TestA")
	g.giveErr = nil
	resp, serr = a.ProvenGive(ctx, give("other", "y", ""))
	require.Nil(t, serr)
	require.Equal(t, "given:other", resp.Variable, "a refused given is not counted")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, windowOf(t, a, "orders::TestA").Given, "a replay holds a test's givens only while it is open")
}

// A test holds at most MaxGivensPerTest values until its end.
func TestATestGivesABoundedNumberOfValues(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	for i := 0; i < MaxGivensPerTest; i++ {
		_, serr := a.ProvenGive(ctx, give("n", "v", ""))
		require.Nil(t, serr)
	}
	_, serr = a.ProvenGive(ctx, give("n", "v", ""))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrBadRequest, serr.Code)
	require.Contains(t, serr.Message, fmt.Sprintf("more than %d values", MaxGivensPerTest))
	_, serr = a.ProvenGive(ctx, give("n", string(make([]byte, models.MaxGivenValue+1)), ""))
	require.NotNil(t, serr)
	require.Contains(t, serr.Message, "longer than")
	_, serr = a.ProvenGive(ctx, give("m", "v", string(make([]byte, models.MaxGivenSource+1))))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrBadRequest, serr.Code)
	require.Contains(t, serr.Message, "source is longer than")
}

// A record holds each test's givens with its window until the run's results
// are read: at most MaxGivenBytesPerRecord in a session, counted as they are
// given and let go with the session's windows.
func TestARecordHoldsABoundedTotalOfGivens(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(ctx, give("n", "v", "a_test.go:9"))
	require.Nil(t, serr)
	a.scopeMu.Lock()
	require.Equal(t, len("given:n")+len("v")+len("a_test.go:9"), a.givenBytes)
	a.givenBytes = MaxGivenBytesPerRecord - len("given:m")
	a.scopeMu.Unlock()
	_, serr = a.ProvenGive(ctx, give("m", "v", ""))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrBadRequest, serr.Code)
	require.Contains(t, serr.Message, "more than 64 MiB of values")
	a.resetScopeState()
	a.scopeMu.Lock()
	require.Zero(t, a.givenBytes, "a new session holds none")
	a.scopeMu.Unlock()
}

// A replayed test that fails while it gave a value its recording does not
// hold (recorded before the test gave it) says so first, naming the given
// and the step that fixes it; one its recording holds is not named, and a
// test that passes is not failed for it.
func TestAGivenItsRecordingLacksSaysToReRecordTheTest(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	giving(a, p)
	ctx := context.Background()
	require.NoError(t, a.SetScopeGiven(ctx, map[string][]models.GivenValue{
		"orders::TestB": {{Name: "given:app", Value: "app-1"}},
		"orders::TestC": {{Name: "given:app", Value: "app-1"}},
		"orders::TestD": {{Name: "given:app", Value: "app-1"}},
	}))
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
	require.Nil(t, serr)
	const lacks = "the test gave given:nonce, which its recording does not hold (recorded before the test gave it): re-record the test"

	// Passing: nothing to say.
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(ctx, give("nonce", "1761609600123456789", ""))
	require.Nil(t, serr)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.NotEqual(t, models.ScopeActionFail, e.Action, "a given the recording lacks fails no test by itself")

	// Failing: the given the recording lacks is named first, the recorded one not.
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(ctx, give("app", "app-9", ""))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(ctx, give("nonce", "1761609600123456789", ""))
	require.Nil(t, serr)
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "127.0.0.1:18282"}}
	e, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.True(t, strings.HasPrefix(e.Reason, "keploy: "+lacks+". POST /audit to 127.0.0.1:18282 had no recorded answer"), e.Reason)
	require.NotContains(t, e.Reason, "given:app")
	reasonB := e.Reason

	// Failing with every given recorded: no such sentence.
	_, serr = a.ProvenBegin(ctx, begin("TestC"))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(ctx, give("app", "app-10", ""))
	require.Nil(t, serr)
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "127.0.0.1:18282"}}
	e, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestC", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.NotContains(t, e.Reason, "recording does not hold")

	// Failing after giving a name twice that its recording holds once: the
	// second value (given:app#2) is the one it lacks, named by its number
	// and alone. Its name starts with the recorded one's; it is not that one.
	_, serr = a.ProvenBegin(ctx, begin("TestD"))
	require.Nil(t, serr)
	for _, v := range []string{"app-11", "app-12"} {
		_, serr = a.ProvenGive(ctx, give("app", v, ""))
		require.Nil(t, serr)
	}
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "127.0.0.1:18282"}}
	e, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestD", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.True(t, strings.HasPrefix(e.Reason, "keploy: the test gave given:app#2, which its recording does not hold (recorded before the test gave it): re-record the test. POST /audit to 127.0.0.1:18282 had no recorded answer"), e.Reason)
	require.Equal(t, 1, strings.Count(e.Reason, "re-record the test"), e.Reason)
	require.Equal(t, reasonB, windowOf(t, a, "orders::TestB").Evidence.Reason, "the window keeps the reason")
}

func TestNotRecordedTextNamesTheFirstFew(t *testing.T) {
	require.Equal(t, "", notRecordedText(nil))
	require.Equal(t, "the test gave given:a and given:b, which its recording does not hold (recorded before the test gave them): re-record the test",
		notRecordedText([]string{"given:a", "given:b"}))
	require.Equal(t, "the test gave given:a, given:b, given:c, given:d, given:e and 2 more, which its recording does not hold (recorded before the test gave them): re-record the test",
		notRecordedText([]string{"given:a", "given:b", "given:c", "given:d", "given:e", "given:f", "given:g"}))
}

// givingLossProxy is a giving proxy that reports the calls it could not
// capture, as the proxy does.
type givingLossProxy struct {
	*givingProxy
	losses models.CaptureLosses
}

func (p *givingLossProxy) CaptureLosses() models.CaptureLosses { return p.losses }
func (p *givingLossProxy) SetMappedUniverse([]string)          {}

// The sentence that says to re-record comes with a replayed failure that has
// a call that missed or a recorded call the test never made, and with no
// other: not with a loss of capture alone. It stays first where the test is
// charged a loss too, whose text writes the evidence's again.
func TestTheReRecordSentenceComesWithAMissOrAnUnmadeCall(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &givingLossProxy{givingProxy: giving(a, wp)}
	a.Proxy = p
	a.proven.losses = a.markCaptureLosses(context.Background())
	ctx := context.Background()
	t0 := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{pingMock("mock-u", "/audit", "ok", t0)}))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"orders::TestUnmade": {"mock-u"}}))
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
	require.Nil(t, serr)
	const lacks = "keploy: the test gave given:nonce, which its recording does not hold (recorded before the test gave it): re-record the test. "
	run := func(name string, during func()) models.ScopeEndResp {
		t.Helper()
		_, serr := a.ProvenBegin(ctx, begin(name))
		require.Nil(t, serr)
		_, serr = a.ProvenGive(ctx, give("nonce", "1761609600123456789", ""))
		require.Nil(t, serr)
		during()
		e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: name, Outcome: "passed"})
		require.Nil(t, serr)
		require.Equal(t, models.ScopeActionFail, e.Action, name)
		return e
	}
	lose := func(n uint64) {
		p.losses = models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: n, What: "to 10.0.0.1:443"}}
	}

	// Only a recorded call the test never made.
	e := run("TestUnmade", func() {})
	require.True(t, strings.HasPrefix(e.Reason, lacks), e.Reason)
	require.Contains(t, e.Reason, "mock-u")

	// Only a loss of capture: no call missed and none is unmade.
	e = run("TestLost", func() { lose(1) })
	require.Contains(t, e.Reason, "TLS handshake")
	require.NotContains(t, e.Reason, "re-record the test", "a loss alone is no sign the recording lacks the given")

	// A miss and a loss: the sentence first, the loss after it.
	e = run("TestMissedAndLost", func() {
		lose(2)
		wp.mu.Lock()
		wp.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "127.0.0.1:18282"}}
		wp.mu.Unlock()
	})
	require.True(t, strings.HasPrefix(e.Reason, lacks), e.Reason)
	require.Contains(t, e.Reason, "TLS handshake")
	require.Contains(t, e.Reason, "POST /audit to 127.0.0.1:18282 had no recorded answer")
	require.Equal(t, 1, strings.Count(e.Reason, "re-record the test"), e.Reason)
	require.Equal(t, e.Reason, windowOf(t, a, "orders::TestMissedAndLost").Evidence.Reason, "the window keeps the reason")
}

// givingUnreadProxy is a giving proxy whose read of the misses returns the
// ones it found with an error, as the agent client does when the agent says
// its list is incomplete (models.ErrUnmatchedCallsUnread).
type givingUnreadProxy struct {
	*givingProxy
	readErr error
}

func (p *givingUnreadProxy) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	misses, err := p.givingProxy.GetMockErrors(ctx)
	if err == nil {
		err = p.readErr
	}
	return misses, err
}

// A replayed test keploy could not read is unread, not judged: it is not
// told to re-record, though its read returned a miss beside the error and
// it gave a value its recording does not hold. Read whole, the same test is.
func TestAnUnreadTestIsNotToldToReRecord(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &givingUnreadProxy{givingProxy: giving(a, wp)}
	a.Proxy = p
	ctx := context.Background()
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
	require.Nil(t, serr)
	run := func(name string, readErr error) (*models.ScopeError, models.ScopeEndResp) {
		t.Helper()
		_, serr := a.ProvenBegin(ctx, begin(name))
		require.Nil(t, serr)
		_, serr = a.ProvenGive(ctx, give("nonce", "1761609600123456789", ""))
		require.Nil(t, serr)
		p.readErr = readErr
		wp.mu.Lock()
		wp.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "127.0.0.1:18282"}}
		wp.mu.Unlock()
		e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: name, Outcome: "passed"})
		return serr, e
	}

	serr, e := run("TestRead", nil)
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.True(t, strings.HasPrefix(e.Reason, "keploy: the test gave given:nonce, which its recording does not hold (recorded before the test gave it): re-record the test. "), e.Reason)

	serr, _ = run("TestUnread", fmt.Errorf("%w (agent: the drain did not finish)", models.ErrUnmatchedCallsUnread))
	require.NotNil(t, serr, "a test keploy could not read fails its end")
	w := windowOf(t, a, "orders::TestUnread")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.True(t, w.Evidence.Unread)
	require.Len(t, w.Evidence.Misses, 1, "the miss read beside the error is the test's")
	require.True(t, strings.HasPrefix(w.Evidence.Reason, "keploy: keploy could not tell what orders::TestUnread did: reading its misses: "), w.Evidence.Reason)
	require.NotContains(t, w.Evidence.Reason, "re-record the test", "an unread test is not judged, so it is not told to re-record")
}

// A replayed test whose misses keploy read is told to re-record, once,
// though keploy could not settle it, could not read its capture losses, or
// ended it itself: its reason still names those misses. After keploy's
// note when it could not settle the test or ended it (left open), before
// the capture-loss note, which writes the evidence's reason again.
func TestTheReRecordSentenceGoesWithTheMissesRead(t *testing.T) {
	const lacks = "the test gave given:nonce, which its recording does not hold (recorded before the test gave it): re-record the test"
	const missed = "POST /audit to 127.0.0.1:18282 had no recorded answer"
	for _, c := range []struct {
		name   string
		test   string
		unread bool
		// refused: the end call is refused, so the reason is the window's.
		refused bool
		// fault makes keploy unable to judge the test; nil leaves it be.
		fault func(h *countHooks)
		// end ends the test, or ends another, which leaves keploy to end it.
		end    func(a *Agent) (models.ScopeEndResp, *models.ScopeError)
		prefix string
	}{{
		name:    "unsettled",
		test:    "TestU",
		unread:  true,
		refused: true,
		fault: func(h *countHooks) {
			h.mu.Lock()
			h.err = models.ErrProxyConnsUnsupported
			h.mu.Unlock()
		},
		end: func(a *Agent) (models.ScopeEndResp, *models.ScopeError) {
			return a.ProvenEnd(context.Background(), models.ScopeReq{Name: "TestU", Outcome: "passed"})
		},
		prefix: "keploy: keploy could not tell that every call of orders::TestU reached it: keploy cannot count the calls still waiting for its proxy: " +
			models.ErrProxyConnsUnsupported.Error() + ". " + lacks + ". " + missed,
	}, {
		name:   "capture losses unread",
		test:   "TestL",
		unread: true,
		fault:  func(h *countHooks) { h.countingHooks.err = errors.New("the pid map cannot be read") },
		end: func(a *Agent) (models.ScopeEndResp, *models.ScopeError) {
			return a.ProvenEnd(context.Background(), models.ScopeReq{Name: "TestL", Outcome: "passed"})
		},
		prefix: "keploy: " + lacks + ". keploy could not tell whether every call of orders::TestL was captured: read the PID map refusals: the pid map cannot be read. " + missed,
	}, {
		name:    "left open",
		test:    "TestW",
		refused: true,
		end: func(a *Agent) (models.ScopeEndResp, *models.ScopeError) {
			return a.ProvenEnd(context.Background(), models.ScopeReq{Name: "TestOther", Outcome: "passed"})
		},
		prefix: "keploy: orders::TestOther ended while orders::TestW was the open test. " + lacks + ". " + missed,
	}} {
		t.Run(c.name, func(t *testing.T) {
			a, wp := wireAgent(t, models.MODE_TEST)
			giving(a, wp)
			ctx := context.Background()
			_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
			require.Nil(t, serr)
			_, serr = a.ProvenBegin(ctx, begin(c.test))
			require.Nil(t, serr)
			_, serr = a.ProvenGive(ctx, give("nonce", "1761609600123456789", ""))
			require.Nil(t, serr)
			wp.mu.Lock()
			wp.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "127.0.0.1:18282"}}
			wp.mu.Unlock()
			if c.fault != nil {
				c.fault(a.Hooks.(*countHooks))
			}
			e, serr := c.end(a)
			if c.refused {
				require.NotNil(t, serr)
			} else {
				require.Nil(t, serr)
				require.Equal(t, models.ScopeActionFail, e.Action)
				require.True(t, strings.HasPrefix(e.Reason, c.prefix), e.Reason)
			}
			w := windowOf(t, a, "orders::"+c.test)
			require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
			require.Equal(t, c.unread, w.Evidence.Unread)
			require.Len(t, w.Evidence.Misses, 1, "the test's misses were read")
			require.True(t, strings.HasPrefix(w.Evidence.Reason, c.prefix), w.Evidence.Reason)
			require.Equal(t, 1, strings.Count(w.Evidence.Reason, "re-record the test"), w.Evidence.Reason)
		})
	}
}
