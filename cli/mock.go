package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	mockSvc "go.keploy.io/server/v3/pkg/service/mock"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("mock", Mock)
}

// Mock is the `keploy mock` parent command: use Keploy as a framework-agnostic
// mocking layer for your OWN test runner (pytest, go test, jest/playwright,
// mobile UI tests). Record captures the outgoing dependency calls your tests
// make; replay serves them back so the suite runs hermetically.
//
// Enterprise re-registers "mock" to add cloud-registry subcommands
// (download/upload/patch); it composes these OSS record/replay builders under
// its own parent, so keep MockRecord/MockReplay exported and free of AddFlags
// calls (the parent's loop adds flags — double registration panics).
func Mock(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	long := `Record the real outgoing calls your test suite makes, then replay them so the
suite runs without the real dependencies — a language-agnostic VCR/WireMock at
the network layer.

Examples:
  # Record the dependency calls pytest makes into the "default" mock set
  keploy mock record -c "pytest"

  # Replay them (dependencies can be offline)
  keploy mock replay -c "pytest"

  # A named set, refreshed on a merge to main, appending any new calls
  keploy mock record -c "go test ./..." --name orders
  keploy mock replay -c "go test ./..." --name orders --on-miss record`
	if provenswitch.On() {
		// Only what the proven model runs: a Go suite, natively, and a
		// replay of this build with KEPLOY_PROVEN=0, as its refusal says.
		long = `Record the real outgoing calls your test suite makes, then replay them so the
suite runs without the real dependencies, at the network layer.

Under the proven model (` + provenswitch.Env + `=1 in this build; keploy enterprise runs
it by default on Linux), keploy judges each test of a Go suite. Replaying
under it needs a keploy build that judges each test (keploy enterprise); this
build replays with ` + provenswitch.Env + `=0, without the proven model.

Examples:
  # Record the dependency calls go test makes into the "default" mock set
  keploy mock record -c "go test -p 1 -json ./..."

  # Replay them without the proven model (dependencies can be offline)
  ` + provenswitch.Env + `=0 keploy mock replay -c "go test -p 1 -json ./..."

  # A named set
  keploy mock record -c "go test -p 1 -json ./..." --name orders`
	}
	cmd := &cobra.Command{
		Use:   "mock",
		Short: "Use Keploy as a mocking framework for your own tests (record/replay dependency calls)",
		Long:  long,
	}

	cmd.AddCommand(MockRecord(ctx, logger, serviceFactory, cmdConfigurator))
	cmd.AddCommand(MockReplay(ctx, logger, serviceFactory, cmdConfigurator))
	for _, subCmd := range cmd.Commands() {
		if err := cmdConfigurator.AddFlags(subCmd); err != nil {
			utils.LogError(logger, err, "failed to add flags to command", zap.String("command", subCmd.Name()))
		}
	}
	return cmd
}

// mockExample is the --help example of keploy mock <verb>. Under the proven
// model it runs only what the proven model takes: a Go suite natively, and
// the replay of this build, which does not judge each test, with
// KEPLOY_PROVEN=0, as its refusal says (a build that judges each test, as
// enterprise's does, gives its own: MockExampleJudged).
func mockExample(verb string) string {
	if !provenswitch.On() {
		if verb == "record" {
			return `keploy mock record -c "pytest" --name default`
		}
		return `keploy mock replay -c "pytest" --name default --on-miss fail`
	}
	if verb == "record" {
		return MockExampleJudged(verb)
	}
	return provenswitch.Env + "=0 " + MockExampleJudged(verb)
}

// MockExampleJudged is keploy mock <verb>'s --help example under the proven
// model in a build that judges each test (keploy enterprise): a Go suite,
// natively, with the flags the proven model takes.
func MockExampleJudged(verb string) string {
	return `keploy mock ` + verb + ` -c "go test -p 1 -json ./..." --name default`
}

// MockRecord builds `keploy mock record`. Exported so enterprise can compose it.
// It does NOT call AddFlags — the parent's loop does.
func MockRecord(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	return &cobra.Command{
		Use:     "record",
		Short:   "Record the outgoing dependency calls your test command makes",
		Example: mockExample("record"),
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, "mock-record")
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				// Return the error, don't swallow it: main's exitCodeForCmdErr
				// turns a non-nil error into exit 1. Returning nil here reported
				// SUCCESS to the shell and to CI after a hard setup failure — so
				// `keploy mock record ... --name nope` printed four ERROR lines and
				// still exited 0. The wrapped runner's own exit code is a
				// separate channel (utils.ErrCode via propagateExit) and is
				// untouched by this.
				return err
			}
			m, ok := svc.(mockSvc.Service)
			if !ok {
				err := fmt.Errorf("service doesn't satisfy mock service interface")
				utils.LogError(logger, nil, err.Error())
				return err
			}
			// Ensure background goroutines (agent monitor, proxy streams) are
			// torn down when Record returns, unless the user already interrupted.
			defer func() {
				select {
				case <-ctx.Done():
				default:
					utils.ExecCancel()
				}
			}()
			if err := m.Record(ctx); err != nil {
				// A user interrupt is not a failure.
				if ctx.Err() == context.Canceled {
					return nil
				}
				if errors.Is(err, mockSvc.ErrRecordRefused) {
					return err
				}
				utils.LogError(logger, err, "failed to record mocks")
				// Record() returns nil after a merely-failing test command (it calls
				// propagateExit and lets utils.ErrCode mirror the runner), so a
				// non-nil error here is always a real Keploy failure and must not
				// exit 0.
				return err
			}
			return nil
		},
	}
}

// MockReplay builds `keploy mock replay`. Exported so enterprise can compose it.
// It does NOT call AddFlags — the parent's loop does.
func MockReplay(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	return &cobra.Command{
		Use:     "replay",
		Short:   "Replay recorded mocks while your test command runs",
		Example: mockExample("replay"),
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, "mock-replay")
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				// Return the error, don't swallow it: main's exitCodeForCmdErr
				// turns a non-nil error into exit 1. Returning nil here reported
				// SUCCESS to the shell and to CI after a hard setup failure — so
				// `keploy mock replay ... --name nope` printed four ERROR lines and
				// still exited 0. The wrapped runner's own exit code is a
				// separate channel (utils.ErrCode via propagateExit) and is
				// untouched by this.
				return err
			}
			m, ok := svc.(mockSvc.Service)
			if !ok {
				err := fmt.Errorf("service doesn't satisfy mock service interface")
				utils.LogError(logger, nil, err.Error())
				return err
			}
			defer func() {
				select {
				case <-ctx.Done():
				default:
					utils.ExecCancel()
				}
			}()
			if err := m.Replay(ctx); err != nil {
				// A user interrupt is not keploy's failure, and no pass
				// either: it exits 128+N for the signal
				// (utils.InterruptExitCode) unless a code is already armed,
				// never the test command's 0. keploy's own Replay arms it and
				// returns nil; this arms it for a service that wraps Replay
				// (enterprise composes it) and fails on the cancel instead. A
				// wrapper that returns nil must arm it itself.
				if ctx.Err() == context.Canceled {
					if code := utils.InterruptExitCode(); code != 0 {
						utils.SetExitCodeOnce(code)
					}
					return nil
				}
				utils.LogError(logger, err, "failed to replay mocks")
				// Replay() returns nil after a merely-failing test command (it calls
				// propagateExit and lets utils.ErrCode mirror the runner), so a
				// non-nil error here is always a real Keploy failure and must not
				// exit 0.
				return err
			}
			return nil
		},
	}
}
