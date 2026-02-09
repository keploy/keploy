package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/mockrecord"
	"go.keploy.io/server/v3/pkg/service/mockreplay"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func MockRecord(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "record",
		Short:   "record outgoing calls as mocks",
		Example: `keploy mock record -c "npm start" -p ./keploy`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			recordSvc, err := serviceFactory.GetService(ctx, "record")
			if err != nil {
				utils.LogError(logger, err, "failed to get record service")
				return nil
			}

			runner, ok := recordSvc.(mockrecord.RecordRunner)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy record runner interface")
				return nil
			}

			recorder := mockrecord.New(logger, cfg, runner, nil)

			// Read the --duration flag if provided, otherwise fall back to config
			duration, _ := cmd.Flags().GetDuration("duration")
			if duration == 0 {
				duration = cfg.Record.RecordTimer
			}

			result, err := recorder.Record(ctx, models.RecordOptions{
				Command:   cfg.Command,
				Path:      cfg.Path,
				Duration:  duration,
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
		Example: `keploy mock test -c "go test ./..." -p ./keploy/run-<timestamp>`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			replaySvc, err := serviceFactory.GetService(ctx, "test")
			if err != nil {
				utils.LogError(logger, err, "failed to get replay service")
				return nil
			}

			runtime, ok := replaySvc.(mockreplay.Runtime)
			if !ok {
				utils.LogError(logger, nil, "service doesn't satisfy replay runtime interface")
				return nil
			}

			replayer := mockreplay.New(logger, cfg, runtime)
			result, err := replayer.Replay(ctx, models.ReplayOptions{
				Command:        cfg.Command,
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

			mocksLoaded := result.MocksReplayed + result.MocksMissed
			mocksUnused := result.MocksMissed
			logger.Info("Mock replay completed",
				zap.Bool("success", result.Success),
				zap.Int("mocksReplayed", result.MocksReplayed),
				zap.Int("mocksLoaded", mocksLoaded),
				zap.Int("mocksUnused", mocksUnused),
				zap.Int("exitCode", result.AppExitCode),
			)
			return nil
		},
	}

	return cmd
}
