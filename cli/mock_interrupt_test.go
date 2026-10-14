package cli

import (
	"context"
	"errors"
	"testing"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// stoppedReplay is a mock service whose replay the user's Ctrl+C stops after
// the test command passed, while it reads what the run did: NewCtx's handler
// records SIGINT and cancels the root context, and the read fails with it.
type stoppedReplay struct{ cancel context.CancelFunc }

func (stoppedReplay) Record(context.Context) error { return nil }

func (s stoppedReplay) Replay(ctx context.Context) error {
	utils.MarkInterrupted()
	s.cancel()
	<-ctx.Done()
	return errors.New("keploy could not read what each test did from its agent: context canceled")
}

// A replay the user stopped is no pass: `keploy mock replay` exits 128+N for
// the signal, whatever its service returned. It dropped the error it got
// with the context cancelled and armed nothing, so a run keploy never judged
// exited with the test command's 0.
func TestMockReplayTheUserStoppedExits128PlusN(t *testing.T) {
	utils.ClearInterrupted()
	t.Cleanup(utils.ClearInterrupted)
	defer utils.SetInterruptSignalForTest(2)() // SIGINT
	saved := utils.ErrCode
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = saved })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := MockReplay(ctx, zap.NewNop(), agentSvcFactory{svc: stoppedReplay{cancel: cancel}}, noFlags{})
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE returned %v: a user interrupt is not keploy's failure", err)
	}
	if utils.ErrCode != 130 {
		t.Fatalf("a replay the user stopped (SIGINT) exits %d, want 130", utils.ErrCode)
	}
}
