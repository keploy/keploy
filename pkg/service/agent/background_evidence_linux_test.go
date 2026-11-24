//go:build linux

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// A call a test made that keploy found the same request as one of the set's
// background recordings (its parser's background mark: a startup: mock, the
// app's call in TestMain), with none left to answer it, is a background
// miss: it counts against no test, so keploy does not fail TestB for it, and
// TestB keeps the outcome its harness gave. It is not lost: TestB's evidence
// lists it apart (ScopeEvidence.Background), with the recording closest to
// it, so the end of the run can show the call and how to serve it, and the
// evidence is kept though it fails nothing. TestC's own miss, unmarked,
// stays evidence against it (Misses) and fails it.
// Mutation: drop the call from the evidence, put it in Misses (TestB fails),
// keep only evidence that fails the test (TestB's is not kept), or list a
// call keploy answered.
func TestABackgroundMissIsListedInItsTestsEvidenceAndFailsNoTest(t *testing.T) {
	a, p := exactAgent(t, []*models.Mock{
		pingMock("mock-1", "/b", "b", t0),
		pingMock("mock-2", "/c", "c", t0.Add(1)),
	}, map[string][]string{"o::TestB": {"mock-1"}, "o::TestC": {"mock-2"}}, map[string][]string{"o": nil})
	p.background = func(uc *models.UnmatchedCall) {
		if strings.Contains(uc.ActualSummary, "/rates/") {
			// The proxy's call carries what the app sent; the evidence
			// keeps none of it.
			uc.Background, uc.ClosestMock = true, "mock-0"
			uc.ReceivedReq = "GET /rates/4111111111111111 HTTP/1.1\r\nAuthorization: Bearer s3cret\r\n\r\n"
		}
	}

	got, e := runTest(t, a, p, "TestB", "/b", "/rates/4111111111111111")
	require.Equal(t, "200 b", got[0])
	require.True(t, strings.HasPrefix(got[1], "502"), "keploy answered the background call: %s", got[1])
	require.NotEqual(t, models.ScopeActionFail, e.Action, "the background miss failed TestB: %s", e.Reason)
	ev := evidenceOf(t, a, "o::TestB")
	require.Empty(t, ev.Misses, "the background miss is evidence against TestB")
	require.True(t, ev.Empty(), "TestB's evidence counts against it")
	require.Empty(t, ev.Reason, "TestB was given a reason for a miss that fails nothing")
	require.Len(t, ev.Background, 1, "TestB's evidence does not list its background miss")
	require.Equal(t, models.MissShape(models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /rates/4111111111111111"}), ev.Background[0].ActualSummary, "the listed call is not in its shape")
	require.NotContains(t, ev.Background[0].ActualSummary, "4111111111111111", "the listed call keeps a value the app sent")
	require.Empty(t, ev.Background[0].ReceivedReq, "the listed call keeps the request the app sent")
	require.Equal(t, "mock-0", ev.Background[0].ClosestMock, "the listed call does not name the recording that answered it")
	require.True(t, ev.Background[0].Background)
	require.False(t, ev.Background[0].CountsAgainstTest())

	_, e = runTest(t, a, p, "TestC", "/c", "/own")
	require.Equal(t, models.ScopeActionFail, e.Action, "TestC's own miss did not fail it")
	ev = evidenceOf(t, a, "o::TestC")
	require.Len(t, ev.Misses, 1, "TestC's own miss is not its evidence")
	require.Equal(t, "GET /own", ev.Misses[0].ActualSummary)
	require.Empty(t, ev.Background, "an unmarked miss is listed as a background one")
}

// A background call keploy answered (an Answer: a gRPC health Watch it
// answered SERVING itself) is no miss: it is listed neither as evidence nor
// as a background miss of the test.
func TestABackgroundCallKeployAnsweredIsNoBackgroundMiss(t *testing.T) {
	a, p := exactAgent(t, []*models.Mock{pingMock("mock-1", "/b", "b", t0)},
		map[string][]string{"o::TestB": {"mock-1"}}, map[string][]string{"o": nil})
	p.background = func(uc *models.UnmatchedCall) {
		if strings.HasSuffix(uc.ActualSummary, "/watch") {
			uc.Background, uc.Answer = true, "unrecorded_health_watch"
		}
	}
	_, e := runTest(t, a, p, "TestB", "/b", "/watch")
	require.NotEqual(t, models.ScopeActionFail, e.Action, e.Reason)
	ws, err := a.GetScopeWindows(context.Background())
	require.NoError(t, err)
	seen := false
	for _, w := range ws {
		if w.Name == "o::TestB" {
			seen = true
			require.False(t, w.Evidence.Kept(), "an answered call is kept as TestB's evidence: %+v", w.Evidence)
		}
	}
	require.True(t, seen, "no window of TestB")
}
