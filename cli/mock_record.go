package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	mockreplaySvc "go.keploy.io/server/v3/pkg/service/mockreplay"
	recordSvc "go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func MockRecord(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "record",
		Short:   "record outgoing mocks for a command",
		Example: `keploy mock record -c "go test ./..." --metadata "name=checkout-mocks"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return nil
			}
			var record recordSvc.Service
			var ok bool
			if record, ok = svc.(recordSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy record service interface")
				return nil
			}

			cfg.Record.MocksOnly = true

			reRecordCfg := models.ReRecordCfg{
				Rerecord: false,
				TestSet:  "",
			}

			if err := record.Start(ctx, reRecordCfg); err != nil {
				utils.LogError(logger, err, "failed to record mocks")
				return nil
			}

			return nil
		},
	}

	return cmd
}

func MockTest(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "test",
		Aliases: []string{"replay"},
		Short:   "run a command with recorded mocks",
		Example: `keploy mock replay -c "go test ./..." --test-sets "checkout-mocks"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cfg.Command == "" {
				utils.LogError(logger, nil, "mock replay requires a command; basePath mode is not supported")
				return nil
			}

			svc, err := serviceFactory.GetService(ctx, "mock-test")
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", "mock-test"))
				return nil
			}
			var mockReplay mockreplaySvc.Service
			var ok bool
			if mockReplay, ok = svc.(mockreplaySvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy mock replay service interface")
				return nil
			}

			if err := mockReplay.Start(ctx); err != nil {
				if ctx.Err() != context.Canceled {
					utils.LogError(logger, err, "failed to replay mocks")
				}
			}

			return nil
		},
	}

	return cmd
}
