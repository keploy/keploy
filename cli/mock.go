package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
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
	cmd := &cobra.Command{
		Use:   "mock",
		Short: "Use Keploy as a mocking framework for your own tests (record/replay dependency calls)",
		Long: `Record the real outgoing calls your test suite makes, then replay them so the
suite runs without the real dependencies — a language-agnostic VCR/WireMock at
the network layer.

Examples:
  # Record the dependency calls pytest makes into the "default" mock set
  keploy mock record -c "pytest"

  # Replay them (dependencies can be offline)
  keploy mock replay -c "pytest"

  # A named set, refreshed on a merge to main, appending any new calls
  keploy mock record -c "go test ./..." --name orders
  keploy mock replay -c "go test ./..." --name orders --on-miss record`,
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

// MockRecord builds `keploy mock record`. Exported so enterprise can compose it.
// It does NOT call AddFlags — the parent's loop does.
func MockRecord(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	return &cobra.Command{
		Use:     "record",
		Short:   "Record the outgoing dependency calls your test command makes",
		Example: `keploy mock record -c "pytest" --name default`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, "mock-record")
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			m, ok := svc.(mockSvc.Service)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy mock service interface")
				return nil
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
				if ctx.Err() != context.Canceled {
					utils.LogError(logger, err, "failed to record mocks")
				}
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
		Example: `keploy mock replay -c "pytest" --name default --on-miss fail`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, "mock-replay")
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			m, ok := svc.(mockSvc.Service)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy mock service interface")
				return nil
			}
			defer func() {
				select {
				case <-ctx.Done():
				default:
					utils.ExecCancel()
				}
			}()
			if err := m.Replay(ctx); err != nil {
				if ctx.Err() != context.Canceled {
					utils.LogError(logger, err, "failed to replay mocks")
				}
			}
			return nil
		},
	}
}
