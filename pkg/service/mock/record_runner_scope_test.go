package mock

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// runnerInstr is a native instrumentation whose wrapped command prints output,
// whose proxy captures mocks, and whose agent answers the scope API.
type runnerInstr struct {
	*composeInstr
	output   string
	mocks    []*models.Mock
	windows  []models.ScopeWindow
	incoming []*models.TestCase

	mu           sync.Mutex
	marks        []string
	opened       map[string]time.Time
	echoed       []models.ScopeWindow
	observed     bool
	setupOpts    models.SetupOptions
	incomingRead bool
}

func (r *runnerInstr) Setup(_ context.Context, _ string, opts models.SetupOptions) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setupOpts = opts
	return nil
}

func (r *runnerInstr) GetIncoming(_ context.Context, _ models.IncomingOptions) (<-chan *models.TestCase, error) {
	r.mu.Lock()
	r.incomingRead = true
	r.mu.Unlock()
	out := make(chan *models.TestCase, len(r.incoming))
	for _, tc := range r.incoming {
		out <- tc
	}
	close(out)
	return out, nil
}

func (r *runnerInstr) setup() (models.SetupOptions, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.setupOpts, r.incomingRead
}

func newRunnerInstr(t *testing.T, output string) *runnerInstr {
	t.Helper()
	return &runnerInstr{composeInstr: newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped}), output: output}
}

func (r *runnerInstr) Run(ctx context.Context, opts models.RunOptions) models.AppError {
	if opts.StdoutObserver != nil {
		r.mu.Lock()
		r.observed = true
		r.mu.Unlock()
		_, _ = io.WriteString(opts.StdoutObserver, r.output)
	}
	return r.composeInstr.Run(ctx, opts)
}

func (r *runnerInstr) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	if _, err := r.composeInstr.GetOutgoing(ctx, opts); err != nil {
		return nil, err
	}
	out := make(chan *models.Mock, len(r.mocks))
	for _, mk := range r.mocks {
		out <- mk
	}
	close(out)
	return out, nil
}

func (r *runnerInstr) GetScopeWindows(context.Context) ([]models.ScopeWindow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append(append([]models.ScopeWindow(nil), r.windows...), r.echoed...), nil
}

func (r *runnerInstr) BeginScope(_ context.Context, name string, _ int, at time.Time) error {
	r.mu.Lock()
	if r.opened == nil {
		r.opened = map[string]time.Time{}
	}
	r.opened[name] = at
	r.mu.Unlock()
	return r.mark("begin " + name)
}

func (r *runnerInstr) EndScope(_ context.Context, name string, _ int, at time.Time) error {
	r.mu.Lock()
	if start, ok := r.opened[name]; ok {
		r.echoed = append(r.echoed, models.ScopeWindow{Name: name, Start: start, End: at})
	}
	r.mu.Unlock()
	return r.mark("end " + name)
}

func (r *runnerInstr) mark(s string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.marks = append(r.marks, s)
	return nil
}

func (r *runnerInstr) seen() (marks []string, observed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.marks...), r.observed
}

func recordSet(t *testing.T, instr *runnerInstr, mapDB MappingDB, tweak func(*config.Config)) error {
	t.Helper()
	return recordSetLogging(t, zap.NewNop(), instr, mapDB, tweak)
}

func recordSetLogging(t *testing.T, logger *zap.Logger, instr *runnerInstr, mapDB MappingDB, tweak func(*config.Config)) error {
	t.Helper()
	return recordSetWith(t, logger, instr, mapDB, nil, tweak)
}

func recordSetWith(t *testing.T, logger *zap.Logger, instr *runnerInstr, mapDB MappingDB, testDB TestDB, tweak func(*config.Config)) error {
	t.Helper()
	cfg := instrConfig(instr.composeInstr, utils.Native, "./shop.test -test.v")
	cfg.Path = t.TempDir()
	if tweak != nil {
		tweak(cfg)
	}
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	svc := New(logger, instr, stubMockDB{}, mapDB, nil, nil, cfg)
	if testDB != nil {
		svc.(TestDBSetter).SetTestDB(testDB)
	}
	return svc.Record(context.Background())
}

const outputTimingWarn = "test boundaries taken from output timing"

