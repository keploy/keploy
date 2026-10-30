//go:build !windows

package mock

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// stuckFileInstrumentation is a runner that made n calls of about 16 KiB
// while it ran, recorded into a mock file that is a FIFO nothing reads: the
// kernel takes what fits in the pipe, and then the write does not return (a
// disk that is stuck). The FIFO is made once the record has cleared its
// staging set, before the first mock reaches the store. stopped, when set,
// is the user's Ctrl+C as the runner exits.
type stuckFileInstrumentation struct {
	trickleInstrumentation
	t    *testing.T
	fifo string
	n    int
	// tiny more mocks of a few hundred bytes follow the n.
	tiny    int
	stopped context.CancelFunc
}

func (s *stuckFileInstrumentation) GetOutgoing(ctx context.Context, o models.OutgoingOptions) (<-chan *models.Mock, error) {
	if err := os.MkdirAll(filepath.Dir(s.fifo), 0o755); err != nil {
		return nil, err
	}
	mkfifo(s.t, s.fifo)
	// The read end, opened and never read, lets the writer's open return.
	r, err := os.OpenFile(s.fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	// The test's end: the writer's write then fails, and it ends.
	s.t.Cleanup(func() { _ = r.Close() })
	return s.trickleInstrumentation.GetOutgoing(ctx, o)
}

func (s *stuckFileInstrumentation) BacklogBefore(context.Context, time.Time) (models.AgentBacklog, error) {
	return models.AgentBacklog{Verdict: models.BacklogClear}, nil
}

func (s *stuckFileInstrumentation) Run(context.Context, models.RunOptions) models.AppError {
	body := strings.Repeat("x", 8<<10)
	for i := 0; i < s.n; i++ {
		s.out <- &models.Mock{Version: models.GetVersion(), Kind: models.HTTP, Name: fmt.Sprintf("mock-%d", i), Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://x/y", Body: body},
			HTTPResp: &models.HTTPResp{StatusCode: 200, Body: body},
		}}
	}
	for i := 0; i < s.tiny; i++ {
		s.out <- &models.Mock{Version: models.GetVersion(), Kind: models.HTTP, Name: fmt.Sprintf("tiny-%d", i), Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Method: "GET", URL: "http://x/y"},
			HTTPResp: &models.HTTPResp{StatusCode: 200},
		}}
	}
	if s.stopped != nil {
		s.stopped()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		<-s.captureCtx.Done() // a live agent keeps its stream until the client ends it
		close(s.out)
	}()
	return models.AppError{}
}

// keploy mock record with a real gob store on a disk that is stuck (a write
// that never returns): once its consumers have ended, it closes the store,
// says while it waits how many mocks are left and that a second interrupt
// stops it now, and at that interrupt ends as a recording that is NOT saved,
// said at ERROR with how many mocks were not written; the existing set is
// kept. It must end so whether the run ended on its own or the user stopped
// it. Before, the staging set's promote (or, at the user's give-up, its
// discard) closed the store with no stop and nothing said: the run hung, and
// no interrupt could end it.
func TestRecord_AStuckMockFileEndsAtTheUsersInterruptAsARecordingNotSaved(t *testing.T) {
	for _, tc := range []struct {
		name          string
		userStoppedIt bool
	}{{"the run ended", false}, {"the user stopped it", true}} {
		t.Run(tc.name, func(t *testing.T) {
			defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
			mockDrainNotice = 20 * time.Millisecond
			t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
			utils.ResetInterruptedAgain()
			utils.ClearInterrupted()
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted(); utils.ErrCode = 0 })
			utils.MarkInterrupted() // the stop
			dir := t.TempDir()
			const name, n = "orders", 20
			existing := filepath.Join(dir, name, "mocks.yaml")
			require.NoError(t, os.MkdirAll(filepath.Dir(existing), 0o755))
			require.NoError(t, os.WriteFile(existing, []byte("# the existing recording\n"), 0o644))
			inst := &stuckFileInstrumentation{
				trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, n)},
				t:                      t, n: n,
				fifo: filepath.Join(dir, name+yaml.MockStagingSuffix, "mocks.gob"),
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.userStoppedIt {
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
			offer := recordDrainWords.storeWriting + "; " + recordDrainWords.stopNow
			if err, early := untilSaid(t, logs, offer, returned); early {
				t.Fatalf("Record returned (%v) while the mock file's write had not returned", err)
			}
			said := logs.FilterMessageSnippet(recordDrainWords.storeWriting).All()[0].ContextMap()
			if left, _ := said["mocksLeft"].(uint64); left == 0 || left > n {
				t.Fatalf("the notice did not say how many mocks are left: %v", said)
			}
			if !utils.MarkInterruptedAgain() { // the user, as told
				t.Fatal("setup: no offer standing")
			}
			err := returnedWithin(t, returned)
			require.ErrorIs(t, err, models.ErrMocksNotWritten, "a recording short of mocks it was handed ended as %v", err)
			failed := logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessage(recordDrainWords.storeNotWritten).All()
			require.Len(t, failed, 1, "the mocks not written were not said: %v", logs.All())
			fields := failed[0].ContextMap()
			require.Contains(t, fields["error"], fmt.Sprintf("of the %d mocks handed to the gob mock writer were not written", n))
			require.Equal(t, recordDrainWords.nextStep, fields["next_step"])
			got, rerr := os.ReadFile(existing)
			require.NoError(t, rerr)
			require.Equal(t, "# the existing recording\n", string(got), "the existing set was not kept")
			_, serr := os.Stat(filepath.Dir(inst.fifo))
			require.True(t, os.IsNotExist(serr), "the staging set was left behind: %v", serr)
			inst.wg.Wait()
		})
	}
}

