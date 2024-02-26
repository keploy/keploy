package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/models"
	recordSvc "go.keploy.io/server/v2/pkg/service/record"
	"go.uber.org/zap"
)

var filters = models.TestFilter{}

func init() {
	Register("record", Record)
}

func Record(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "record",
		Short:   "record the keploy testcases from the API calls",
		Example: `keploy record -c "/path/to/user/app"`,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return cmdConfigurator.ValidateFlags(cmd, cfg)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := serviceFactory.GetService(cmd.Name(), *cfg)
			if err != nil {
				return err
			}
			if record, ok := svc.(recordSvc.Service); !ok {
				return fmt.Errorf("record is not of type MyInterface")
			} else {
				record.Start(ctx)
			}
			return nil
		},
	}

	cmdConfigurator.AddFlags(cmd, cfg)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd
}
