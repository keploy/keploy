package cli

import (
	"context"
	"github.com/spf13/cobra"
	"go.keploy.io/server/config"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

var filters = models.TestFilter{}

func init() {
	// register the record command
	Register("record", Record)
}

func Record(ctx context.Context, logger *zap.Logger, conf *config.Config, svc Services) *cobra.Command {
	// record the keploy testcases/mocks for the user application
	var cmd = &cobra.Command{
		Use:     "record",
		Short:   "record the keploy testcases from the API calls",
		Example: `keploy record -c "/path/to/user/app"`,
		RunE: func(cmd *cobra.Command, args []string) error {

			recorder.StartCaptureTraffic(path, proxyPort, appCmd, appContainer, networkName, delay, buildDelay, ports, &filters, enableTele, passThrough)
			return nil
		},
	}

	cmd.SilenceUsage = true
	cmd.SilenceErrors = true

	return cmd
}
