package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	stubSvc "go.keploy.io/server/v3/pkg/service/stub"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("stub", Stub)
}

func Stub(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "stub",
		Short: "Manage stubs/mocks for external tests (e.g., Playwright, Pytest)",
		Long: `Record and replay stubs/mocks for external test frameworks.

This command allows you to capture outgoing calls (HTTP, database, etc.) as mocks 
while running external tests like Playwright, Pytest, Jest, etc., and replay them later.

Unlike 'keploy record' which captures both incoming requests and outgoing mocks,
'keploy stub record' only captures outgoing calls as mocks, making it ideal for
integration with existing test suites.`,
	}

	recordCmd := StubRecord(ctx, logger, serviceFactory, cmdConfigurator)
	replayCmd := StubReplay(ctx, logger, serviceFactory, cmdConfigurator)

	cmd.AddCommand(recordCmd)
	cmd.AddCommand(replayCmd)

	// Add flags AFTER the commands are added to parent, so Parent() check works
	if err := cmdConfigurator.AddFlags(recordCmd); err != nil {
		utils.LogError(logger, err, "failed to add stub record flags")
	}
	if err := cmdConfigurator.AddFlags(replayCmd); err != nil {
		utils.LogError(logger, err, "failed to add stub replay flags")
	}

	return cmd
}

func StubRecord(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "record",
		Short: "Record mocks while running external tests",
		Example: `  # Record mocks while running Playwright tests
  keploy stub record -c "npx playwright test"

  # Record mocks with a custom name
  keploy stub record -c "pytest tests/" --name my-test-mocks

  # Record mocks with a timer
  keploy stub record -c "npm test" --record-timer 60s`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, "stub")
			if err != nil {
				utils.LogError(logger, err, "failed to get stub service")
				return nil
			}
			var stub stubSvc.Service
			var ok bool
			if stub, ok = svc.(stubSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy stub service interface")
				return nil
			}

			err = stub.Record(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to record stubs")
				return nil
			}

			return nil
		},
	}

	// Note: flags are added by the parent Stub function after this command is added as a child

	return cmd
}

func StubReplay(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "replay",
		Short: "Replay mocks while running external tests",
		Example: `  # Replay mocks while running Playwright tests (uses latest stub)
  keploy stub replay -c "npx playwright test"

  # Replay mocks from a specific stub set
  keploy stub replay -c "pytest tests/" --name my-test-mocks

  # Replay with fallback to real services on mock miss
  keploy stub replay -c "npm test" --fallback-on-miss`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, "stub")
			if err != nil {
				utils.LogError(logger, err, "failed to get stub service")
				return nil
			}
			var stub stubSvc.Service
			var ok bool
			if stub, ok = svc.(stubSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy stub service interface")
				return nil
			}

			// defering the stop function to stop keploy in case of any error in test or in case of context cancellation
			defer func() {
				select {
				case <-ctx.Done():
					break
				default:
					utils.ExecCancel()
				}
			}()

			err = stub.Replay(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to replay stubs")
				return nil
			}

			return nil
		},
	}

	// Note: flags are added by the parent Stub function after this command is added as a child

	return cmd
}
