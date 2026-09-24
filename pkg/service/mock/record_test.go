package mock

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

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