// Plain output gives read-time boundaries; the record says so once and counts the mocks that sat close to one.
func TestRecordWarnsOnceWhenBoundariesCameFromOutputTiming(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	instr := newRunnerInstr(t, plainSequential)
	instr.windows = []models.ScopeWindow{{Name: "fixture.A", Start: ts(10), End: ts(20)}, {Name: "fixture.B", Start: ts(20), End: ts(30)}}
	instr.mocks = []*models.Mock{
		mockAt("mock-0", ts(10).Add(3*time.Millisecond)),
		mockAt("mock-1", ts(15)),
		mockAt("mock-2", ts(20).Add(-2*time.Millisecond)),
	}
	require.NoError(t, recordSetLogging(t, zap.New(core), instr, mapdb.New(zap.NewNop(), t.TempDir(), ""), nil))

	warns := logs.FilterMessageSnippet(outputTimingWarn).All()
	require.Len(t, warns, 1)
	require.Contains(t, warns[0].Message, "2 mocks fell within 20 ms of a boundary")
	require.Contains(t, warns[0].Message, "go test -json")
}

const jsonSequential = `{"Time":"2026-09-24T10:00:00Z","Action":"run","Package":"orders/e2e","Test":"TestA"}
{"Time":"2026-09-24T10:00:00.01Z","Action":"pass","Package":"orders/e2e","Test":"TestA"}
{"Time":"2026-09-24T10:00:00.01Z","Action":"run","Package":"orders/e2e","Test":"TestC"}
{"Time":"2026-09-24T10:00:00.02Z","Action":"pass","Package":"orders/e2e","Test":"TestC"}
`

func TestRecordDoesNotWarnWhenTheRunnerGaveItsOwnTime(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	instr := newRunnerInstr(t, jsonSequential)
	instr.windows = []models.ScopeWindow{{Name: "orders/e2e.TestA", Start: ts(10), End: ts(20)}}
	instr.mocks = []*models.Mock{mockAt("mock-0", ts(10).Add(time.Millisecond))}
	require.NoError(t, recordSetLogging(t, zap.New(core), instr, mapdb.New(zap.NewNop(), t.TempDir(), ""), nil))
	require.Empty(t, logs.FilterMessageSnippet(outputTimingWarn).All())
}

func TestRecordReadsTestBoundariesFromRunnerOutput(t *testing.T) {
	instr := newRunnerInstr(t, plainSequential)
	require.NoError(t, recordSet(t, instr, mapdb.New(zap.NewNop(), t.TempDir(), ""), nil))
	marks, observed := instr.seen()
	require.True(t, observed)
	require.Empty(t, marks, "boundaries read from output are never posted to the agent")
}

const plainParallel = `=== RUN   TestA
=== PAUSE TestA
=== RUN   TestB
=== PAUSE TestB
=== CONT  TestA
=== CONT  TestB
--- PASS: TestA (0.00s)
--- PASS: TestB (0.00s)
PASS
`

func TestRecordFailsWhenTestsOverlap(t *testing.T) {
	instr := newRunnerInstr(t, plainParallel)
	err := recordSet(t, instr, mapdb.New(zap.NewNop(), t.TempDir(), ""), nil)
	require.ErrorContains(t, err, "TestA and TestB")
	require.ErrorContains(t, err, "--allow-parallel-tests")
	require.Equal(t, 0, utils.ErrCode, "the runner passed; the failure is keploy's own and reaches the exit code as the returned error")
}

func TestRecordFailingOnOverlapStillMirrorsTheRunner(t *testing.T) {
	instr := newRunnerInstr(t, plainParallel)
	instr.runResult = models.AppError{AppErrorType: models.ErrUnExpected, ExitCode: 3}
	require.Error(t, recordSet(t, instr, mapdb.New(zap.NewNop(), t.TempDir(), ""), nil))
	require.Equal(t, 3, utils.ErrCode)
}

func TestRecordAllowsOverlapWhenAsked(t *testing.T) {
	instr := newRunnerInstr(t, plainParallel)
	require.NoError(t, recordSet(t, instr, mapdb.New(zap.NewNop(), t.TempDir(), ""), func(cfg *config.Config) {
		cfg.Mock.AllowParallelTests = true
	}))
}

func TestNoRunnerScopeLeavesTheOutputAlone(t *testing.T) {
	instr := newRunnerInstr(t, plainParallel)
	require.NoError(t, recordSet(t, instr, mapdb.New(zap.NewNop(), t.TempDir(), ""), func(cfg *config.Config) {
		cfg.Mock.NoRunnerScope = true
	}))
	marks, observed := instr.seen()
	require.False(t, observed)
	require.Empty(t, marks)
}

