package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	recordSvc "go.keploy.io/server/v2/pkg/service/record"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("record", Record)
}

func Record(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "record",
		Short:   "record the keploy testcases from the API calls",
		Example: `keploy record -c "/path/to/user/app"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			// Check if this is a Docker Compose command and suppress usage on validation errors
			commandFlag, _ := cmd.Flags().GetString("command")
			if utils.FindDockerCmd(commandFlag) == utils.DockerCompose {
				cmd.SilenceUsage = true
			}
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

			cfg := models.ReRecordCfg{
				Rerecord: false,
				TestSet:  "",
			}

			err = record.Start(ctx, cfg)

			if err != nil {
				utils.LogError(logger, err, "failed to record")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add record flags")
		return nil
	}

	return cmd
}
