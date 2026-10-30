//go:build linux

package mock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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

// fullDiskInstrumentation is stuckFileInstrumentation's runner, recorded
// into a mock file (its fifo path) on a disk that is full: the file is
// /dev/full, which fails every write with ENOSPC.
type fullDiskInstrumentation struct{ stuckFileInstrumentation }

func (f *fullDiskInstrumentation) GetOutgoing(ctx context.Context, o models.OutgoingOptions) (<-chan *models.Mock, error) {
	if err := os.MkdirAll(filepath.Dir(f.fifo), 0o755); err != nil {
		return nil, err
	}
	if err := os.Symlink("/dev/full", f.fifo); err != nil {
		return nil, err
	}
	return f.trickleInstrumentation.GetOutgoing(ctx, o)
}

// keploy mock record with a real mock store on a disk that is full: the run
// ends as a recording that is NOT saved, said at ERROR with how many of the
// mocks handed to the store the disk did not take and the disk to check; the
// existing set is kept. The gob store says it at its close (closeStore); the
// yaml one, the default, refuses each mock as it is handed it, and Record
// counts them (persist). Before, keploy mock record saved a yaml recording
// short of every mock the disk refused, and exited 0. No second interrupt is
// needed: a disk that fails a write ends the wait on its own. The run's exit
// code is keploy's 1 however it ended: when the user stopped the run, keploy
// mock record's command returns no error (a user interrupt is not a
// failure), so the exit code is utils.ErrCode alone.
func TestRecord_AMockFileOnAFullDiskEndsAsARecordingNotSaved(t *testing.T) {
	_, err := os.Stat("/dev/full")
	require.NoError(t, err, "setup: no /dev/full")
	for _, tc := range []struct {
		name, format, file, handedTo string
		userStoppedIt                bool
	}{
		{"gob/the run ended", "gob", "mocks.gob", "gob mock writer", false},
		{"gob/the user stopped it", "gob", "mocks.gob", "gob mock writer", true},
		{"yaml/the run ended", "yaml", "mocks.yaml", "mock store", false},
		{"yaml/the user stopped it", "yaml", "mocks.yaml", "mock store", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", tc.format)
			utils.ResetInterruptedAgain()
			utils.ClearInterrupted()
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted(); utils.ErrCode = 0 })
			dir := t.TempDir()
			const name, n = "orders", 20 // about 320 KiB: more than the writer's 256 KiB buffer
			existing := filepath.Join(dir, name, "mocks.yaml")
			require.NoError(t, os.MkdirAll(filepath.Dir(existing), 0o755))
			require.NoError(t, os.WriteFile(existing, []byte("# the existing recording\n"), 0o644))
			inst := &fullDiskInstrumentation{stuckFileInstrumentation{
				trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, n)},
				t:                      t, n: n,
				fifo: filepath.Join(dir, name+yaml.MockStagingSuffix, tc.file),
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.userStoppedIt {
				utils.MarkInterrupted() // the stop
				inst.stopped = cancel
			}
			cfg := &config.Config{}
			cfg.Mock.Name = name
			cfg.Path = dir
			core, logs := observer.New(zapcore.InfoLevel)
			db := mockdb.New(zap.New(core), dir, "mocks")
			returned := make(chan error, 1)
			go func() {
				returned <- New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(ctx)
			}()
			err := returnedWithin(t, returned)
			inst.wg.Wait()
			for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
				t.Logf("said at ERROR: %s", e.Message)
			}
			require.ErrorIs(t, err, models.ErrMocksNotWritten, "a recording short of mocks it was handed ended as %v", err)
			require.Equal(t, utils.ExitKeployError, utils.ErrCode, "a disk that failed did not set keploy's exit code")
			got, rerr := os.ReadFile(existing)
			require.NoError(t, rerr)
			require.Equal(t, "# the existing recording\n", string(got), "the existing set was not kept")
			_, serr := os.Lstat(filepath.Dir(inst.fifo))
			require.True(t, os.IsNotExist(serr), "the staging set was left behind: %v", serr)
			failed := logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessage(recordDrainWords.storeNotWritten).All()
			require.Len(t, failed, 1, "the mocks not written were not said")
			fields := failed[0].ContextMap()
			require.Contains(t, fields["error"], fmt.Sprintf("of the %d mocks handed to the %s were not written", n, tc.handedTo))
			require.Contains(t, fields["error"], "no space left on device", "the disk's own error was not said")
			require.Contains(t, fields["next_step"], "check the disk")
		})
	}
}

