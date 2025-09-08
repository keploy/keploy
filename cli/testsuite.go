package cli

import (
	"context"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	testsuiteSvc "go.keploy.io/server/v2/pkg/service/testsuite"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func init() {
	Register("testsuite", TestSuite)
}

func TestSuite(ctx context.Context, logger *zap.Logger, _ *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:     "testsuite",
		Short:   "execute a testsuite against a given url (--base-url)",
		Example: `keploy testsuite --base-url "http://localhost:8080/path/to/user/app"`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}

			var tsSvc testsuiteSvc.Service
			var ok bool
			if tsSvc, ok = svc.(testsuiteSvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy testsuite service interface")
				return nil
			}

			// If any other command is using the TSExecutor there is no need to report to the screen.
			// for example, the load command uses the TSExecutor to execute the testsuite. It does not need to report the execution to the screen.
			ctx = context.WithValue(ctx, "command", cmd.Name())

			_, err = tsSvc.Execute(ctx, nil)
			if err != nil {
				utils.LogError(logger, err, "failed to execute testsuite")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(cmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add testsuite flags")
		return nil
	}

	return cmd
}
