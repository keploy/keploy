//go:build linux

package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A test's end waits until its calls are settled: a call the application
// makes after the test's harness said it ended, while the proxy still holds
// the test's work, is the test's own, and the end judges it so, whatever the
// load. Without the wait the end would judge the mock unconsumed.
func TestAnEndJudgesTheCallsStillInFlight(t *testing.T) {
	a, p := exactAgent(t,
		[]*models.Mock{pingMock("mock-1", "/audit", "ok", t0)},
		map[string][]string{"o::TestAsync": {"mock-1"}}, map[string][]string{})
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestAsync"))
	require.Nil(t, serr)
	p.set(0, 1) // the application's call is in the proxy's hands
	ended := make(chan models.ScopeEndResp, 1)
	go func() {
		e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestAsync", CanSkip: true, Outcome: models.ScopeOutcomePassed})
		require.Nil(t, serr)
		ended <- e
	}()
	code, _ := p.call(t, "/audit")
	require.Equal(t, 200, code)
	select {
	case e := <-ended:
		t.Fatalf("the end judged while the test's call was in flight: %+v", e)
	default:
	}
	p.set(0, 0) // answered: nothing in the proxy's hands
	e := <-ended
	require.Empty(t, e.Action, e.Reason)
}

// A test whose calls keploy cannot count is not judged settled: its end
// fails it, and says why.
func TestAnEndThatCannotCountFailsTheTest(t *testing.T) {
	a, _ := exactAgent(t, nil, map[string][]string{"o::TestA": {}}, map[string][]string{})
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	a.Hooks.(*countHooks).err = models.ErrProxyConnsUnsupported
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.NotNil(t, serr)
	w := evidenceOf(t, a, "o::TestA")
	require.Contains(t, w.Reason, "could not tell that every call of o::TestA reached it")
}

// The run's last read of its misses waits, as a test's end does, until the
// calls in the proxy's hands are answered: a call made after the last test's
// end, still held when the run's command exited, is read, never filed after
// the read (keploy #4806's H: the run green with the miss unseen). A read
// keploy cannot settle cannot vouch for every miss.
func TestTheRunsLastReadOfMissesWaitsForTheCallsInFlight(t *testing.T) {
	a, p := exactAgent(t, nil, map[string][]string{}, map[string][]string{})
	ctx := context.Background()
	p.set(0, 1) // a call held after the last test ended
	read := make(chan error, 1)
	go func() {
		_, err := a.GetMockErrors(ctx)
		read <- err
	}()
	select {
	case err := <-read:
		t.Fatalf("the run's misses were read while a call was in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	p.set(0, 0)
	require.NoError(t, <-read)

	a.Hooks.(*countHooks).err = models.ErrProxyConnsUnsupported
	_, err := a.GetMockErrors(ctx)
	require.ErrorIs(t, err, models.ErrUnmatchedCallsUnread, "a read keploy cannot settle vouches for every miss")
}

// A connection served by a parser that reads ahead of its answers and does
// not say which requests it holds (gRPC, HTTP/2, Kafka, Mongo until each marks
// them) cannot be told settled: a test's end that finds one leaves the test
// not verified, never passed, naming the protocol; a begin goes on; the run's
// read of its misses cannot vouch for every miss; keploy test's end of a test
// case serves the pool and says so.
func TestAnEndWithAConnectionWhoseHeldRequestsAreUnknownIsNotVerified(t *testing.T) {
	a, p := exactAgent(t, nil, map[string][]string{"o::TestA": {}}, map[string][]string{})
	ctx := context.Background()
	p.smu.Lock()
	p.unknown = []string{"grpc"}
	p.smu.Unlock()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr, "a begin goes on")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.NotNil(t, serr, "an end keploy cannot settle is keploy's error, as one that cannot count")
	w := windowOf(t, a, "o::TestA")
	require.Contains(t, w.Evidence.Reason, "keploy cannot tell whether grpc on a connection still holds a request")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.True(t, w.Evidence.Unread, "not verified: no verdict of its own")

	_, err := a.GetMockErrors(ctx)
	require.ErrorIs(t, err, models.ErrUnmatchedCallsUnread)

	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}, nil), "a test case opens")
	err = a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}, nil)
	require.ErrorIs(t, err, errHoldsUnknown, "a test case's end is told it is not verified")
}

