package mock

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// runnerInstr is a native instrumentation whose wrapped command prints output,
// whose proxy captures mocks, and whose agent answers the scope API.
type runnerInstr struct {
	*composeInstr
	output  string
	mocks   []*models.Mock
	windows []models.ScopeWindow

	mu       sync.Mutex
	marks    []string
	observed bool
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
	return r.windows, nil
}

func (r *runnerInstr) BeginScope(_ context.Context, name string, _ int) error {
	return r.mark("begin " + name)
}

func (r *runnerInstr) EndScope(_ context.Context, name string, _ int) error {
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
	cfg := instrConfig(instr.composeInstr, utils.Native, "./shop.test -test.v")
	cfg.Path = t.TempDir()
	if tweak != nil {
		tweak(cfg)
	}
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	return New(zap.NewNop(), instr, stubMockDB{}, mapDB, nil, nil, cfg).Record(context.Background())
}

func TestRecordReadsTestBoundariesFromRunnerOutput(t *testing.T) {
	instr := newRunnerInstr(t, plainSequential)
	require.NoError(t, recordSet(t, instr, mapdb.New(zap.NewNop(), t.TempDir(), ""), nil))
	marks, observed := instr.seen()
	require.True(t, observed)
	require.Equal(t, []string{
		"begin TestA", "begin TestA/one", "begin TestA/two",
		"end TestA", "end TestA/one", "end TestA/two",
		"begin TestC", "end TestC",
	}, marks)
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
