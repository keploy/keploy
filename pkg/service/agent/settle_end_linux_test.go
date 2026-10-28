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

// A test during which the kernel dropped, at the proxy's listener, what can
// lose a connection (a SYN or final ACK it could not take: the listener's
// drops and its namespace's ListenDrops both rose, an accept queue that
// overflowed among the causes) is charged it as its own capture loss
// (listener_dropped): that connection is in no count until the client
// retransmits it, so a call of the test may not have reached keploy. It alone
// is not judged -- never failed, the drop on its window -- and the tests
// before and after it are judged: one drop does not leave the run without a
// verdict. Its begin's drops are what its end compares. keploy test's test
// case is charged the same, in the agent's counts its read of the test
// case's losses takes after its end.
func TestADropInOneTestsWindowLeavesOnlyThatTestNotJudged(t *testing.T) {
	a, p := exactAgent(t, nil, map[string][]string{"o::TestA": {}, "o::TestB": {}, "o::TestC": {}}, map[string][]string{})
	ctx := context.Background()
	drop := func(listener, net uint64) {
		p.smu.Lock()
		p.drops, p.netDrops = p.drops+listener, p.netDrops+net
		p.smu.Unlock()
	}
	drop(4, 4) // dropped before the run's tests: no test's
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Empty(t, e.Action, "no packet dropped while TestA ran: %s", e.Reason)

	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	drop(2, 1) // an accept queue full at the final ACK: twice at the listener, once in the namespace
	e, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr, "a drop is TestB's capture loss, not keploy's failure to end it")
	require.Empty(t, e.Action, "not judged is never a failure: %s", e.Reason)

	// TestB's drop is charged to TestB alone: what its listener count left
	// unpaired is carried one window, and pairs with nothing there.
	_, serr = a.ProvenBegin(ctx, begin("TestC"))
	require.Nil(t, serr)
	e, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestC", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Empty(t, e.Action, "TestB's drop is not TestC's: %s", e.Reason)

	dropped := models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1}}
	w := windowOf(t, a, "o::TestB")
	require.Equal(t, models.ScopeOutcomePassed, w.Outcome)
	require.NotNil(t, w.Evidence)
	require.Equal(t, dropped, w.Evidence.CannotCapture)
	require.Empty(t, w.Evidence.CaptureIncomplete)
	require.False(t, w.Evidence.Unread, "judged as a test that has its own loss, not as one keploy could not read")
	require.Contains(t, w.Evidence.Reason, "not judged: the kernel dropped 1 connection attempt(s) at keploy's proxy listener (its SYN or accept queue full), so a call may not have reached keploy")
	for _, id := range []string{"o::TestA", "o::TestC"} {
		require.Nil(t, windowOf(t, a, id).Evidence, "%s is judged, and charged nothing", id)
	}
	un, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "it was charged to its test")

	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}, nil))
	drop(1, 1)
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}, nil), "the test case's end serves the pool: its drop is in its losses")
	lost, err := a.CaptureLosses(ctx)
	require.NoError(t, err)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 2}}, lost, "TestB's drop, then test-1's")
}

// A record charges a drop the same: the test it fell in keeps its outcome
// against real infra and carries the drop as its own capture loss, which a
// judge of the recording sets aside alone (the test's own kind, NotJudged
// and PerTest), and the run's count of it is what the tests were charged, so
// none of it is left to the recording as a whole.
func TestARecordChargesADropToTheTestItFellIn(t *testing.T) {
	a, p := wireAgent(t, models.MODE_RECORD)
	claimSelf(t, a)
	ctx := context.Background()
	require.Empty(t, endPassed(t, a, "TestA").Action)
	_, serr := a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	p.smu.Lock()
	p.drops, p.netDrops = p.drops+1, p.netDrops+1
	p.smu.Unlock()
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Empty(t, e.Action, "a record never fails a test: %s", e.Reason)
	require.Empty(t, endPassed(t, a, "TestC").Action)

	w := windowOf(t, a, "o::TestB")
	require.Equal(t, models.ScopeOutcomePassed, w.Outcome)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1}}, w.Evidence.CannotCapture)
	require.False(t, w.Evidence.Unread)
	for _, id := range []string{"o::TestA", "o::TestC"} {
		require.Nil(t, windowOf(t, a, id).Evidence, "%s is charged nothing", id)
	}
	un, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "the recording as a whole is charged nothing")
}

