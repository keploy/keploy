package cli

import (
	"context"
	"errors"
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

func init() {
	Register("sandbox", Sandbox)
}

func Sandbox(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "sandbox",
		Short: "Managing sandbox",
	}

	cmd.AddCommand(SandboxRecord(ctx, logger, cfg, serviceFactory, cmdConfigurator))
	cmd.AddCommand(SandboxReplay(ctx, logger, cfg, serviceFactory, cmdConfigurator))
	for _, subCmd := range cmd.Commands() {
		err := cmdConfigurator.AddFlags(subCmd)
		if err != nil {
			utils.LogError(logger, err, "failed to add flags to command", zap.String("command", subCmd.Name()))
		}
	}
	return cmd
}

func SandboxRecord(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "record",
		Short:   "record outgoing calls as sandboxes",
		Example: `keploy sandbox record -c "go test -v" --location "./sandboxes" --name "main_test"`,
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

			name, err := cmd.Flags().GetString("name")
			if err != nil {
				utils.LogError(logger, err, "failed to get name flag")
				return errors.New("failed to get name flag")
			}

			result, err := recorder.Record(ctx, models.RecordOptions{
				Command:   cfg.Command,
				Path:      cfg.Path,
				Name:      name,
				Duration:  cfg.Record.RecordTimer,
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
				logger.Warn("sandbox record command exited with non-zero code",
					zap.Int("exitCode", result.AppExitCode),
				)
			}

			logger.Info("Sandbox recording completed",
				zap.Int("sandboxCount", result.MockCount),
				zap.Strings("protocols", result.Metadata.Protocols),
				zap.String("sandboxFilePath", result.MockFilePath),
				zap.Int("exitCode", result.AppExitCode),
			)
			return nil
		},
	}

	return cmd
}

func SandboxReplay(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "replay",
		Short:   "replay recorded sandboxes during testing",
		Example: `keploy sandbox replay -c "go test -v" --location "./sandboxes" --name "main_test"`,
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

			name, err := cmd.Flags().GetString("name")
			if err != nil {
				utils.LogError(logger, err, "failed to get name flag")
				return errors.New("failed to get name flag")
			}

			replayer := mockreplay.New(logger, cfg, runtime)
			result, err := replayer.Replay(ctx, models.ReplayOptions{
				Command:   cfg.Command,
				Path:      cfg.Path,
				Name:      name,
				ProxyPort: cfg.ProxyPort,
				DNSPort:   cfg.DNSPort,
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
			logger.Info("Sandbox replay completed",
				zap.Bool("success", result.Success),
				zap.Int("sandboxesReplayed", result.MocksReplayed),
				zap.Int("sandboxesLoaded", mocksLoaded),
				zap.Int("sandboxesUnused", mocksUnused),
				zap.Int("exitCode", result.AppExitCode),
			)
			return nil
		},
	}

	return cmd
}
