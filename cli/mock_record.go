package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	recordSvc "go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/pkg/service/mockrecord"
	"go.keploy.io/server/v3/pkg/service/mockreplay"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func MockRecord(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "record",
		Short:   "record outgoing calls as mocks",
		Example: `keploy mock record -c "npm start" -p ./keploy --duration 60s`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			recSvc, err := serviceFactory.GetService(ctx, "record")
			if err != nil {
				utils.LogError(logger, err, "failed to get record service")
				return nil
			}

			recordService, ok := recSvc.(recordSvc.Service)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy record service interface")
				return nil
			}

			recorder := mockrecord.New(logger, cfg, recordService)
			result, err := recorder.Record(ctx, models.RecordOptions{
				Command:  cfg.Command,
				Path:     cfg.Path,
				Duration: cfg.Record.RecordTimer,
				ProxyPort: cfg.ProxyPort,
				DNSPort:   cfg.DNSPort,
			})
			if err != nil {
				utils.LogError(logger, err, "failed to record mocks")
				return nil
			}

			if output := strings.TrimSpace(result.Output); output != "" {
				fmt.Fprintln(cmd.OutOrStdout(), output)
			}
			if result.AppExitCode != 0 {
				logger.Warn("mock record command exited with non-zero code",
					zap.Int("exitCode", result.AppExitCode),
				)
			}

			logger.Info("Mock recording completed",
				zap.Int("mockCount", result.MockCount),
				zap.Strings("protocols", result.Metadata.Protocols),
				zap.String("mockFilePath", result.MockFilePath),
				zap.Int("exitCode", result.AppExitCode),
			)
			return nil
		},
	}

	return cmd
}

func MockTest(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "test",
		Short:   "replay recorded mocks during testing",
		Example: `keploy mock test -c "go test ./..." --mock-path ./keploy/user-service`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			mockPath, err := cmd.Flags().GetString("mock-path")
			if err != nil {
				utils.LogError(logger, err, "failed to read mock-path flag")
				return nil
			}

			agentSvc, err := serviceFactory.GetService(ctx, "agent")
			if err != nil {
				utils.LogError(logger, err, "failed to get agent service")
				return nil
			}

			agentService, ok := agentSvc.(agent.Service)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy agent service interface")
				return nil
			}

			replayer := mockreplay.New(logger, cfg, &replayAgentAdapter{agent: agentService}, nil)
			result, err := replayer.Replay(ctx, models.ReplayOptions{
				Command:        cfg.Command,
				MockFilePath:   mockPath,
				FallBackOnMiss: cfg.Test.FallBackOnMiss,
				ProxyPort:      cfg.ProxyPort,
				DNSPort:        cfg.DNSPort,
			})
			if err != nil {
				utils.LogError(logger, err, "failed to replay mocks")
				return nil
			}

			if output := strings.TrimSpace(result.Output); output != "" {
				fmt.Fprintln(cmd.OutOrStdout(), output)
			}

			logger.Info("Mock replay completed",
				zap.Bool("success", result.Success),
				zap.Int("mocksReplayed", result.MocksReplayed),
				zap.Int("mocksMissed", result.MocksMissed),
				zap.Int("exitCode", result.AppExitCode),
			)
			return nil
		},
	}

	return cmd
}
