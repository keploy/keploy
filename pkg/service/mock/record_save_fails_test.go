package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// promoteFailsDB is the real mock store, whose promote of the staging set
// over the named set fails, as a rename the filesystem refuses would.
type promoteFailsDB struct{ *mockdb.MockYaml }

func (promoteFailsDB) PromoteStagedSet(context.Context, string, string) error {
	return errors.New("rename the staging set: permission denied")
}

// stoppedRunner is a runner that made n calls while it ran; the user's
// Ctrl+C (stop) came as it exited.
type stoppedRunner struct {
	trickleInstrumentation
	n    int
	stop context.CancelFunc
}

func (s *stoppedRunner) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	return models.AgentBacklog{Verdict: models.BacklogClear}, nil
}

func (s *stoppedRunner) Run(context.Context, models.RunOptions) models.AppError {
	for i := 0; i < s.n; i++ {
		s.out <- &models.Mock{Version: models.GetVersion(), Kind: models.HTTP, Name: fmt.Sprintf("mock-%d", i), Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://x/y"},
			HTTPResp: &models.HTTPResp{StatusCode: 200},
		}}
	}
	s.stop()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-s.captureCtx.Done() // a live agent keeps its stream until the client ends it
		close(s.out)
	}()
	return models.AppError{}
}

// keploy mock record stopped by the user (Ctrl+C) once it had captured
// mocks, whose save of them (the staging set's promote) then fails: the
// recording is not saved, which the run says at ERROR, the existing set is
// kept, and the run fails with keploy's exit code. keploy mock record's
// command does not fail a run the user stopped for its error, so the exit
// code is utils.ErrCode alone. Before, Record returned nil with the exit code
// left at 0: the command exited 0 as if the recording had been saved.
func TestRecord_AStoppedRecordingThatCouldNotBeSavedFailsTheRun(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "yaml")
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted(); utils.ErrCode = 0 })
	utils.MarkInterrupted() // the stop
	dir := t.TempDir()
	const name, n = "orders", 3
	existing := filepath.Join(dir, name, "mocks.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(existing), 0o755))
	require.NoError(t, os.WriteFile(existing, []byte("# the existing recording\n"), 0o644))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inst := &stoppedRunner{trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, n)}, n: n, stop: cancel}
	cfg := &config.Config{}
	cfg.Mock.Name = name
	cfg.Path = dir
	core, logs := observer.New(zapcore.InfoLevel)
	db := promoteFailsDB{mockdb.New(zap.New(core), dir, "mocks")}
	returned := make(chan error, 1)
	go func() {
		returned <- New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(ctx)
	}()
	err := returnedWithin(t, returned)
	inst.wg.Wait()
	for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		t.Logf("said at ERROR: %s", e.Message)
	}
	require.Equal(t, utils.ExitKeployError, utils.ErrCode, "a recording that was not saved left the exit code at %d (Record returned %v)", utils.ErrCode, err)
	require.ErrorContains(t, err, "permission denied", "a recording that was not saved ended as %v", err)
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessage("recording stopped, but saving the captured mocks failed").Len(),
		"the save's failure was not said: %v", logs.All())
	got, rerr := os.ReadFile(existing)
	require.NoError(t, rerr)
	require.Equal(t, "# the existing recording\n", string(got), "the existing set was not kept")
	_, serr := os.Stat(filepath.Join(dir, name+yaml.MockStagingSuffix))
	require.True(t, os.IsNotExist(serr), "the staging set was left behind: %v", serr)
}

// refusesAndFailsCloseDB is the real (yaml) mock store, which refuses one
// mock (mock-1) as a full disk would, and whose close then fails too.
type refusesAndFailsCloseDB struct{ *mockdb.MockYaml }

func (d refusesAndFailsCloseDB) InsertMock(ctx context.Context, m *models.Mock, set string) error {
	if m.Name == "mock-1" {
		return errors.New("write mocks.yaml: no space left on device")
	}
	return d.MockYaml.InsertMock(ctx, m, set)
}

func (refusesAndFailsCloseDB) CloseUnless(<-chan struct{}) error {
	return fmt.Errorf("%w: the mock file failed as it was closed: input/output error", models.ErrMocksNotWritten)
}

