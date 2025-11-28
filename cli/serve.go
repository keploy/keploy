package cli

import (
	"context"
	"errors"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	serveSvc "go.keploy.io/server/v3/pkg/service/serve"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("serve", Serve)
}

func Serve(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "serve",
		Short: "Start Keploy mock server to serve recorded mocks",
		Long: `Start Keploy mock server in standalone mode to serve recorded mocks.
This allows you to run your application against mocked dependencies while recording new test cases.

Example usage:
  # Serve mocks from default test sets on default port
  keploy serve

  # Serve mocks on a specific port
  keploy serve --port 8080

  # Serve mocks from specific test sets
  keploy serve --test-sets "test-set-1,test-set-2"
`,
		Example: `keploy serve --port 8080 --test-sets "test-set-1"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service", zap.String("command", cmd.Name()))
				return err
			}
			var serve serveSvc.Service
			var ok bool
			if serve, ok = svc.(serveSvc.Service); !ok {
				err := errors.New("service doesn't satisfy serve service interface")
				utils.LogError(logger, err, "service doesn't satisfy serve service interface")
				return err
			}

			err = serve.Start(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to start mock server")
				return err
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add serve flags")
		return nil
	}

	return cmd
}