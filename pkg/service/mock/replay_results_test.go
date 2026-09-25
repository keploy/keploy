package mock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

const jsonResults = `{"Time":"2026-09-24T10:00:00Z","Action":"run","Package":"orders/e2e","Test":"TestA"}
{"Time":"2026-09-24T10:00:00.01Z","Action":"pass","Package":"orders/e2e","Test":"TestA","Elapsed":0.01}
{"Time":"2026-09-24T10:00:00.01Z","Action":"run","Package":"orders/e2e","Test":"TestB"}
{"Time":"2026-09-24T10:00:00.03Z","Action":"output","Package":"orders/e2e","Test":"TestB","Output":"    orders_test.go:12: want 2 got 3\n"}
{"Time":"2026-09-24T10:00:00.03Z","Action":"fail","Package":"orders/e2e","Test":"TestB","Elapsed":0.02}
{"Time":"2026-09-24T10:00:00.03Z","Action":"run","Package":"orders/e2e","Test":"TestC"}
{"Time":"2026-09-24T10:00:00.03Z","Action":"skip","Package":"orders/e2e","Test":"TestC","Elapsed":0}
{"Time":"2026-09-24T10:00:00.04Z","Action":"fail","Package":"orders/e2e","Elapsed":0.04}
`

const plainResults = `=== RUN   TestA
--- PASS: TestA (0.01s)
=== RUN   TestB
    orders_test.go:12: want 2 got 3
--- FAIL: TestB (0.02s)
=== RUN   TestC
--- SKIP: TestC (0.00s)
FAIL
`

func TestRunnerScopeCollectsTheRunnerResults(t *testing.T) {
	_, scope := feed(t, jsonResults)
	require.Equal(t, []TestOutcome{
		{Name: "orders/e2e.TestA", Status: "pass", Duration: 10 * time.Millisecond},
		{Name: "orders/e2e.TestB", Status: "fail", Duration: 20 * time.Millisecond},
		{Name: "orders/e2e.TestC", Status: "skip"},
	}, scope.tests())

	_, scope = feed(t, plainResults)
	require.Equal(t, []TestOutcome{
		{Name: "TestA", Status: "pass", Duration: 10 * time.Millisecond},
		{Name: "TestB", Status: "fail", Duration: 20 * time.Millisecond},
		{Name: "TestC", Status: "skip"},
	}, scope.tests())
}

// Subtests report too, in the order the runner printed their results.
func TestRunnerScopeCollectsSubtestResults(t *testing.T) {
	_, scope := feed(t, plainSequential)
	names := make([]string, 0)
	for _, r := range scope.tests() {
		names = append(names, r.Name+":"+r.Status)
	}
	require.Equal(t, []string{"TestA:pass", "TestA/one:pass", "TestA/two:skip", "TestC:pass"}, names)
}

// replayWith runs one replay and returns what the outcome reporter was handed.
func replayWith(t *testing.T, instr *runnerInstr, tweak func(*config.Config)) (ReplayOutcome, error) {
	t.Helper()
	var got ReplayOutcome
	RegisterReplayOutcomeReporter(func(_ context.Context, o ReplayOutcome) { got = o })
	t.Cleanup(func() { RegisterReplayOutcomeReporter(nil) })
	cfg := instrConfig(instr.composeInstr, utils.Native, "./shop.test -test.v")
	cfg.Path = t.TempDir()
	if tweak != nil {
		tweak(cfg)
	}
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	err := New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, cfg).Replay(context.Background())
	return got, err
}

func TestReplayOutcomeCarriesTheRunnerResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		prefix string
	}{
		{"json", jsonResults, "orders/e2e."},
		{"plain", plainResults, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instr := newRunnerInstr(t, tc.output)
			got, err := replayWith(t, instr, nil)
			require.NoError(t, err)
			require.Equal(t, "set", got.SetName)
			require.Equal(t, []TestOutcome{
				{Name: tc.prefix + "TestA", Status: "pass", Duration: 10 * time.Millisecond},
				{Name: tc.prefix + "TestB", Status: "fail", Duration: 20 * time.Millisecond},
				{Name: tc.prefix + "TestC", Status: "skip"},
			}, got.Tests)
			marks, observed := instr.seen()
			require.True(t, observed)
			require.Empty(t, marks, "replay reads results only: a boundary read from output lags the test and would restrict the pool to the wrong test")
		})
	}
}

func TestReplayOutcomeHasNoTestsWhenTheAdapterIsOff(t *testing.T) {
	instr := newRunnerInstr(t, jsonResults)
	got, err := replayWith(t, instr, func(cfg *config.Config) { cfg.Mock.NoRunnerScope = true })
	require.NoError(t, err)
	require.Equal(t, "set", got.SetName, "the reporter still fires, with no tests")
	require.Empty(t, got.Tests)
	marks, observed := instr.seen()
	require.False(t, observed)
	require.Empty(t, marks)
}