// An agent whose capture losses cannot be read (hooks that count no PID-map
// refusals, a proxy and a downstream build that count none) has no read that
// would take a drop: the test it fell in is not verified, never charged to
// nothing, and so is the test after a gap it fell in, and keploy test's test
// case.
func TestADropAnAgentCannotCountAsALossLeavesItsTestNotVerified(t *testing.T) {
	a, p := unclaimedExactAgent(t, nil, map[string][]string{"o::TestA": {}, "o::TestB": {}}, map[string][]string{})
	ctx := context.Background()
	a.Hooks = uncountedHooks{}
	a.proven.losses = a.markCaptureLosses(ctx)
	claimSelf(t, a)
	_, err := a.CaptureLosses(ctx)
	require.ErrorIs(t, err, models.ErrCaptureLossesUnsupported)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.smu.Lock()
	p.drops, p.netDrops = p.drops+1, p.netDrops+1
	p.smu.Unlock()
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.NotNil(t, serr)
	p.smu.Lock()
	p.drops, p.netDrops = p.drops+2, p.netDrops+2 // in the gap: TestB's
	p.smu.Unlock()
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.NotNil(t, serr, "the gap's drops fail the next test's end")

	w := windowOf(t, a, "o::TestA")
	require.Contains(t, w.Evidence.Reason, "the kernel dropped 1 connection attempt(s) at keploy's proxy listener while o::TestA ran (its SYN or accept queue full: the listener's drops and its network namespace's ListenDrops both rose)")
	require.True(t, w.Evidence.Unread, "not verified")
	w = windowOf(t, a, "o::TestB")
	require.Contains(t, w.Evidence.Reason, "the kernel dropped 2 connection attempt(s) at keploy's proxy listener while o::TestB ran, or before it while no test was open (its SYN or accept queue full")
	require.True(t, w.Evidence.Unread, "not verified")

	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}, nil))
	p.smu.Lock()
	p.drops, p.netDrops = p.drops+1, p.netDrops+1
	p.smu.Unlock()
	err = a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}, nil)
	require.ErrorIs(t, err, errDropped)
}

// The kernel raises a drop's two counts one after the other, and keploy reads
// them one after the other, so a drop that can lose a connection can land its
// listener's rise in one window and its namespace's in a later one: late
// enough, on either side of the whole gap between two tests. It is charged to
// the test's window in which both its rises are seen, never to none: the gap
// passes on whole what it left unpaired. The test before it is judged, as is
// the test after the one charged; and a rise that pairs with nothing in the
// next test's window is no later test's. keploy test's test cases are charged
// the same.
func TestADropWhoseRisesLandOnEitherSideOfABeginIsTheNextTests(t *testing.T) {
	a, p := exactAgent(t, nil, map[string][]string{"o::TestA": {}, "o::TestB": {}, "o::TestC": {}, "o::TestD": {}, "o::TestE": {}, "o::TestF": {}, "o::TestG": {}}, map[string][]string{})
	ctx := context.Background()
	drop := func(listener, net uint64) {
		p.smu.Lock()
		p.drops, p.netDrops = p.drops+listener, p.netDrops+net
		p.smu.Unlock()
	}
	run := func(name string, during func()) *models.ScopeError {
		_, serr := a.ProvenBegin(ctx, begin(name))
		require.Nil(t, serr)
		during()
		e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: name, CanSkip: true, Outcome: models.ScopeOutcomePassed})
		if serr == nil {
			require.Empty(t, e.Action, "%s: %s", name, e.Reason)
		}
		return serr
	}

	// The listener's rise while TestA ran, the namespace's after TestB began.
	require.Nil(t, run("TestA", func() { drop(1, 0) }), "a listener's rise alone loses no call")
	require.Nil(t, run("TestB", func() { drop(0, 1) }))
	// One rise in the gap after TestB, the other after TestC began.
	drop(0, 1)
	require.Nil(t, run("TestC", func() { drop(1, 0) }))
	// A listener's rise in TestD that pairs with nothing in TestE is not
	// TestF's.
	require.Nil(t, run("TestD", func() { drop(1, 0) }), "nothing of TestC's drop is left to charge TestD")
	require.Nil(t, run("TestE", func() {}))
	require.Nil(t, run("TestF", func() { drop(0, 1) }), "another listener's drop in TestF, TestD's rise two tests back")
	// TestF's namespace rise, unpaired, is carried into TestG, which pairs it
	// with nothing.
	require.Nil(t, run("TestG", func() {}))

	for _, id := range []string{"o::TestA", "o::TestD", "o::TestE", "o::TestF", "o::TestG"} {
		require.Nil(t, windowOf(t, a, id).Evidence, "%s judged, and charged nothing", id)
	}
	for _, id := range []string{"o::TestB", "o::TestC"} {
		w := windowOf(t, a, id)
		require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1}}, w.Evidence.CannotCapture, id)
		require.Contains(t, w.Evidence.Reason, "the kernel dropped 1 connection attempt(s) at keploy's proxy listener (its SYN or accept queue full)", id)
		require.False(t, w.Evidence.Unread, "%s not judged for its own loss, not unread", id)
	}

	count := func() uint64 { return listenerDropped(t, a) }
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}, nil))
	drop(1, 0)
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}, nil))
	require.Equal(t, uint64(2), count(), "test-1's listener rise alone charges it nothing")
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-2"}}, nil))
	drop(0, 1)
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}, nil))
	require.Equal(t, uint64(3), count(), "test-2 pairs the rise test-1 left with its own")
}