// The same stuck mock file, with more mocks than the store's queue holds:
// the consumer's insert waits for room in it, so the run waits for it (the
// drain, then waitConsumers), and offers to stop now. A user who presses
// Ctrl+C at every offer ends the run as a recording that is NOT saved, said
// at ERROR, and the discard of the staging set does not then wait for the
// store the user gave up on.
func TestRecord_AStuckMockFileWithAFullQueueEndsAtTheUsersInterrupt(t *testing.T) {
	defer func(d time.Duration) { mockDrainNotice = d }(mockDrainNotice)
	mockDrainNotice = 20 * time.Millisecond
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	utils.ResetInterruptedAgain()
	utils.ClearInterrupted()
	t.Cleanup(func() { utils.ResetInterruptedAgain(); utils.ClearInterrupted() })
	utils.MarkInterrupted() // the stop
	dir := t.TempDir()
	const name = "orders"
	existing := filepath.Join(dir, name, "mocks.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(existing), 0o755))
	require.NoError(t, os.WriteFile(existing, []byte("# the existing recording\n"), 0o644))
	inst := &stuckFileInstrumentation{
		trickleInstrumentation: trickleInstrumentation{out: make(chan *models.Mock, 4300)},
		t:                      t, n: 20, tiny: 4200, // the queue holds 4096
		fifo: filepath.Join(dir, name+yaml.MockStagingSuffix, "mocks.gob"),
	}
	cfg := &config.Config{}
	cfg.Mock.Name = name
	cfg.Path = dir
	core, logs := observer.New(zapcore.InfoLevel)
	db := mockdb.New(zap.New(core), dir, "mocks")
	returned := make(chan error, 1)
	go func() {
		returned <- New(zap.New(core), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background())
	}()
	if err, early := untilSaid(t, logs, "press Ctrl+C again", returned); early {
		t.Fatalf("Record returned (%v) while its consumer waited for room in the store's queue", err)
	}
	var err error
	guard := time.After(time.Minute) // only turns a hang into a failure
	for ended := false; !ended; {
		utils.MarkInterruptedAgain() // the user, at every offer
		select {
		case err = <-returned:
			ended = true
		case <-guard:
			t.Fatalf("Record did not end at the user's interrupts: %v", logs.All())
		case <-time.After(5 * time.Millisecond):
		}
	}
	for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		t.Logf("said at ERROR: %s", e.Message)
	}
	require.Error(t, err, "a recording short of mocks it was handed ended as a success")
	require.NotZero(t, logs.FilterLevelExact(zapcore.ErrorLevel).Len(), "the mocks not written were not said: %v", logs.All())
	got, rerr := os.ReadFile(existing)
	require.NoError(t, rerr)
	require.Equal(t, "# the existing recording\n", string(got), "the existing set was not kept")
	_, serr := os.Stat(filepath.Dir(inst.fifo))
	require.True(t, os.IsNotExist(serr), "the staging set was left behind: %v", serr)
	inst.wg.Wait()
}
