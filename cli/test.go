package cli

import (
	"context"
	"go.keploy.io/server/v2/pkg/graph"
	"os/exec"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	replaySvc "go.keploy.io/server/v2/pkg/service/replay"
	"go.uber.org/zap"
)

func init() {
	Register("test", Test)
}

func Test(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var testCmd = &cobra.Command{
		Use:     "test",
		Short:   "run the recorded testcases and execute assertions",
		Example: `keploy test -c "/path/to/user/app" --delay 6`,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return cmdConfigurator.ValidateFlags(ctx, cmd, cfg)
		},
		RunE: func(cmd *cobra.Command, args []string) error {

			svc, err := serviceFactory.GetService(ctx, cmd.Name(), *cfg)
			if err != nil {
				logger.Error("failed to get service", zap.Error(err))
				return err
			}
			if replay, ok := svc.(replaySvc.Service); !ok {
				logger.Error("service doesn't satisfy replay service interface")
				return err
			} else {
				if cfg.Test.Coverage {
					g := graph.NewGraph(logger, replay, *cfg)
					err := g.Serve(ctx)
					if err != nil {
						logger.Error("failed to start graph service", zap.Error(err))
						return err
					}
				}
				replay.Start(ctx)
			}

			//TODO: Use CommandContext here.
			c := exec.Command("sudo", "chmod", "-R", "777", cfg.Path)
			err = c.Run()
			if err != nil {
				logger.Error("failed to set the permission of keploy directory", zap.Error(err))
				return err
			}
			return nil
		},
	}

	cmdConfigurator.AddFlags(testCmd, cfg)

	return testCmd
}