// A drop between two tests is charged where a miss would be: one that can
// lose a connection between a test's end and the next begin is the next
// test's, and so is one whose rises straddle a test's end read (keploy reads the
// listener's count, then the namespace's; the kernel raises them in that
// order, so the namespace's rise can be seen inside the test and the
// listener's only after its end): it is counted once both have risen, which
// is after the end. Never charged to no test. The test before is judged.
// keploy test's test cases are charged the same.
func TestADropBetweenTwoTestsIsTheNextTests(t *testing.T) {
	a, p := exactAgent(t, nil, map[string][]string{"o::TestA": {}, "o::TestB": {}, "o::TestC": {}, "o::TestD": {}}, map[string][]string{})
	ctx := context.Background()
	drop := func(listener, net uint64) {
		p.smu.Lock()
		p.drops, p.netDrops = p.drops+listener, p.netDrops+net
		p.smu.Unlock()
	}
	run := func(name string, during func()) {
		_, serr := a.ProvenBegin(ctx, begin(name))
		require.Nil(t, serr)
		during()
		e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: name, CanSkip: true, Outcome: models.ScopeOutcomePassed})
		require.Nil(t, serr)
		require.Empty(t, e.Action, "%s: %s", name, e.Reason)
	}
	run("TestA", func() {})
	drop(1, 1) // an accept queue full between TestA and TestB
	run("TestB", func() {})
	run("TestC", func() { drop(0, 1) }) // the namespace's rise seen by TestC's end read
	drop(1, 0)                          // the same drop's listener rise, seen after it
	run("TestD", func() {})

	for _, id := range []string{"o::TestA", "o::TestC"} {
		require.Nil(t, windowOf(t, a, id).Evidence, "%s is judged, and charged nothing", id)
	}
	for _, id := range []string{"o::TestB", "o::TestD"} {
		w := windowOf(t, a, id)
		require.NotNil(t, w.Evidence, "%s is charged the drop before it", id)
		require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1}}, w.Evidence.CannotCapture, id)
		require.False(t, w.Evidence.Unread, id)
	}
	un, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "both drops were charged to a test")

	count := func() uint64 { return listenerDropped(t, a) }
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}, nil))
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}, nil))
	drop(1, 1)
	require.Equal(t, uint64(2), count(), "not counted before the gap closes")
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-2"}}, nil))
	require.Equal(t, uint64(3), count(), "counted as test-2 opens: its read after its end takes it")
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}, nil))
	drop(1, 1)
	_, err = a.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(4), count(), "the read of the misses after test-2's end counts it: it may be the run's last")
}

// listenerDropped is the agent's count of listener drops that can lose a
// connection (models.CaptureLossListenerDropped): what keploy test's read of
// a test case's losses, after its end, takes the rise of.
func listenerDropped(t *testing.T, a *Agent) uint64 {
	t.Helper()
	lost, err := a.CaptureLosses(context.Background())
	require.NoError(t, err)
	for _, c := range lost {
		if c.Kind == models.CaptureLossListenerDropped {
			return c.Count
		}
	}
	return 0
}

