package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// trailRun replays, under the switch, a set whose worker scope holds mock-3,
// a trailing write of o::TestA (a trail move put it there), and mock-4, an
// ordinary background recording, with the agent reporting windows and
// consumed (consumedErr: the read of what it consumed failed). It returns the
// exit code, the receipt and the logs.
func trailRun(t *testing.T, windows []models.ScopeWindow, consumed []models.MockState, consumedErr error) (int, *Receipt, *TestsReceipt, *observer.ObservedLogs) {
	t.Helper()
	return trailRunExit(t, models.AppError{AppErrorType: models.ErrAppStopped}, windows, consumed, consumedErr)
}

// trailRunExit is trailRun with the test command ending as exit says.
func trailRunExit(t *testing.T, exit models.AppError, windows []models.ScopeWindow, consumed []models.MockState, consumedErr error) (int, *Receipt, *TestsReceipt, *observer.ObservedLogs) {
	t.Helper()
	provenOn(t)
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: TestMocksMapping\ntest_set_id: set\ntests:\n    - id: o::TestA\n      dir: o\n      mocks:\n        - name: mock-0\n    - id: o::TestB\n      dir: o\n      mocks:\n        - name: mock-1\nworker:\n    - name: mock-3\n      trails: o::TestA\n    - name: mock-4\n"), 0o644))
	core, logs := observer.New(zapcore.DebugLevel)
	instr := newInstr(t, agentUpFromSetup, false, exit)
	instr.consumedMocks, instr.consumedErr, instr.windows = consumed, consumedErr, windows
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	_ = New(zap.New(core), instr, named("mock-0", "mock-1", "mock-3", "mock-4"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	tests, err := ReadTestsReceipt(keployDir, "set")
	require.NoError(t, err)
	require.NotNil(t, tests)
	return utils.ErrCode, mustReceipt(t, keployDir), tests, logs
}

// testLine is the receipt's line of the test named name.
func testLine(t *testing.T, r *TestsReceipt, name string) TestReceipt {
	t.Helper()
	i := slices.IndexFunc(r.Tests, func(tr TestReceipt) bool { return tr.Name == name })
	require.GreaterOrEqual(t, i, 0, "the receipt has no line for %s", name)
	return r.Tests[i]
}

// A recording a trail move put in the worker scope, which no declared rule
// placed in a series, is served, not required: a run of the test it trails
// that never makes it passes, as does the run, with no ERROR. keploy
// requires a trailing write once per run by its series (requiredSeries), and
// one copy of a call the application repeats a number of times that depends
// on how long the run takes is not required on its own: such a check failed
// tests that passed. A run that made it passes too.
func TestATrailMovesRecordingWithNoSeriesIsServedNotRequired(t *testing.T) {
	at := time.Now()
	ran := []models.ScopeWindow{
		{Name: "o::TestA", Start: at, End: at.Add(time.Second), Outcome: models.ScopeOutcomePassed},
		{Name: "o::TestB", Start: at.Add(2 * time.Second), End: at.Add(3 * time.Second), Outcome: models.ScopeOutcomePassed},
	}
	for _, consumed := range [][]models.MockState{
		{{Name: "mock-0"}, {Name: "mock-1"}},
		{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-3"}},
	} {
		code, r, tr, logs := trailRun(t, ran, consumed, nil)
		require.Equal(t, 0, code, "consumed %v", consumed)
		require.Empty(t, r.FailedBy)
		require.Nil(t, r.RequiredNotMade)
		a := testLine(t, tr, "o::TestA")
		require.Equal(t, models.ScopeOutcomePassed, a.Outcome)
		if a.Evidence != nil {
			require.Empty(t, a.Evidence.Unconsumed)
			require.False(t, a.Evidence.Unread)
		}
		require.Zero(t, logs.FilterLevelExact(zapcore.ErrorLevel).Len(), "no ERROR for a recording no series holds")
	}
	// What the run consumed cannot be read: with no series due, nothing is
	// left unread for the trail.
	_, _, tr, _ := trailRun(t, ran, nil, errors.New("connection reset by peer"))
	if a := testLine(t, tr, "o::TestA"); a.Evidence != nil {
		require.NotContains(t, a.Evidence.Reason, trailsUnreadReason)
	}
}
