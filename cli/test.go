package cli

import (
	"context"
	"fmt"
	"github.com/spf13/cobra"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/graph"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"os/exec"
)

func init() {
	// register the test command
	Register("test", Test)
}

func CheckTest(logger *zap.Logger, conf *config.Config, cmd *cobra.Command) error {
	testSets, err := cmd.Flags().GetStringSlice("testsets")
	if err != nil {
		logger.Error("failed to get the testsets", zap.Error(err))
		return err
	}
	config.SetSelectedTests(conf, testSets)

	if conf.Test.Delay <= 5 {
		logger.Warn(fmt.Sprintf("Delay is set to %d seconds, incase your app takes more time to start use --delay to set custom delay", conf.Test.Delay))
		if conf.InDocker {
			logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
		} else {
			logger.Info("Example usage: " + cmd.Example)
		}
	}
	return nil
}

func Test(ctx context.Context, logger *zap.Logger, conf *config.Config, svc Services) *cobra.Command {
	var testCmd = &cobra.Command{
		Use:     "test",
		Short:   "run the recorded testcases and execute assertions",
		Example: `keploy test -c "/path/to/user/app" --delay 6`,
		PersistentPreRunE:
		PreRunE: func(cmd *cobra.Command, args []string) error {
			return CheckTest(logger, conf, cmd)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			//isDockerCmd := len(os.Getenv("IS_DOCKER_CMD")) > 0

			testReportPath, err := utils.GetUniqueReportDir(conf.Path+"/testReports", models.TestRunTemplateName)
			if err != nil {
				logger.Error("failed to get the next test report directory", zap.Error(err))
				return err
			}

			logger.Info("", zap.Any("keploy test and mock path", conf.Test), zap.Any("keploy testReport path", testReportPath))

			if conf.Test.Coverage {
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

	testCmd.Flags().StringSliceP("testsets", "t", utils.Keys(conf.Test.SelectedTests), "Testsets to run e.g. --testsets \"test-set-1, test-set-2\"")

	testCmd.Flags().Uint64P("delay", "d", conf.Test.Delay, "User provided time to run its application")

	testCmd.Flags().Uint64("apiTimeout", conf.Test.ApiTimeout, "User provided timeout for calling its application")

	testCmd.Flags().String("mongoPassword", conf.Test.MongoPassword, "Authentication password for mocking MongoDB conn")

	testCmd.Flags().String("coverageReportPath", conf.Test.CoverageReportPath, "Write a go coverage profile to the file in the given directory.")

	testCmd.Flags().StringP("language", "l", conf.Test.Language, "application programming language")

	testCmd.Flags().Bool("ignoreOrdering", conf.Test.IgnoreOrdering, "Ignore ordering of array in response")

	testCmd.Flags().Bool("coverage", conf.Test.Coverage, "Enable coverage reporting for the testcases. for golang please set language flag to golang, ref https://keploy.io/docs/server/sdk-installation/go/")
	testCmd.Flags().Lookup("coverage").NoOptDefVal = "true"
	//testCmd.SilenceUsage = true
	//testCmd.SilenceErrors = true

	return testCmd
}