// keploy test's read of a test case's misses closes the gap after its end
// and passes on whole what it leaves unpaired, the test case's own unpaired
// rise included: a drop whose namespace rise lands in test-1 and whose
// listener rise lands after that read pairs as test-2 opens, and is test-2's,
// never none's.
func TestADropWhoseRisesStraddleTheReadOfMissesIsTheNextTestCases(t *testing.T) {
	a, p := exactAgent(t, nil, map[string][]string{}, map[string][]string{})
	ctx := context.Background()
	drop := func(listener, net uint64) {
		p.smu.Lock()
		p.drops, p.netDrops = p.drops+listener, p.netDrops+net
		p.smu.Unlock()
	}
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}, nil))
	drop(0, 1) // the namespace's rise, seen by test-1's end read
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}, nil))
	_, err := a.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(0), listenerDropped(t, a), "half a drop is no drop yet")
	drop(1, 0) // the same drop's listener rise, seen after the read of test-1's misses
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-2"}}, nil))
	require.Equal(t, uint64(1), listenerDropped(t, a), "counted as test-2 opens: its read after its end takes it")
}

// keploy test's read of a test case's misses counts the listener's drops
// after its settle, just before it reads the misses: a drop while the read
// waits for a call still in flight is the test case's, as a miss made then
// is, never the next one's.
func TestADropWhileTheReadOfMissesSettlesIsThatTestCases(t *testing.T) {
	a, p := exactAgent(t, nil, map[string][]string{}, map[string][]string{})
	ctx := context.Background()
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}, nil))
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}, nil))
	p.smu.Lock()
	p.inWork = make(chan struct{})
	p.smu.Unlock()
	p.set(0, 1) // a call of test-1's still in the proxy's hands
	read := make(chan error, 1)
	go func() {
		_, err := a.GetMockErrors(ctx)
		read <- err
	}()
	<-p.inWork // the read settles: it waits on the call
	p.smu.Lock()
	p.drops, p.netDrops = p.drops+1, p.netDrops+1 // an accept queue full
	p.smu.Unlock()
	p.set(0, 0)
	require.NoError(t, <-read)
	require.Equal(t, uint64(1), listenerDropped(t, a), "counted by the read of test-1's misses: its read of test-1's losses after it takes it")
}

// A drop after a record's last test is where every loss after the last test
// is: among the losses charged to no test (UnchargedCaptureLosses), never
// lost. A connect abandoned there is not counted.
func TestADropAfterTheLastTestIsTheRuns(t *testing.T) {
	a, p := wireAgent(t, models.MODE_RECORD)
	claimSelf(t, a)
	ctx := context.Background()
	require.Empty(t, endPassed(t, a, "TestA").Action)
	p.smu.Lock()
	p.drops, p.netDrops = p.drops+3, p.netDrops+1 // two abandoned connects and an accept queue full
	p.smu.Unlock()
	un, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1}}, un)
	// The run's read of its losses after it has the drop: mock record and
	// mock test make the uncharged read first, so theirs does.
	require.Equal(t, uint64(1), listenerDropped(t, a), "the run's read has the drop")
	require.Nil(t, windowOf(t, a, "o::TestA").Evidence, "TestA is charged nothing")
	un, err = a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1}}, un, "counted once")
}

// A run's last read while a test is still open (out of the CLI's order, which
// reads the windows first, so that a test whose process went away is ended
// by its reap before) closes no window of a test's: the drops in it are the
// test's, charged by its end, which always comes. That read counts none of
// them as the run's, and a later one counts them once, on that test.
func TestARunsReadWhileATestIsOpenLeavesItsDropsToItsEnd(t *testing.T) {
	a, p := wireAgent(t, models.MODE_RECORD)
	claimSelf(t, a)
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.smu.Lock()
	p.drops, p.netDrops = p.drops+1, p.netDrops+1 // an accept queue full
	p.smu.Unlock()
	un, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "TestA's drop is not the run's while TestA is open")
	_, err = a.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(0), listenerDropped(t, a), "nor counted by a read of the misses while TestA is open")
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Empty(t, e.Action, "a record never fails a test: %s", e.Reason)
	w := windowOf(t, a, "o::TestA")
	require.NotNil(t, w.Evidence, "TestA is charged its drop at its end")
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1}}, w.Evidence.CannotCapture)
	un, err = a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un, "charged to TestA, not to the run")
}