// A store that refused a mock as it was handed it, and whose close then
// failed too, says both: how many of the mocks it was handed it refused, and
// its close's own error. The recording is not saved, the existing set is
// kept, and the run fails with keploy's exit code.
func TestRecord_AStoreThatRefusedAMockAndFailedItsCloseSaysBoth(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "yaml")
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted(); utils.ErrCode = 0 })
	dir := t.TempDir()
	const name, n = "orders", 3
	existing := filepath.Join(dir, name, "mocks.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(existing), 0o755))
	require.NoError(t, os.WriteFile(existing, []byte("# the existing recording\n"), 0o644))
	inst := &stoppedRunner{trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, n)}, n: n, stop: func() {}}
	cfg := &config.Config{}
	cfg.Mock.Name = name
	cfg.Path = dir
	core, logs := observer.New(zapcore.InfoLevel)
	db := refusesAndFailsCloseDB{mockdb.New(zap.New(core), dir, "mocks")}
	returned := make(chan error, 1)
	go func() {
		returned <- New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background())
	}()
	err := returnedWithin(t, returned)
	inst.wg.Wait()
	require.ErrorIs(t, err, models.ErrMocksNotWritten, "a recording short of a mock its store refused ended as %v", err)
	require.ErrorContains(t, err, "the mock file failed as it was closed", "the store's close error was not said")
	require.ErrorContains(t, err, fmt.Sprintf("1 of the %d mocks handed to the mock store were not written", n), "the mock the store refused was not said")
	require.ErrorContains(t, err, "no space left on device", "the refused write's own error was not said")
	require.Equal(t, utils.ExitKeployError, utils.ErrCode, "a recording that was not saved left the exit code at %d", utils.ErrCode)
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessage(recordDrainWords.storeNotWritten).Len(), "the recording not saved was not said: %v", logs.All())
	got, rerr := os.ReadFile(existing)
	require.NoError(t, rerr)
	require.Equal(t, "# the existing recording\n", string(got), "the existing set was not kept")
	_, serr := os.Stat(filepath.Join(dir, name+yaml.MockStagingSuffix))
	require.True(t, os.IsNotExist(serr), "the staging set was left behind: %v", serr)
}

// encodeRefusesDB is the real yaml store, which cannot encode mock-1 (a
// payload fault: models.ErrMockEncode).
type encodeRefusesDB struct{ *mockdb.MockYaml }

func (d encodeRefusesDB) InsertMock(ctx context.Context, m *models.Mock, set string) error {
	if m.Name == "mock-1" {
		return fmt.Errorf("%w (yaml): a value yaml cannot marshal", models.ErrMockEncode)
	}
	return d.MockYaml.InsertMock(ctx, m, set)
}

// A mock the store cannot encode (a payload yaml cannot marshal) is not one
// the disk refused: it is dropped with a WARN, as before, and the rest of
// the recording is saved, exit 0. Counted as refused, one such mock would
// have every recording that holds one end as not saved.
func TestRecord_AnUnencodableMockIsDroppedAndTheRestSaved(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "yaml")
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted(); utils.ErrCode = 0 })
	dir := t.TempDir()
	const name, n = "orders", 3
	existing := filepath.Join(dir, name, "mocks.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(existing), 0o755))
	require.NoError(t, os.WriteFile(existing, []byte("# the existing recording\n"), 0o644))
	inst := &stoppedRunner{trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, n)}, n: n, stop: func() {}}
	cfg := &config.Config{}
	cfg.Mock.Name = name
	cfg.Path = dir
	core, logs := observer.New(zapcore.InfoLevel)
	db := encodeRefusesDB{mockdb.New(zap.New(core), dir, "mocks")}
	returned := make(chan error, 1)
	go func() {
		returned <- New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background())
	}()
	err := returnedWithin(t, returned)
	inst.wg.Wait()
	require.NoError(t, err, "a recording with one unencodable mock failed: %v", logs.All())
	require.Equal(t, 0, utils.ErrCode)
	got, rerr := os.ReadFile(existing)
	require.NoError(t, rerr)
	require.NotEqual(t, "# the existing recording\n", string(got), "the recording was not saved")
	require.Equal(t, 2, strings.Count(string(got), "kind: Http"), "the saved set: %s", got)
	require.Equal(t, 1, logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("unencodable").Len(), "the dropped mock was not said at WARN")
}
