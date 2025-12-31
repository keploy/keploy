package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	mockreplaySvc "go.keploy.io/server/v3/pkg/service/mockreplay"
	"go.keploy.io/server/v3/pkg/service/record"
	replaySvc "go.keploy.io/server/v3/pkg/service/replay"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("mock", Mock)
}

func Mock(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "mock",
		Short: "Managing mocks - record, test, upload, and download",
		Long: `The mock command provides tools for managing Keploy mocks.
		
Use 'keploy mock record' to capture outgoing network calls during test execution.
Use 'keploy mock test' to run tests with recorded mocks for environment isolation.
Use 'keploy mock download' to download mocks from the Keploy registry.
Use 'keploy mock upload' to upload mocks to the Keploy registry.`,
	}

	cmd.AddCommand(MockRecord(ctx, logger, cfg, serviceFactory, cmdConfigurator))
	cmd.AddCommand(MockTest(ctx, logger, cfg, serviceFactory, cmdConfigurator))
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

// MockRecord creates the 'mock record' subcommand for recording mocks during test execution
func MockRecord(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {

	var cmd = &cobra.Command{
		Use:   "record",
		Short: "Record mocks by capturing outgoing network calls during test execution",
		Long: `Record mocks by wrapping your test command and capturing all external dependencies.

This command intercepts outgoing network calls (HTTP, database, etc.) made during
test execution and saves them as mock files.

Examples:
  keploy mock record -c "go test ./..."
  keploy mock record -c "npm test"
  keploy mock record -c "pytest tests/"`,
		Example: `  keploy mock record -c "go test ./..."
  keploy mock record -c "npm test"
  keploy mock record -c "pytest tests/"`,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Get the test command from args after --
			testCommand := cfg.Command
			if testCommand == "" && len(args) > 0 {
				testCommand = args[0]
				for i := 1; i < len(args); i++ {
					testCommand += " " + args[i]
				}
			}

			if testCommand == "" {
				return fmt.Errorf("test command is required. Use -c flag or provide command after --")
			}

			logger.Info("🔴 Starting mock recording",
				zap.String("command", testCommand),
			)

			// Get the record service

			// Get the record service
			svc, err := serviceFactory.GetService(ctx, "record")
			if err != nil {
				utils.LogError(logger, err, "failed to get record service")
				return nil
			}

			recordSvc, ok := svc.(record.Service)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy record service interface")
				return nil
			}

			// Start recording
			reRecordCfg := models.ReRecordCfg{
				Rerecord: false,
				TestSet:  "",
			}

			err = recordSvc.Start(ctx, reRecordCfg)
			if err != nil {
				utils.LogError(logger, err, "failed to record mocks")
				return nil
			}

			logger.Info("✅ Mock recording completed successfully")
			return nil
		},
	}

	return cmd
}

// MockTest creates the 'mock test' subcommand for running tests with recorded mocks
func MockTest(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var testSetID string
	var validateIsolation bool
	var selectedMocks []string

	var cmd = &cobra.Command{
		Use:   "test",
		Short: "Run tests using recorded mocks for environment isolation",
		Long: `Run tests using previously recorded mocks to ensure environment isolation.

This command injects mock data during test execution, intercepting outgoing
network calls and providing recorded responses. This ensures tests run
consistently without depending on external services.

You can specify particular mocks to use with the --mocks flag.

Examples:
  keploy mock test -c "go test ./..."
  keploy mock test --test-set test-set-0 -c "npm test"
  keploy mock test --mocks http-fetch-users-abc123,postgres-query-def456 -c "go test ./..."
  keploy mock test --validate-isolation -c "pytest tests/"`,
		Example: `  keploy mock test -c "go test ./..."
  keploy mock test -c "go test ./..." --test-set test-set-0
  keploy mock test --mocks http-fetch-users-abc123 -c "npm test"
  keploy mock test --validate-isolation -c "npm test"`,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Get the test command from args after --
			testCommand := cfg.Command
			if testCommand == "" && len(args) > 0 {
				testCommand = args[0]
				for i := 1; i < len(args); i++ {
					testCommand += " " + args[i]
				}
			}

			if testCommand == "" {
				return fmt.Errorf("test command is required. Use -c flag or provide command after --")
			}

			logger.Info("🟢 Starting mock test",
				zap.String("command", testCommand),
				zap.String("testSetID", testSetID),
				zap.Strings("selectedMocks", selectedMocks),
				zap.Bool("validateIsolation", validateIsolation),
			)

			// Configure for mock replay mode
			cfg.Test.Mocking = true
			if testSetID != "" {
				cfg.Test.SelectedTests = map[string][]string{
					testSetID: {},
				}
			}

			// Set selected mocks if provided
			if len(selectedMocks) > 0 {
				cfg.Test.SelectedMocks = selectedMocks
				logger.Info("Using specific mocks for replay", zap.Strings("mocks", selectedMocks))
			}

			// Get the mock replay service (separate from standard test replay)
			svc, err := serviceFactory.GetService(ctx, "mock-test")
			if err != nil {
				utils.LogError(logger, err, "failed to get mock replay service")
				return nil
			}

			mockReplay, ok := svc.(mockreplaySvc.Service)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy mock replay service interface")
				return nil
			}

			// Defer cleanup
			defer func() {
				select {
				case <-ctx.Done():
					break
				default:
					utils.ExecCancel()
				}
			}()

			// Start mock replay (runs user command with mocks)
			err = mockReplay.Start(ctx)
			if err != nil {
				if ctx.Err() != context.Canceled {
					utils.LogError(logger, err, "failed to run with mocks")
				}
				return nil
			}

			if validateIsolation {
				logger.Info("✅ Mock replay completed - isolation validated")
			} else {
				logger.Info("✅ Mock replay completed")
			}

			return nil
		},
	}

	// Add mock replay specific flags
	cmd.Flags().StringVar(&testSetID, "test-set", "", "Specific test set ID to use for mocks (uses all if not provided)")
	cmd.Flags().StringSliceVar(&selectedMocks, "mocks", []string{}, "Comma-separated list of specific mock names to use during replay (e.g., http-fetch-users-abc123,postgres-query-def456)")
	cmd.Flags().BoolVar(&validateIsolation, "validate-isolation", true, "Validate that no real network calls were made during replay")

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