// A test during which the kernel dropped packets at the proxy's listener (a
// SYN or final ACK it could not take) is not verified: that connection is in
// no count until the client retransmits it, so a call of the test may not
// have reached keploy. Its begin's drops are what its end compares; a test
// with none dropped passes. keploy test's end of a test case says so after
// serving the pool.
func TestATestDuringWhichTheListenerDroppedPacketsIsNotVerified(t *testing.T) {
	a, p := exactAgent(t, nil, map[string][]string{"o::TestA": {}, "o::TestB": {}}, map[string][]string{})
	ctx := context.Background()
	p.smu.Lock()
	p.drops = 4 // dropped before the run's tests: no test's
	p.smu.Unlock()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Empty(t, e.Action, "no packet dropped while TestA ran: %s", e.Reason)

	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	p.smu.Lock()
	p.drops = 5
	p.smu.Unlock()
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.NotNil(t, serr)
	w := windowOf(t, a, "o::TestB")
	require.Contains(t, w.Evidence.Reason, "the kernel dropped 1 packet(s) at keploy's proxy listener while o::TestB ran")
	require.True(t, w.Evidence.Unread, "not verified")

	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}, nil))
	p.smu.Lock()
	p.drops = 7
	p.smu.Unlock()
	err := a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}, nil)
	require.ErrorIs(t, err, errDropped)
}

// A test's begin waits only for what the previous test's end left
// unsettled (settleBegin), never for a call made since that end: it is
// background work or the new test's, and the new test's end waits for it.
func TestABeginDoesNotWaitForTheCallsMadeSinceThePreviousEnd(t *testing.T) {
	w := stallWake(t)
	a, p := exactAgent(t, nil, map[string][]string{"o::TestA": {}}, map[string][]string{})
	ctx := context.Background()
	p.set(0, 1) // a call made since the previous end is in the proxy's hands
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr, "the begin went on while it was in flight")
	ended := make(chan *models.ScopeEndResp, 1)
	go func() {
		e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
		require.Nil(t, serr)
		ended <- &e
	}()
	<-w.blocked
	select {
	case e := <-ended:
		t.Fatalf("the test ended while a call in its window was in flight: %+v", e)
	default:
	}
	p.set(0, 0)
	<-ended
}

// keploy test's open of a test case waits until the calls made before are
// settled: one still in the proxy's hands is the pool's or the previous test
// case's, never this one's (the application gets the test case's request
// only once its view is installed).
func TestKeployTestsOpenWaitsForTheCallsMadeBefore(t *testing.T) {
	a, p := exactAgent(t, nil, map[string][]string{"o::TestA": {}}, map[string][]string{})
	ctx := context.Background()
	for _, open := range []struct {
		name string
		do   func() error
	}{
		{"keploy test's open", func() error {
			return a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}, nil)
		}},
	} {
		t.Run(open.name, func(t *testing.T) {
			p.set(0, 1) // a call made before is in the proxy's hands
			opened := make(chan error, 1)
			go func() { opened <- open.do() }()
			select {
			case err := <-opened:
				t.Fatalf("opened while a call made before was in flight: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			p.set(0, 0)
			require.NoError(t, <-opened)
		})
	}
}

// windowsProxy is servingProxy counting the test windows the agent begins on
// its connections (coreAgent.InFlightWindower).
type windowsProxy struct {
	*servingProxy
	windows atomic.Int32
}

func (w *windowsProxy) InFlightBeginWindow() { w.windows.Add(1) }

// A test's window begins on the proxy's connections at the wire's begin and
// at keploy test's open of a test case, once the calls before are settled,
// and at neither end: the end waits only for the requests opened since. With
// the switch off no window begins.
func TestATestsWindowBeginsAtItsBeginOnly(t *testing.T) {
	ctx := context.Background()
	a, p := exactAgent(t, nil, map[string][]string{"o::TestA": {}}, map[string][]string{})
	w := &windowsProxy{servingProxy: p}
	a.Proxy = w
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	require.EqualValues(t, 1, w.windows.Load(), "the wire's begin begins the test's window")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.EqualValues(t, 1, w.windows.Load(), "the test's end begins none")

	b, q := exactAgent(t, nil, map[string][]string{"o::TestA": {}}, map[string][]string{})
	k := &windowsProxy{servingProxy: q}
	b.Proxy = k
	require.NoError(t, b.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}, nil))
	require.EqualValues(t, 1, k.windows.Load(), "keploy test's open begins the test case's window")
	require.NoError(t, b.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}, nil))
	require.EqualValues(t, 1, k.windows.Load(), "the test case's end begins none")

	t.Setenv(provenswitch.Env, "0")
	b.beginWindow()
	require.EqualValues(t, 1, k.windows.Load(), "with the switch off no window begins")
}
