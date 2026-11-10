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

// A recording a trail move put in the worker scope is required once per run
// of the test it trails: a run of that test that never makes it fails the
// test, naming the recording as one it did not make, and the run, whatever
// the test command exits, with an ERROR naming the recording and the test. A
// run that made it once passes. An ordinary background recording of the
// worker scope stays not required, made or not. A run that did not run the
// trailed test requires none of its trails.
func TestATrailingWriteIsRequiredOncePerRunOfItsTest(t *testing.T) {
	at := time.Now()
	ran := func(outcome string) []models.ScopeWindow {
		return []models.ScopeWindow{
			{Name: "o::TestA", Start: at, End: at.Add(time.Second), Outcome: outcome},
			{Name: "o::TestB", Start: at.Add(2 * time.Second), End: at.Add(3 * time.Second), Outcome: models.ScopeOutcomePassed},
		}
	}
	t.Run("never made: the test and the run fail", func(t *testing.T) {
		code, r, tr, logs := trailRun(t, ran(models.ScopeOutcomePassed), []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}, nil)
		require.Equal(t, 1, code)
		require.Equal(t, FailedByTrailUnmade, r.FailedBy)
		a := testLine(t, tr, "o::TestA")
		require.Equal(t, models.ScopeOutcomeFailed, a.Outcome)
		require.NotNil(t, a.Evidence)
		require.Equal(t, []string{"mock-3"}, a.Evidence.Unconsumed, "the trailing write is named as a call TestA did not make; mock-4 is no test's")
		require.Equal(t, models.ScopeOutcomePassed, testLine(t, tr, "o::TestB").Outcome, "only the trailed test fails")
		var named int
		for _, e := range logs.FilterMessageSnippet("never made a call keploy recorded as a trailing write").All() {
			if f := e.ContextMap(); e.Level == zapcore.ErrorLevel && f["mock"] == "mock-3" && f["test"] == "o::TestA" {
				named++
			}
		}
		require.Equal(t, 1, named, "one ERROR names the trailing write and its test")
	})
	t.Run("never made by a test that failed: it keeps failing and names it", func(t *testing.T) {
		code, _, tr, _ := trailRun(t, ran(models.ScopeOutcomeFailed), []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}, nil)
		require.Equal(t, 1, code)
		require.Equal(t, []string{"mock-3"}, testLine(t, tr, "o::TestA").Evidence.Unconsumed)
	})
	t.Run("never made, the test command failing: its exit and its failure's name are kept", func(t *testing.T) {
		code, r, tr, _ := trailRunExit(t, models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3}, ran(models.ScopeOutcomePassed), []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}}, nil)
		require.Equal(t, 3, code)
		require.Equal(t, FailedByRunner, r.FailedBy)
		require.Equal(t, []string{"mock-3"}, testLine(t, tr, "o::TestA").Evidence.Unconsumed)
	})
	t.Run("made once: passes", func(t *testing.T) {
		code, r, tr, logs := trailRun(t, ran(models.ScopeOutcomePassed), []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-3"}}, nil)
		require.Equal(t, 0, code)
		require.Empty(t, r.FailedBy)
		require.Equal(t, models.ScopeOutcomePassed, testLine(t, tr, "o::TestA").Outcome)
		require.Zero(t, logs.FilterMessageSnippet("trailing write").Len())
	})
	t.Run("the trailed test did not run: none required", func(t *testing.T) {
		skipped := ran(models.ScopeOutcomeSkipped)
		code, r, tr, _ := trailRun(t, skipped, []models.MockState{{Name: "mock-1"}}, nil)
		require.Equal(t, 0, code)
		require.Empty(t, r.FailedBy)
		require.Equal(t, models.ScopeOutcomeSkipped, testLine(t, tr, "o::TestA").Outcome)
	})
	t.Run("what the run consumed cannot be read: the trailed test is unread and the run fails", func(t *testing.T) {
		code, r, tr, _ := trailRun(t, ran(models.ScopeOutcomePassed), nil, errors.New("connection reset by peer"))
		require.Equal(t, 1, code)
		require.Equal(t, FailedByTrailUnmade, r.FailedBy)
		a := testLine(t, tr, "o::TestA")
		require.NotNil(t, a.Evidence)
		require.True(t, a.Evidence.Unread)
		require.Equal(t, models.ScopeOutcomePassed, a.Outcome, "unread, not failed: keploy cannot say it was not made")
		require.Contains(t, a.Evidence.Reason, "cannot tell whether the run made this test's trailing writes")
		if b := testLine(t, tr, "o::TestB"); b.Evidence != nil {
			require.False(t, b.Evidence.Unread, "TestB has no trailing write")
		}
	})
}

// An ordinary background recording of the worker scope is not required: the
// same set without the trail passes a run that never made either worker
// recording.
func TestAnOrdinaryWorkerRecordingStaysNotRequired(t *testing.T) {
	require.Nil(t, requiredTrails(&models.Mapping{Worker: []models.MockEntry{{Name: "mock-4"}}}, map[string][]string{"o::TestA": {}}))
	require.Nil(t, requiredTrails(&models.Mapping{Worker: []models.MockEntry{{Name: "mock-3", Trails: "o::TestGone"}}}, map[string][]string{"o::TestA": {}}),
		"a trail naming no test of the set trails none")
	require.Nil(t, requiredTrails(nil, map[string][]string{"o::TestA": {}}))
	require.Equal(t, map[string]string{"mock-3": "o::TestA"}, requiredTrails(&models.Mapping{Worker: []models.MockEntry{{Name: "mock-3", Trails: "o::TestA"}, {Name: "mock-4"}}}, map[string][]string{"o::TestA": {}}),
		"required whether or not the agent holds it")
	require.Nil(t, requiredTrails(&models.Mapping{Worker: []models.MockEntry{{Name: "mock-4"}}}, map[string][]string{"": {}}),
		"an entry that trails no test trails none, even where the set's mappings.yaml lists a test with no id")
	unmade, unread := unmadeTrails(nil, []TestReceipt{{Name: "o::TestA", Outcome: models.ScopeOutcomePassed}}, nil, true)
	require.Nil(t, unmade)
	require.Nil(t, unread)
}

// failUnmadeTrails touches only a test with a trailing write the run did not
// make, or one it could not read: every other test's line is left as the run
// wrote it, with no evidence added.
func TestFailingUnmadeTrailsLeavesEveryOtherTestAsTheRunWroteIt(t *testing.T) {
	got := failUnmadeTrails([]TestReceipt{
		{Name: "o::TestA", Outcome: models.ScopeOutcomePassed},
		{Name: "o::TestB", Outcome: models.ScopeOutcomePassed},
		{Name: "o::TestC", Outcome: models.ScopeOutcomeFailed},
	}, map[string]string{"mock-3": "o::TestA"}, nil)
	require.Equal(t, models.ScopeOutcomeFailed, got[0].Outcome)
	require.Equal(t, []string{"mock-3"}, got[0].Evidence.Unconsumed)
	require.Equal(t, TestReceipt{Name: "o::TestB", Outcome: models.ScopeOutcomePassed}, got[1], "a test with no trailing write gets no evidence")
	require.Equal(t, TestReceipt{Name: "o::TestC", Outcome: models.ScopeOutcomeFailed}, got[2])
}