// failedRunnerOnAFullDisk is fullDiskInstrumentation whose test command
// failed with exit code 3 (a failing test suite).
type failedRunnerOnAFullDisk struct{ fullDiskInstrumentation }

func (f *failedRunnerOnAFullDisk) Run(ctx context.Context, o models.RunOptions) models.AppError {
	f.fullDiskInstrumentation.Run(ctx, o)
	return models.AppError{AppErrorType: models.ErrCommandError, ExitCode: 3, Err: errors.New("exit status 3")}
}

// keploy mock propagates the test command's exit code (main.go's
// finalExitCode): a test command that failed with 3, recorded on a disk that
// failed the store's writes, still exits 3, the recording not saved and the
// existing set kept. keploy's own 1 is for a test command that passed, or
// that the user stopped.
func TestRecord_AFailedTestCommandsExitCodeIsKeptWhenTheStoreFailed(t *testing.T) {
	for _, tc := range []struct{ format, file string }{{"gob", "mocks.gob"}, {"yaml", "mocks.yaml"}} {
		t.Run(tc.format, func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", tc.format)
			utils.ResetInterruptedAgain()
			utils.ClearInterrupted()
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted(); utils.ErrCode = 0 })
			dir := t.TempDir()
			const name, n = "orders", 5
			existing := filepath.Join(dir, name, "mocks.yaml")
			require.NoError(t, os.MkdirAll(filepath.Dir(existing), 0o755))
			require.NoError(t, os.WriteFile(existing, []byte("# the existing recording\n"), 0o644))
			inst := &failedRunnerOnAFullDisk{fullDiskInstrumentation{stuckFileInstrumentation{
				trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, n)},
				t:                      t, n: n,
				fifo: filepath.Join(dir, name+yaml.MockStagingSuffix, tc.file),
			}}}
			cfg := &config.Config{}
			cfg.Mock.Name = name
			cfg.Path = dir
			core, _ := observer.New(zapcore.InfoLevel)
			db := mockdb.New(zap.New(core), dir, "mocks")
			returned := make(chan error, 1)
			go func() {
				returned <- New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background())
			}()
			err := returnedWithin(t, returned)
			inst.wg.Wait()
			require.ErrorIs(t, err, models.ErrMocksNotWritten, "the recording short of mocks ended as %v", err)
			got, rerr := os.ReadFile(existing)
			require.NoError(t, rerr)
			require.Equal(t, "# the existing recording\n", string(got), "the existing set was not kept")
			require.Equal(t, 3, utils.ErrCode, "the failed test command's exit code 3 was not propagated")
		})
	}
}

// A recording the user stopped with a real Ctrl+C (the signal's number is
// known), whose yaml store refused mocks, exits keploy's 1, not 128+N: a
// refused write is the disk's failure, not a give-up of the user's.
func TestRecord_AStoppedRecordingWhoseStoreRefusedMocksExits1WithTheSignalKnown(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "yaml")
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted(); utils.ErrCode = 0 })
	t.Cleanup(utils.SetInterruptSignalForTest(2))
	utils.MarkInterrupted()
	dir := t.TempDir()
	const name, n = "orders", 5
	existing := filepath.Join(dir, name, "mocks.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(existing), 0o755))
	require.NoError(t, os.WriteFile(existing, []byte("# the existing recording\n"), 0o644))
	inst := &fullDiskInstrumentation{stuckFileInstrumentation{
		trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, n)},
		t:                      t, n: n,
		fifo: filepath.Join(dir, name+yaml.MockStagingSuffix, "mocks.yaml"),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inst.stopped = cancel
	cfg := &config.Config{}
	cfg.Mock.Name = name
	cfg.Path = dir
	core, _ := observer.New(zapcore.InfoLevel)
	db := mockdb.New(zap.New(core), dir, "mocks")
	returned := make(chan error, 1)
	go func() {
		returned <- New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(ctx)
	}()
	err := returnedWithin(t, returned)
	inst.wg.Wait()
	require.ErrorIs(t, err, models.ErrMocksNotWritten)
	require.Equal(t, utils.ExitKeployError, utils.ErrCode, "a refused write armed the give-up code")
	got, rerr := os.ReadFile(existing)
	require.NoError(t, rerr)
	require.Equal(t, "# the existing recording\n", string(got), "the existing set was not kept")
}
