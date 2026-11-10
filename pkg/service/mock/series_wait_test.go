package mock

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
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

// seriesWaitRun replays, under the switch, the set of seriesMappings with the
// agent reporting windows, handing reads to the reads of what it consumed one
// each and then final, and with runsOn saying whether what the test command
// left of the application runs on once it exited (nil: nothing does). The
// wait wakes at once, and stops after stallLimit wake-ups with no progress.
// It returns the exit code, the receipt, the tests' receipt, the logs and the
// agent.
func seriesWaitRun(t *testing.T, windows []models.ScopeWindow, reads [][]models.MockState, final []models.MockState, runsOn func() bool, stallLimit int) (int, *Receipt, *TestsReceipt, *observer.ObservedLogs, *composeInstr) {
	t.Helper()
	provenOn(t)
	wake, limit := seriesWake, seriesStallLimit
	t.Cleanup(func() { seriesWake, seriesStallLimit = wake, limit })
	seriesStallLimit = stallLimit
	seriesWake = func(context.Context) (func() <-chan time.Time, func()) {
		return func() <-chan time.Time {
			c := make(chan time.Time, 1)
			c <- time.Now()
			return c
		}, func() {}
	}
	repo := t.TempDir()
	t.Chdir(repo)
	keployDir := filepath.Join(repo, "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mocks.yaml"), []byte("version: api.keploy.io/v1beta1\nkind: Http\nname: mock-0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte(seriesMappings), 0o644))
	core, logs := observer.New(zapcore.DebugLevel)
	instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	instr.consumedReads, instr.consumedMocks, instr.windows, instr.runsOn = reads, final, windows, runsOn
	cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
	cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	_ = New(zap.New(core), instr, named("mock-0", "mock-1", "mock-3", "mock-4", "mock-5", "mock-6", "mock-7"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg).Replay(context.Background())
	tests, err := ReadTestsReceipt(keployDir, "set")
	require.NoError(t, err)
	require.NotNil(t, tests)
	return utils.ErrCode, mustReceipt(t, keployDir), tests, logs, instr
}

// runsOnForGood is a runsOn that says the application runs on for good.
func runsOnForGood() bool { return true }

// A trailing clean-up the application makes after the test command exited
// (from a goroutine, or a sweep, of what the command left running) is waited
// for: it lands two wake-ups into the wait, with one wake-up of no progress
// before it, and the run passes on it. What the wait read is the run's: the
// end's read, which hands back only what came after, finds nothing more.
func TestASeriesMadeAfterTheCommandExitedIsWaitedFor(t *testing.T) {
	code, r, tr, logs, instr := seriesWaitRun(t, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed),
		[][]models.MockState{{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-6"}}, nil, {{Name: "mock-5"}}}, nil, runsOnForGood, 3)
	require.Zero(t, code)
	require.Empty(t, r.RequiredNotMade)
	require.Equal(t, models.ScopeOutcomePassed, testLine(t, tr, "o::TestA").Outcome)
	require.Empty(t, ruleNotMadeLines(logs))
	require.Equal(t, 1, logs.FilterMessageSnippet("waiting for the series of declared rules the run owes").Len())
	require.Equal(t, 4, instr.consumedReadCount, "the wait's first read, two wake-ups, and the run's end")
}

// One the application no longer makes holds the run for seriesStallLimit
// wake-ups with none of the owed series answered, no more, and is then not
// made: the run fails on it. Progress on another owed series (the background
// one) starts the count again.
func TestASeriesTheAppNoLongerMakesIsNotMadeAfterTheStall(t *testing.T) {
	code, r, _, logs, instr := seriesWaitRun(t, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed),
		[][]models.MockState{{{Name: "mock-0"}, {Name: "mock-1"}}, nil, nil, {{Name: "mock-7"}}}, nil, runsOnForGood, 3)
	require.Equal(t, 1, code)
	require.Equal(t, []RequiredNotMade{{Series: "cleanup/o::TestA/1", Rule: "cleanup@3f2a91c0", Trails: "o::TestA", Names: []string{"mock-3", "mock-5"}}}, r.RequiredNotMade)
	require.Len(t, ruleNotMadeLines(logs), 1)
	// The first read, three wake-ups to the background series' answer, three
	// with none after it, and the run's end.
	require.Equal(t, 8, instr.consumedReadCount)
}

// With nothing of the application running once the test command exited, the
// run checks at once: no wait, no read but the end's, and the series not
// made fails the run. One that stops running while the wait goes on ends it
// at the next wake-up, with no read after: the series it would have made two
// wake-ups later is not waited for.
func TestTheSeriesAreCheckedAtOnceWhenTheAppExited(t *testing.T) {
	windows := seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomePassed)
	reads := func() [][]models.MockState {
		return [][]models.MockState{{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-6"}}, nil, nil, {{Name: "mock-3"}}}
	}
	for _, tc := range []struct {
		name   string
		runsOn func() bool
		reads  int
	}{
		{"nothing runs on", nil, 1},
		{"it stops running", stopsAfter(2), 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, r, _, _, instr := seriesWaitRun(t, windows, reads(), []models.MockState{{Name: "mock-0"}, {Name: "mock-1"}, {Name: "mock-6"}}, tc.runsOn, 100)
			require.Equal(t, 1, code)
			require.Len(t, r.RequiredNotMade, 1)
			require.Equal(t, tc.reads, instr.consumedReadCount)
		})
	}
}

// stopsAfter is a runsOn that says the application runs on for its first n
// asks, and not after.
func stopsAfter(n int32) func() bool {
	var asked atomic.Int32
	return func() bool { return asked.Add(1) <= n }
}

// Only a due series holds the wait: a background series placed after a test
// that was skipped is not due, and with the trailing one already made the
// wait reads once and ends.
func TestASeriesNotDueHoldsNoWait(t *testing.T) {
	code, r, _, logs, instr := seriesWaitRun(t, seriesWindows(models.ScopeOutcomePassed, models.ScopeOutcomeSkipped),
		[][]models.MockState{{{Name: "mock-0"}, {Name: "mock-3"}}}, nil, runsOnForGood, 3)
	require.Zero(t, code)
	require.Empty(t, r.RequiredNotMade)
	require.Zero(t, logs.FilterMessageSnippet("waiting for the series of declared rules the run owes").Len())
	require.Equal(t, 2, instr.consumedReadCount, "the wait's one read and the run's end")
}

// The wait's wake-ups come soon at first, then at most seriesWakeMost apart.
func TestTheSeriesWaitWakesSoonThenAtMostItsLongest(t *testing.T) {
	for n, want := range map[int]time.Duration{0: seriesWakeFirst, 1: 2 * seriesWakeFirst, 3: 8 * seriesWakeFirst, 4: seriesWakeMost, 40: seriesWakeMost} {
		require.Equal(t, want, seriesWakeAfter(n), "wake-up %d", n)
	}
}
