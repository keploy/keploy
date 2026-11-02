package mock

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// The spans of a recorded run of a shop paid by a provider's callback: the
// test's GET waits for the order to be paid while the callback's handler
// looks the charge up. Which of the two began first varies from run to run.
const (
	lookupCall   = "GET 127.0.0.1:18111/v1/charges/ch_1"
	leftCallback = "case post-webhooks-pay-1: case_mocks leaves out 2 calls made while other cases were in flight too (get-orders-o-1-paid-1), as the time alone cannot tell whose handler made them: mock-1 (" + lookupCall + "), mock-2 (PostgresV2)"
	leftPaid     = "case get-orders-o-1-paid-1: case_mocks leaves out 2 calls made while other cases were in flight too (post-webhooks-pay-1), as the time alone cannot tell whose handler made them: mock-1 (" + lookupCall + "), mock-2 (PostgresV2)"
)

// A call made while two cases of a test were in flight is charged to
// neither, whichever began first: a published case lists its case_mocks as
// its own calls, and only one handler made it. The record says so a line a
// case, with the calls and the other cases. A call made while one case was
// in flight is that case's alone, said nowhere.
func TestACallMadeWhileTwoCasesWereInFlightIsChargedToNeither(t *testing.T) {
	at := func(us int) time.Time { return runnerT0.Add(time.Duration(us) * time.Microsecond) }
	windows := []models.ScopeWindow{{Name: paidTest, Start: at(0), End: at(200_000)}}
	place := capturedMock{name: "post-orders-1", ts: at(10_000), end: at(26_000), call: "POST /orders"}
	charge := capturedMock{name: "mock-0", kind: string(models.HTTP), ts: at(15_000), call: "POST 127.0.0.1:18111/v1/charges"}
	for _, tc := range []struct {
		name           string
		paid, callback capturedMock
		lookup         time.Time
		lines          []string
	}{
		{
			// As recorded: the GET began 0.37 ms before the callback.
			name:     "the GET first",
			paid:     capturedMock{name: "get-orders-o-1-paid-1", ts: at(27_997), end: at(58_258), call: "GET /orders/o-1/paid"},
			callback: capturedMock{name: "post-webhooks-pay-1", ts: at(28_363), end: at(58_307), call: payCall},
			lookup:   at(53_264),
			lines:    []string{leftPaid, leftCallback},
		},
		{
			// As recorded: the callback came first, and the GET began and
			// ended inside its span, before its handler's lookup.
			name:     "the callback first",
			paid:     capturedMock{name: "get-orders-o-1-paid-1", ts: at(33_000), end: at(52_000), call: "GET /orders/o-1/paid"},
			callback: capturedMock{name: "post-webhooks-pay-1", ts: at(28_000), end: at(60_000), call: payCall},
			lookup:   at(50_000),
			lines:    []string{leftCallback, leftPaid},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := capturedMock{name: "mock-1", kind: string(models.HTTP), ts: tc.lookup, call: lookupCall}
			// A call with no HTTP call to name is named by its kind.
			session := capturedMock{name: "mock-2", kind: string(models.PostgresV2), ts: tc.lookup.Add(time.Microsecond)}
			mocks := []capturedMock{charge, lookup, session}
			// The cases in the order they were captured: that order is the lines'.
			cases := []capturedMock{place, tc.paid, tc.callback}
			if tc.callback.ts.Before(tc.paid.ts) {
				cases = []capturedMock{place, tc.callback, tc.paid}
			}
			got, _ := correlateCases(windows, mocks, cases, nil)
			require.Equal(t, map[string][]string{"post-orders-1": {"mock-0"}}, got[paidTest].CaseMocks)
			// The same lines each time: the calls come from maps.
			for range 20 {
				_, shared := correlateCases(windows, mocks, cases, nil)
				require.Equal(t, tc.lines, sharedLines(cases, shared))
			}
		})
	}
	got, shared := correlateCases(windows, []capturedMock{charge}, []capturedMock{place}, nil)
	require.Equal(t, map[string][]string{"post-orders-1": {"mock-0"}}, got[paidTest].CaseMocks)
	require.Empty(t, sharedLines([]capturedMock{place}, shared), "a call one case holds is said nowhere")
}

// A page that loads with many requests at once: each call made while three
// cases were in flight is left out of all three, and the record says so in
// one line a case, never a line a call. A call made after the other two
// ended is the last case's alone.
func TestCallsOfThreeCasesInFlightAreSaidOnceACase(t *testing.T) {
	at := func(us int) time.Time { return runnerT0.Add(time.Duration(us) * time.Microsecond) }
	windows := []models.ScopeWindow{{Name: paidTest, Start: at(0), End: at(200_000)}}
	// Begun out of their names' order: a line names the other cases by name,
	// and the lines come in the order the cases were captured.
	cases := []capturedMock{
		{name: "get-b-1", ts: at(10_000), end: at(40_000), call: "GET /b"},
		{name: "get-a-1", ts: at(11_000), end: at(41_000), call: "GET /a"},
		{name: "get-c-1", ts: at(12_000), end: at(60_000), call: "GET /c"},
	}
	// Handed over out of the order they were made in, two at one instant:
	// a line lists the calls by when they were made, then by name.
	var mocks []capturedMock
	for _, m := range []struct {
		n  int
		us int
	}{{4, 24_000}, {7, 22_000}, {6, 22_000}, {9, 20_000}, {3, 21_000}, {5, 50_000}} {
		mocks = append(mocks, capturedMock{name: fmt.Sprintf("mock-%d", m.n), kind: string(models.PostgresV2), ts: at(m.us)})
	}
	got, shared := correlateCases(windows, mocks, cases, nil)
	require.Equal(t, map[string][]string{"get-c-1": {"mock-5"}}, got[paidTest].CaseMocks)
	calls := "mock-9 (PostgresV2), mock-3 (PostgresV2), mock-6 (PostgresV2), mock-7 (PostgresV2), mock-4 (PostgresV2)"
	require.Equal(t, []string{
		"case get-b-1: case_mocks leaves out 5 calls made while other cases were in flight too (get-a-1, get-c-1), as the time alone cannot tell whose handler made them: " + calls,
		"case get-a-1: case_mocks leaves out 5 calls made while other cases were in flight too (get-b-1, get-c-1), as the time alone cannot tell whose handler made them: " + calls,
		"case get-c-1: case_mocks leaves out 5 calls made while other cases were in flight too (get-a-1, get-b-1), as the time alone cannot tell whose handler made them: " + calls,
	}, sharedLines(cases, shared))
}

