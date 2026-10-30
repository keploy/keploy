//go:build linux

package mock

import (
	"context"
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

// keploy mock record with a real gob store on a disk that is full: the
// store's close (closeStore) returns how many of the mocks it was handed the
// disk did not take, so the run ends as a recording that is NOT saved, said
// at ERROR with that count and the disk to check; the existing set is kept.
// No second interrupt is needed: a disk that fails a write ends the wait on
// its own. The run's exit code is keploy's 1 however it ended: when the user
// stopped the run, keploy mock record's command returns no error (a user
// interrupt is not a failure), so the exit code is utils.ErrCode alone.
func TestRecord_AMockFileOnAFullDiskEndsAsARecordingNotSaved(t *testing.T) {
	_, err := os.Stat("/dev/full")
	require.NoError(t, err, "setup: no /dev/full")
	for _, tc := range []struct {
		name          string
		userStoppedIt bool
	}{{"the run ended", false}, {"the user stopped it", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
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
				fifo: filepath.Join(dir, name+yaml.MockStagingSuffix, "mocks.gob"),
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
			require.Contains(t, fields["error"], fmt.Sprintf("of the %d mocks handed to the gob mock writer were not written", n))
			require.Contains(t, fields["error"], "no space left on device", "the disk's own error was not said")
			require.Contains(t, fields["next_step"], "check the disk")
		})
	}
}
