package cli

import (
	"context"
	"os/exec"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/graph"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
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
			return cmdConfigurator.ValidateFlags(cmd, cfg)
		},
		RunE: func(cmd *cobra.Command, args []string) error {

			testReportPath, err := utils.GetUniqueReportDir(cfg.Path+"/testReports", models.TestRunTemplateName)
			if err != nil {
				logger.Error("failed to get the next test report directory", zap.Error(err))
				return err
			}

			logger.Info("", zap.Any("keploy test and mock path", cfg.Test), zap.Any("keploy testReport path", testReportPath))

			if cfg.Test.Coverage {
				g := graph.NewGraph(logger)
				return g.Serve(path, proxyPort, mongoPassword, testReportPath, delay, pid, port, lang, ports, apiTimeout, appCmd, enableTele)
			}
			t.tester.StartTest(path, testReportPath, appCmd, Options{
				Tests:              tests,
				AppContainer:       appContainer,
				AppNetwork:         networkName,
				MongoPassword:      mongoPassword,
				Delay:              delay,
				BuildDelay:         buildDelay,
				PassThroughPorts:   ports,
				ApiTimeout:         apiTimeout,
				ProxyPort:          proxyPort,
				GlobalNoise:        globalNoise,
				TestsetNoise:       testsetNoise,
				WithCoverage:       withCoverage,
				CoverageReportPath: coverageReportPath,
				IgnoreOrdering:     ignoreOrdering,
				PassthroughHosts:   passThroughHosts,
			}, enableTele)

			c := exec.Command("sudo", "chmod", "-R", "777", conf.Path)
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
