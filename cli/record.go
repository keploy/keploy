package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	recordSvc "go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

var BypassRules string

func init() {
	Register("record", Record)
}

func Record(ctx context.Context, logger *zap.Logger, conf *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "record",
		Short:   "record the keploy testcases from the API calls",
		Example: `keploy record -c "/path/to/user/app"`,
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

			cfg := models.ReRecordCfg{
				Rerecord: false,
				TestSet:  "",
			}

			if BypassRules != "" {
				var rules []models.BypassRule

				if err := json.Unmarshal([]byte(BypassRules), &rules); err != nil {
					return fmt.Errorf("invalid --bypassRules value: %w", err)
				}

				conf.BypassRules = rules
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

	cmd.Flags().StringVar(
		&BypassRules,
		"bypassRules",
		"",
		"JSON array for bypass rules (example: '[{\"path\":\"/health\"}]')",
	)

	return cmd
}