// A test whose end keploy could not settle is not judged, and its window
// goes on as the gap's: a drop in it, or after its end, is charged where a
// miss there would be, to the next test or, after the last test, to the run,
// never to none.
func TestADropInATestWhoseEndFailedIsTheGaps(t *testing.T) {
	for _, next := range []bool{true, false} {
		name := "the run's, after the last test"
		if next {
			name = "the next test's"
		}
		t.Run(name, func(t *testing.T) {
			a, p := wireAgent(t, models.MODE_RECORD)
			claimSelf(t, a)
			ctx := context.Background()
			h := a.Hooks.(*countHooks)
			setErr := func(err error) {
				h.mu.Lock()
				h.err = err
				h.mu.Unlock()
			}
			drop := func() {
				p.smu.Lock()
				p.drops, p.netDrops = p.drops+1, p.netDrops+1 // an accept queue full
				p.smu.Unlock()
			}
			_, serr := a.ProvenBegin(ctx, begin("TestA"))
			require.Nil(t, serr)
			drop()
			setErr(models.ErrProxyConnsUnsupported)
			_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: models.ScopeOutcomePassed})
			require.NotNil(t, serr, "TestA's end could not settle")
			setErr(nil)
			drop()
			want := models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 2}}
			if next {
				require.Empty(t, endPassed(t, a, "TestB").Action)
				w := windowOf(t, a, "o::TestB")
				require.NotNil(t, w.Evidence, "TestB is charged both drops")
				require.Equal(t, want, w.Evidence.CannotCapture)
				un, err := a.UnchargedCaptureLosses(ctx)
				require.NoError(t, err)
				require.Empty(t, un)
			} else {
				un, err := a.UnchargedCaptureLosses(ctx)
				require.NoError(t, err)
				require.Equal(t, want, un, "both drops are the run's")
			}
			w := windowOf(t, a, "o::TestA")
			require.NotNil(t, w.Evidence)
			require.True(t, w.Evidence.Unread, "TestA is not judged")
		})
	}
}

// A session's drops are counted from its start: one after a test process's
// claim and before its first begin is its first test's, as a miss there is.
// One before the claim is the run's
// (TestADropBeforeATestProcesssClaimIsTheRuns).
func TestADropBeforeTheFirstTestIsTheFirstTests(t *testing.T) {
	a, p := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	p.smu.Lock()
	p.drops, p.netDrops = p.drops+1, p.netDrops+1 // before the session: no test's
	p.smu.Unlock()
	require.NoError(t, a.resetSession(ctx))
	claimSelf(t, a)
	p.smu.Lock()
	p.drops, p.netDrops = p.drops+1, p.netDrops+1
	p.smu.Unlock()
	require.Empty(t, endPassed(t, a, "TestA").Action)
	require.Empty(t, endPassed(t, a, "TestB").Action)
	w := windowOf(t, a, "o::TestA")
	require.NotNil(t, w.Evidence)
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1}}, w.Evidence.CannotCapture)
	require.Nil(t, windowOf(t, a, "o::TestB").Evidence)
	un, err := a.UnchargedCaptureLosses(ctx)
	require.NoError(t, err)
	require.Empty(t, un)
}

// A drop before a test process's claim is the run's, as a failed TLS
// handshake there is and, in a replay, a miss (the claim reads the misses
// before it as the run's): the claim counts it before it marks the losses
// its first test counts from, so that test is charged nothing and the
// losses charged to no test carry it. So for the session's first claim, and
// for a claim after another test process's last test; in a record and in a
// replay.
func TestADropBeforeATestProcesssClaimIsTheRuns(t *testing.T) {
	drop := func(p *wireProxy) {
		p.smu.Lock()
		p.drops, p.netDrops = p.drops+1, p.netDrops+1 // an accept queue full
		p.smu.Unlock()
	}
	want := models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1}}
	for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
		t.Run(string(mode)+"/the first claim", func(t *testing.T) {
			a, p := wireAgent(t, mode)
			ctx := context.Background()
			require.NoError(t, a.resetSession(ctx))
			drop(p) // after the session's start, before the claim
			claimSelf(t, a)
			e := endPassed(t, a, "TestA")
			require.Empty(t, e.Action, e.Reason)
			un, err := a.UnchargedCaptureLosses(ctx)
			require.NoError(t, err)
			require.Equal(t, want, un, "the drop before the claim is the run's")
			require.Nil(t, windowOf(t, a, "o::TestA").Evidence, "the first test is charged nothing")
		})
		t.Run(string(mode)+"/between two test processes", func(t *testing.T) {
			a, p := wireAgent(t, mode)
			ctx := context.Background()
			require.NoError(t, a.resetSession(ctx))
			first := child(t)
			_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "a"}, first.Process.Pid)
			require.Nil(t, serr)
			e := endPassed(t, a, "TestOne")
			require.Empty(t, e.Action, e.Reason)
			kill(t, first)
			drop(p) // after process a's last test, before process o's claim
			claimSelf(t, a)
			e = endPassed(t, a, "TestTwo")
			require.Empty(t, e.Action, e.Reason)
			un, err := a.UnchargedCaptureLosses(ctx)
			require.NoError(t, err)
			require.Equal(t, want, un, "the drop between the two processes is the run's")
			require.Nil(t, windowOf(t, a, "a::TestOne").Evidence, "the test before it is charged nothing")
			require.Nil(t, windowOf(t, a, "o::TestTwo").Evidence, "the next process's first test is charged nothing")
		})
	}
}

