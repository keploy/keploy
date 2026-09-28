package mock

import (
	"context"
	"errors"
	"fmt"
	"go.keploy.io/server/v3/utils"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestRecordOverwriteDeletesMappingsBeforeRunnerStarts(t *testing.T) {
	var order []string
	mappingDB := &recordMappingDB{order: &order}
	instr := &recordInstrumentation{order: &order}
	mockDB := &recordMockDB{order: &order}
	store := &recordStore{order: &order}
	cfg := config.New()
	cfg.Command = "go test ./..."
	cfg.Mock.Name = "stale-map-demo"

	svc := &mockService{
		logger:          zap.NewNop(),
		instrumentation: instr,
		mockDB:          mockDB,
		mappingDB:       mappingDB,
		store:           store,
		config:          cfg,
	}

	require.NoError(t, svc.Record(context.Background()))
	require.True(t, mockDB.deleted)
	require.True(t, mappingDB.deleted)
	require.Equal(t, []string{"setup", "delete-mocks", "delete-mappings", "reset-counter", "get-outgoing", "run", "push", "notify-shutdown"}, order)
}

type recordInstrumentation struct {
	order *[]string
}

func (i *recordInstrumentation) Setup(context.Context, string, models.SetupOptions) error {
	*i.order = append(*i.order, "setup")
	return nil
}

func (i *recordInstrumentation) GetOutgoing(context.Context, models.OutgoingOptions) (<-chan *models.Mock, error) {
	*i.order = append(*i.order, "get-outgoing")
	ch := make(chan *models.Mock)
	close(ch)
	return ch, nil
}

func (i *recordInstrumentation) MockOutgoing(context.Context, models.OutgoingOptions) error {
	return nil
}
func (i *recordInstrumentation) StoreMocks(context.Context, []*models.Mock, []*models.Mock) error {
	return nil
}
func (i *recordInstrumentation) UpdateMockParams(context.Context, models.MockFilterParams) error {
	return nil
}
func (i *recordInstrumentation) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return nil, nil
}
func (i *recordInstrumentation) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return nil, nil
}

func (i *recordInstrumentation) Run(context.Context, models.RunOptions) models.AppError {
	*i.order = append(*i.order, "run")
	return models.AppError{AppErrorType: models.ErrAppStopped}
}

func (i *recordInstrumentation) MakeAgentReadyForDockerCompose(context.Context) error { return nil }
func (i *recordInstrumentation) NotifyGracefulShutdown(context.Context) error {
	*i.order = append(*i.order, "notify-shutdown")
	return nil
}

type recordMockDB struct {
	order   *[]string
	deleted bool
}

func (db *recordMockDB) InsertMock(context.Context, *models.Mock, string) error { return nil }
func (db *recordMockDB) DeleteMocksForSet(context.Context, string) error {
	db.deleted = true
	*db.order = append(*db.order, "delete-mocks")
	return nil
}
func (db *recordMockDB) GetFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return nil, nil
}
func (db *recordMockDB) GetUnFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return nil, nil
}
func (db *recordMockDB) ResetCounterID()    { *db.order = append(*db.order, "reset-counter") }
func (db *recordMockDB) SetCounterID(int64) {}

type recordMappingDB struct {
	order   *[]string
	deleted bool
}

func (db *recordMappingDB) DeleteMappingsForSet(context.Context, string) error {
	db.deleted = true
	*db.order = append(*db.order, "delete-mappings")
	return nil
}
func (db *recordMappingDB) UpsertBatch(context.Context, string, map[string][]models.MockEntry) error {
	return nil
}
func (db *recordMappingDB) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	return nil, false, nil
}

type recordStore struct {
	order *[]string
}

func (recordStore) Pull(context.Context, string) error { return nil }
func (s recordStore) Push(context.Context, string) error {
	*s.order = append(*s.order, "push")
	return nil
}

// failsRecord is failsGroup for the stages only a recording has: a dying
// agent fails a goroutine in the run's errgroup, and the stage in flight
// fails with it.
type failsRecord struct {
	*failsGroup
}

func (f failsRecord) GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error) {
	if f.stage == "capture" {
		killGroup(ctx)
		return nil, errors.New("could not start capturing outgoing calls")
	}
	return f.composeInstr.GetOutgoing(ctx, opts)
}

func (f failsRecord) MakeAgentReadyForDockerCompose(ctx context.Context) error {
	if f.stage == "release" {
		killGroup(ctx)
		return errors.New("could not release the app")
	}
	return f.composeInstr.MakeAgentReadyForDockerCompose(ctx)
}

// recordWith runs one recording against instr and returns its error.
func recordWith(t *testing.T, ctx context.Context, instr Instrumentation, base *composeInstr, cmdType utils.CmdType) error {
	t.Helper()
	cfg := instrConfig(base, cmdType, "go test ./...")
	cfg.Path = t.TempDir()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	return New(zap.NewNop(), instr, stubMockDB{}, nil, nil, nil, cfg).Record(ctx)
}

// keploy's own failure is not the user pressing Ctrl+C. The errgroup cancels
// its context whenever anything in it fails -- the agent process exiting is
// one of those -- so testing that context turned a native agent that could
// not start into `return nil`: exit 0, the test command never run, and the CI
// step the VS Code extension generates went green. Every stage guards itself,
// so every stage is failed.
func TestARecordingKeployFailedIsNotAnInterrupt(t *testing.T) {
	for _, tc := range []struct {
		stage    string
		lifetime agentLifetime
		cmdType  utils.CmdType
	}{
		{"setup", agentUpFromSetup, utils.Native},
		{"compose", agentNeverUp, utils.DockerCompose},
		{"capture", agentUpFromSetup, utils.Native},
		{"release", agentUpAfterRun, utils.DockerCompose},
		{"run", agentUpFromSetup, utils.Native},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			shortAgentBudget(t)
			base := newInstr(t, tc.lifetime, tc.cmdType == utils.DockerCompose, models.AppError{AppErrorType: models.ErrAppStopped})
			instr := failsRecord{&failsGroup{composeInstr: base, stage: tc.stage}}
			if err := recordWith(t, context.Background(), instr, base, tc.cmdType); err == nil {
				t.Fatalf("a recording that failed at %s reported success", tc.stage)
			}
		})
	}
}

// Why it failed has to survive to the exit: the agent's reason is what
// decides the exit code (utils.ExitCodeFor) and the message a tool shows, and
// the recording used to replace it with its own bare stop reason.
func TestARecordingThatCouldNotStartSaysWhy(t *testing.T) {
	why := fmt.Errorf("%w: the keploy agent could not start (exit status 3)", utils.ErrPrivilegeRequired)
	base := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
	err := recordWith(t, context.Background(), &runWrites{composeInstr: base, setupErr: why}, base, utils.Native)
	if !errors.Is(err, utils.ErrPrivilegeRequired) {
		t.Fatalf("Record returned %v; the agent's reason is gone", err)
	}
}

// The user's Ctrl+C is still not a failure, wherever it lands.
func TestAnInterruptedRecordingIsNotAFailure(t *testing.T) {
	for _, stage := range []string{"setup", "run"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			base := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrCtxCanceled})
			var instr Instrumentation = &runWrites{composeInstr: base, setupErr: context.Canceled}
			if stage == "run" {
				instr = &runWrites{composeInstr: base, onRun: cancel}
			} else {
				cancel()
			}
			defer cancel()
			if err := recordWith(t, ctx, instr, base, utils.Native); err != nil {
				t.Fatalf("an interrupted recording failed: %v", err)
			}
		})
	}
}