func TestNoMappingDBMeansNoRunnerScope(t *testing.T) {
	instr := newRunnerInstr(t, plainParallel)
	require.NoError(t, recordSet(t, instr, nil, nil))
	_, observed := instr.seen()
	require.False(t, observed)
}

// mappedNames is the mapping on disk as test name -> mock names.
func mappedNames(t *testing.T, mapDB *mapdb.MappingDb) map[string][]string {
	t.Helper()
	got, _, err := mapDB.Get(context.Background(), "set")
	require.NoError(t, err)
	out := make(map[string][]string, len(got))
	for test, entries := range got {
		for _, e := range entries {
			out[test] = append(out[test], e.Name)
		}
	}
	return out
}

// The published agent ignores the runner's time and stamps its own; the CLI's windows must not depend on it.
func TestRecordMapsFromTheAdapterWindowsWhenTheAgentHasNone(t *testing.T) {
	instr := newRunnerInstr(t, jsonSequential)
	instr.mocks = []*models.Mock{
		mockAt("mock-0", runnerT0.Add(5*time.Millisecond)),
		mockAt("mock-1", runnerT0.Add(15*time.Millisecond)),
	}
	mapDB := mapdb.New(zap.NewNop(), t.TempDir(), "")
	require.NoError(t, recordSet(t, instr, mapDB, nil))
	require.Equal(t, map[string][]string{
		"orders/e2e.TestA": {"mock-0"},
		"orders/e2e.TestC": {"mock-1"},
	}, mappedNames(t, mapDB))
}

// A suite that also calls the scope API itself keeps those windows.
func TestRecordKeepsAgentWindowsForTestsTheAdapterDidNotSee(t *testing.T) {
	instr := newRunnerInstr(t, jsonSequential)
	instr.windows = []models.ScopeWindow{{Name: "fixture.Setup", Start: ts(10), End: ts(20)}}
	instr.mocks = []*models.Mock{
		mockAt("mock-0", runnerT0.Add(5*time.Millisecond)),
		mockAt("mock-1", ts(15)),
	}
	mapDB := mapdb.New(zap.NewNop(), t.TempDir(), "")
	require.NoError(t, recordSet(t, instr, mapDB, nil))
	require.Equal(t, map[string][]string{
		"orders/e2e.TestA": {"mock-0"},
		"fixture.Setup":    {"mock-1"},
	}, mappedNames(t, mapDB))
}

// The lab's failure: the agent's windows for the same tests sat one test late; the adapter's must win.
// A test that marked its own start and end is trusted over the window read from the runner's output.
func TestRecordPrefersTheTestsOwnMarksForTheSameTest(t *testing.T) {
	instr := newRunnerInstr(t, jsonSequential)
	instr.windows = []models.ScopeWindow{
		{Name: "orders/e2e.TestA", Start: runnerT0.Add(8 * time.Millisecond), End: runnerT0.Add(18 * time.Millisecond)},
		{Name: "orders/e2e.TestC", Start: runnerT0.Add(18 * time.Millisecond), End: runnerT0.Add(28 * time.Millisecond)},
	}
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(15*time.Millisecond))}
	mapDB := mapdb.New(zap.NewNop(), t.TempDir(), "")
	require.NoError(t, recordSet(t, instr, mapDB, nil))
	require.Equal(t, map[string][]string{"orders/e2e.TestA": {"mock-0"}}, mappedNames(t, mapDB))
}

// A test that made no dependency call is still a flow of the suite; it gets an entry with no mocks so its cases have a home.
func TestRecordListsAFlowWithoutMocks(t *testing.T) {
	instr := newRunnerInstr(t, jsonSequential)
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
	mapDB := mapdb.New(zap.NewNop(), t.TempDir(), "")
	require.NoError(t, recordSet(t, instr, mapDB, nil))

	got, _, err := mapDB.Get(context.Background(), "set")
	require.NoError(t, err)
	require.Equal(t, []string{"mock-0"}, mappedNames(t, mapDB)["orders/e2e.TestA"])
	mocks, listed := got["orders/e2e.TestC"]
	require.True(t, listed, "TestC ran and must be listed even though it made no call")
	require.Empty(t, mocks)
}
