package cli

import (
	"context"
	"os"

	"go.keploy.io/server/v2/pkg/graph"
	"go.keploy.io/server/v2/utils"

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
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return cmdConfigurator.Validate(ctx, cmd)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := serviceFactory.GetService(ctx, cmd.Name())
			if err != nil {
				utils.LogError(logger, err, "failed to get service")
				return nil
			}
			var replay replaySvc.Service
			var ok bool
			if replay, ok = svc.(replaySvc.Service); !ok {
				utils.LogError(logger, nil, "service doesn't satisfy replay service interface")
				return nil
			}
			if cfg.Test.Coverage {
				g := graph.NewGraph(logger, replay, *cfg)
				err := g.Serve(ctx)
				if err != nil {
					utils.LogError(logger, err, "failed to start graph service")
					return nil
				}
			}

			cmdType := utils.FindDockerCmd(cfg.Command)
			if cmdType == utils.Native && cfg.Test.GoCoverage {
				err := os.Setenv("GOCOVERDIR", cfg.Test.CoverageReportPath)
				if err != nil {
					utils.LogError(logger, err, "failed to set GOCOVERDIR")
					return nil
				}
			}

			err = replay.Start(ctx)
			if err != nil {
				utils.LogError(logger, err, "failed to replay")
				return nil
			}

			return nil
		},
	}

	err := cmdConfigurator.AddFlags(testCmd)
	if err != nil {
		utils.LogError(logger, err, "failed to add test flags")
		return nil
	}

	return testCmd
}