// A client that closed its connect before the handshake finished (its
// process exited mid-connect, as go test's -timeout does to a polling test,
// or it gave up on a handshake a loaded host had not finished) resets the
// half-open connection, which raises the listener's drops alone: it loses no
// connection, so the test it fell in is judged, and so is keploy test's test
// case.
func TestAConnectAbandonedMidHandshakeLeavesTheTestJudged(t *testing.T) {
	a, p := exactAgent(t, nil, map[string][]string{"o::TestA": {}}, map[string][]string{})
	ctx := context.Background()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.smu.Lock()
	p.drops += 5
	p.smu.Unlock()
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)
	require.Empty(t, e.Action, "five abandoned connects lose no call: %s", e.Reason)
	require.Nil(t, windowOf(t, a, "o::TestA").Evidence, "judged, and charged nothing")

	before := listenerDropped(t, a)
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{Test: "test-1"}}, nil))
	p.smu.Lock()
	p.drops += 3
	p.smu.Unlock()
	require.NoError(t, a.serveKeployTestExactly(ctx, models.MockFilterParams{Exact: &models.ExactServing{}}, nil))
	_, err := a.GetMockErrors(ctx)
	require.NoError(t, err)
	require.Equal(t, before, listenerDropped(t, a), "test-1's three abandoned connects are no loss its read of losses would take")
}

// Where keploy cannot read its network namespace's ListenDrops, it cannot
// tell an abandoned connect from a drop that can lose a connection: every
// drop at the listener counts, and the reason says why.
func TestADropKeployCannotTellIsCounted(t *testing.T) {
	a, p := exactAgent(t, nil, map[string][]string{"o::TestA": {}, "o::TestB": {}}, map[string][]string{})
	ctx := context.Background()
	p.smu.Lock()
	p.netUnread = "tell the listener's network namespace (SIOCGSKNS): operation not permitted"
	p.smu.Unlock()
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	p.smu.Lock()
	p.drops++
	p.smu.Unlock()
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)

	// Unread at TestB's begin alone: every drop of its window counts, and
	// its loss says why with the begin's reason.
	const atBegin = "read the network namespace's TCP counters: no such file"
	p.smu.Lock()
	p.netUnread = atBegin
	p.smu.Unlock()
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	p.smu.Lock()
	p.netUnread = ""
	p.drops++
	p.smu.Unlock()
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", CanSkip: true, Outcome: models.ScopeOutcomePassed})
	require.Nil(t, serr)

	w := windowOf(t, a, "o::TestA")
	why := "every packet the listener dropped counted: keploy could not read its network namespace's ListenDrops, which tell a drop that can lose a connection from a client that gave up its connect: tell the listener's network namespace (SIOCGSKNS): operation not permitted"
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1, What: why}}, w.Evidence.CannotCapture)
	require.Contains(t, w.Evidence.Reason, "the kernel dropped 1 packet(s) at keploy's proxy listener (a full SYN or accept queue, or a client that gave up its connect: keploy could not tell which)")
	require.NotContains(t, w.Evidence.Reason, "its SYN or accept queue full", "not said where keploy could not tell")
	require.Contains(t, w.Evidence.Reason, "("+why+")")
	require.False(t, w.Evidence.Unread)
	w = windowOf(t, a, "o::TestB")
	require.NotNil(t, w.Evidence, "an abandoned connect cannot be told from a real drop with the begin's namespace count unread")
	require.Equal(t, models.CaptureLosses{{Kind: models.CaptureLossListenerDropped, Count: 1, What: "every packet the listener dropped counted: keploy could not read its network namespace's ListenDrops, which tell a drop that can lose a connection from a client that gave up its connect: " + atBegin}}, w.Evidence.CannotCapture)
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
	w := stallWake(t)
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
			case <-w.blocked: // the open's settle waits for that call
			}
			select {
			case err := <-opened:
				t.Fatalf("opened while a call made before was in flight: %v", err)
			default:
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