// A case is in flight from its request's first instant to its response's
// last, both included: a call made at the instant one case began, or the
// instant one ended, while another was in flight is left out of both.
func TestACallAtACasesFirstOrLastInstantIsInFlight(t *testing.T) {
	at := func(us int) time.Time { return runnerT0.Add(time.Duration(us) * time.Microsecond) }
	windows := []models.ScopeWindow{{Name: paidTest, Start: at(0), End: at(200_000)}}
	cases := []capturedMock{
		{name: "get-a-1", ts: at(10_000), end: at(40_000), call: "GET /a"},
		{name: "get-b-1", ts: at(20_000), end: at(60_000), call: "GET /b"},
	}
	var mocks []capturedMock
	for _, m := range []struct {
		n  int
		us int
	}{{1, 20_000}, {2, 40_000}, {3, 15_000}, {4, 50_000}} {
		mocks = append(mocks, capturedMock{name: fmt.Sprintf("mock-%d", m.n), kind: string(models.PostgresV2), ts: at(m.us)})
	}
	got, shared := correlateCases(windows, mocks, cases, nil)
	require.Equal(t, map[string][]string{"get-a-1": {"mock-3"}, "get-b-1": {"mock-4"}}, got[paidTest].CaseMocks,
		"mock-1 at get-b-1's first instant and mock-2 at get-a-1's last are in both")
	calls := "mock-1 (PostgresV2), mock-2 (PostgresV2)"
	require.Equal(t, []string{
		"case get-a-1: case_mocks leaves out 2 calls made while other cases were in flight too (get-b-1), as the time alone cannot tell whose handler made them: " + calls,
		"case get-b-1: case_mocks leaves out 2 calls made while other cases were in flight too (get-a-1), as the time alone cannot tell whose handler made them: " + calls,
	}, sharedLines(cases, shared))
}

// Recorded, a callback's lookup made while the test's GET was in flight too
// is in neither case's case_mocks, and keploy record says why at INFO, once
// a case.
func TestRecordLeavesACallTwoCasesInFlightShareOutAndSaysSo(t *testing.T) {
	provenOn(t)
	dir := t.TempDir()
	w := mark(paidTest, 0, 200*time.Millisecond)
	w.Outcome, w.Dir = models.ScopeOutcomePassed, "pay"
	instr := newRunnerInstr(t, w)
	paid := httpCase("get-orders-o-1-paid-1", "GET", "http://localhost:8080/orders/o-1/paid", 200, "{}", runnerT0.Add(28*time.Millisecond))
	paid.HTTPResp.Timestamp = runnerT0.Add(58 * time.Millisecond)
	callback := httpCase("post-webhooks-pay-1", "POST", "http://localhost:8080/webhooks/pay", 200, "ok", runnerT0.Add(29*time.Millisecond))
	callback.HTTPResp.Timestamp = runnerT0.Add(59 * time.Millisecond)
	instr.incoming = []*models.TestCase{paid, callback}
	instr.mocks = []*models.Mock{httpMockAt("GET", "http://127.0.0.1:18111/v1/charges/ch_1", runnerT0.Add(53*time.Millisecond))}
	cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path = dir
	withRequests(cfg)
	core, logs := observer.New(zap.InfoLevel)
	svc := New(zap.New(core), instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
	svc.(TestDBSetter).SetTestDB(&memTestDB{})
	require.NoError(t, svc.Record(context.Background()))

	mp, err := mapdb.New(zap.NewNop(), dir, "").GetMapping(context.Background(), "set")
	require.NoError(t, err)
	require.Len(t, mp.TestCases, 1)
	require.Empty(t, mp.TestCases[0].CaseMocks)
	require.Equal(t, []string{"mock-0"}, func() []string {
		var n []string
		for _, e := range mp.TestCases[0].Mocks {
			n = append(n, e.Name)
		}
		return n
	}(), "the call stays in the test's mocks")
	for _, l := range []string{
		"case get-orders-o-1-paid-1: case_mocks leaves out 1 call made while other cases were in flight too (post-webhooks-pay-1), as the time alone cannot tell whose handler made them: mock-0 (" + lookupCall + ")",
		"case post-webhooks-pay-1: case_mocks leaves out 1 call made while other cases were in flight too (get-orders-o-1-paid-1), as the time alone cannot tell whose handler made them: mock-0 (" + lookupCall + ")",
	} {
		said := logs.FilterMessage(l).All()
		require.Len(t, said, 1, "the record says each case's left-out calls once: %v", logs.All())
		require.Equal(t, zap.InfoLevel, said[0].Level)
	}
	require.Equal(t, 2, logs.FilterMessageSnippet("case_mocks leaves out").Len(), "a line a case: %v", logs.All())
}
