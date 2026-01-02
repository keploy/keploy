package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	mockSvc "go.keploy.io/server/v3/pkg/service/mock"
	replaySvc "go.keploy.io/server/v3/pkg/service/replay"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("mock", Mock)
}

func Mock(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "mock",
		Short: "Manage mocks - record and replay outgoing calls",
		Long: `The mock command allows you to record and replay outgoing calls from any command.
This is useful for mocking external dependencies like HTTP APIs, databases, etc.

Examples:
  # Record all outgoing calls from pytest
  keploy mock record -c "pytest"

  # Replay previously captured mocks
  keploy mock replay -c "pytest"`,
	}

	cmd.AddCommand(MockRecord(ctx, logger, serviceFactory, cmdConfigurator))
	cmd.AddCommand(MockReplay(ctx, logger, serviceFactory, cmdConfigurator))
	cmd.AddCommand(DownloadMocks(ctx, logger, serviceFactory, cmdConfigurator))
	cmd.AddCommand(UploadMocks(ctx, logger, serviceFactory, cmdConfigurator))
	for _, subCmd := range cmd.Commands() {
		err := cmdConfigurator.AddFlags(subCmd)
		if err != nil {
			utils.LogError(logger, err, "failed to add flags to command", zap.String("command", subCmd.Name()))
		}
	}
	return cmd
}

// MockRecord creates the mock record subcommand
func MockRecord(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "record",
		Aliases: []string{"mock-record"},
		Short:   "Record outgoing calls as mocks",
		Long: `Record all outgoing network calls (HTTP, gRPC, database queries, etc.) 
from the specified command and save them as mocks for later replay.

The recorded mocks will be saved to the keploy/stubs folder by default.`,
		Example: `  # Record outgoing calls from pytest
  keploy mock record -c "pytest"
  
  # Record with a custom mock set name
  keploy mock record -c "npm test" --mock-set "api-mocks"
  
  # Record with a timer
  keploy mock record -c "go test ./..." --record-timer 60s`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, "mock-record")
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			mock, ok := svc.(mockSvc.Service)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy mock service interface")
				return nil
			}

			// Defer stop function
			defer func() {
				select {
				case <-ctx.Done():
					break
				default:
					utils.ExecCancel()
				}
			}()

			err = mock.Record(ctx)
			if err != nil {
				if ctx.Err() != context.Canceled {
					utils.LogError(logger, err, "failed to record mocks")
				}
			}

			return nil
		},
	}

	return cmd
}

// MockReplay creates the mock replay subcommand
func MockReplay(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "replay",
		Aliases: []string{"mock-replay"},
		Short:   "Replay recorded mocks for outgoing calls",
		Long: `Replay previously recorded mocks when running the specified command.
Outgoing network calls will be intercepted and matched against recorded mocks.

For HTTP requests, matching is done based on sequence and schema with best-effort matching.`,
		Example: `  # Replay mocks for pytest
  keploy mock replay -c "pytest"
  
  # Replay a specific mock set
  keploy mock replay -c "npm test" --mock-set "api-mocks"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, "mock-replay")
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			mock, ok := svc.(mockSvc.Service)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy mock service interface")
				return nil
			}

			// Defer stop function
			defer func() {
				select {
				case <-ctx.Done():
					break
				default:
					utils.ExecCancel()
				}
			}()

			err = mock.Replay(ctx)
			if err != nil {
				if ctx.Err() != context.Canceled {
					utils.LogError(logger, err, "failed to replay mocks")
				}
			}

			return nil
		},
	}

	return cmd
}

func DownloadMocks(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "download",
		Short:   "Download mocks from the keploy registry",
		Example: `keploy mock download`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Parent().Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Parent().Name()))
				return nil
			}
			var replay replaySvc.Service
			var ok bool
			if replay, ok = svc.(replaySvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy replay service interface")
				return nil
			}

			if err := replay.DownloadMocks(ctx); err != nil {
				utils.LogError(logger, err, "failed to download mocks from keploy registry")
				return nil
			}
			return nil
		},
	}

	return cmd
}

func UploadMocks(ctx context.Context, logger *zap.Logger, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "upload",
		Short:   "Upload mocks to the keploy registry",
		Example: `keploy mock upload`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Parent().Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Parent().Name()))
				return nil
			}
			var replay replaySvc.Service
			var ok bool
			if replay, ok = svc.(replaySvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy replay service interface")
				return nil
			}
			if err := replay.UploadMocks(ctx, nil); err != nil {
				utils.LogError(logger, err, "failed to upload mocks to the keploy registry")
				return nil
			}
			return nil
		},
	}

	return cmd
}
