package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/graph"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/service/test"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func NewCmdTest(logger *zap.Logger) *Test {
	tester := test.NewTester(logger)
	return &Test{
		tester: tester,
		logger: logger,
	}
}

func ReadTestConfig(configPath string) (*models.Test, error) {
	file, err := os.OpenFile(configPath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	var doc models.Config
	err = decoder.Decode(&doc)
	if err != nil {
		return nil, err
	}
	return &doc.Test, nil
}

func (t *Test) getTestConfig(path *string, proxyPort *uint32, appCmd *string, tests *map[string][]string, appContainer, networkName *string, Delay *uint64, buildDelay *time.Duration, passThroughPorts *[]uint, apiTimeout *uint64, globalNoise *models.GlobalNoise, testSetNoise *models.TestsetNoise, coverageReportPath *string, withCoverage *bool, generateTestReport *bool, configPath string, ignoreOrdering *bool, passThroughHosts *[]models.Filters) error {
	configFilePath := filepath.Join(configPath, "keploy-config.yaml")
	if isExist := utils.CheckFileExists(configFilePath); !isExist {
		return errFileNotFound
	}
	confTest, err := ReadTestConfig(configFilePath)
	if err != nil {
		return fmt.Errorf("failed to get the test config from config file due to error: %s", err)
	}
	if len(*path) == 0 {
		*path = confTest.Path
	}
	if *proxyPort == 0 {
		*proxyPort = confTest.ProxyPort
	}
	if *appCmd == "" {
		*appCmd = confTest.Command
	}
	for testset, testcases := range confTest.SelectedTests {
		if _, ok := (*tests)[testset]; !ok {
			(*tests)[testset] = testcases
		}
	}
	if *appContainer == "" {
		*appContainer = confTest.ContainerName
	}
	if *networkName == "" {
		*networkName = confTest.NetworkName
	}
	if *Delay == 5 {
		*Delay = confTest.Delay
	}
	if *buildDelay == 30*time.Second && confTest.BuildDelay != 0 {
		*buildDelay = confTest.BuildDelay
	}

	if len(*coverageReportPath) == 0 {
		*coverageReportPath = confTest.CoverageReportPath
	}
	*withCoverage = *withCoverage || confTest.WithCoverage
	*generateTestReport = *generateTestReport || confTest.GenerateTestReport
	if *apiTimeout == 5 {
		*apiTimeout = confTest.ApiTimeout
	}
	*globalNoise = confTest.GlobalNoise.Global
	*testSetNoise = confTest.GlobalNoise.Testsets
	if !*ignoreOrdering {
		*ignoreOrdering = confTest.IgnoreOrdering
	}
	passThroughPortProvided := len(*passThroughPorts) == 0
	for _, filter := range confTest.Stubs.Filters {
		if filter.Port != 0 && filter.Host == "" && filter.Path == "" && passThroughPortProvided {
			*passThroughPorts = append(*passThroughPorts, filter.Port)
		} else {
			*passThroughHosts = append(*passThroughHosts, filter)
		}
	}

	return nil
}

type Test struct {
	tester test.Tester
	logger *zap.Logger
}

func (t *Test) GetCmd() *cobra.Command {
	var testCmd = &cobra.Command{
		Use:     "test",
		Short:   "run the recorded testcases and execute assertions",
		Example: `sudo -E env PATH=$PATH keploy test -c "/path/to/user/app" --delay 6`,
		RunE: func(cmd *cobra.Command, args []string) error {
			isDockerCmd := len(os.Getenv("IS_DOCKER_CMD")) > 0

			path, err := cmd.Flags().GetString("path")
			if err != nil {
				t.logger.Error("failed to read the testcase path input")
				return err
			}
			withCoverage, err := cmd.Flags().GetBool("withCoverage")
			if err != nil {
				t.logger.Error("failed to read the go coverage binary", zap.Error(err))
				return err
			}
			coverageReportPath, err := cmd.Flags().GetString("coverageReportPath")
			if err != nil {
				t.logger.Error("failed to read the go coverage directory path", zap.Error(err))
				return err
			}

			appCmd, err := cmd.Flags().GetString("command")
			if err != nil {
				t.logger.Error("Failed to get the command to run the user application", zap.Error((err)))
				return err
			}

			appContainer, err := cmd.Flags().GetString("containerName")
			if err != nil {
				t.logger.Error("Failed to get the application's docker container name", zap.Error((err)))
				return err
			}

			networkName, err := cmd.Flags().GetString("networkName")
			if err != nil {
				t.logger.Error("Failed to get the application's docker network name", zap.Error((err)))
				return err
			}

			delay, err := cmd.Flags().GetUint64("delay")
			if err != nil {
				t.logger.Error("Failed to get the delay flag", zap.Error((err)))
				return err
			}

			coverage, err := cmd.Flags().GetBool("coverage")
			if err != nil {
				t.logger.Error("Failed to get the coverage flag", zap.Error((err)))
				return err
			}
			var lang string
			var pid uint32
			var port uint32
			if !coverage {

				lang, err = cmd.Flags().GetString("language")
				if err != nil {
					t.logger.Error("failed to read the programming language")
					return err
				}

				pid, err = cmd.Flags().GetUint32("pid")
				if err != nil {
					t.logger.Error("Failed to get the pid of the application", zap.Error((err)))
					return err
				}

				port, err = cmd.Flags().GetUint32("port")
				if err != nil {
					t.logger.Error("Failed to get the port of keploy server", zap.Error((err)))
					return err
				}

			}

			buildDelay, err := cmd.Flags().GetDuration("buildDelay")
			if err != nil {
				t.logger.Error("Failed to get the build-delay flag", zap.Error((err)))
				return err
			}

			apiTimeout, err := cmd.Flags().GetUint64("apiTimeout")
			if err != nil {
				t.logger.Error("Failed to get the apiTimeout flag", zap.Error((err)))
				return err
			}

			ports, err := cmd.Flags().GetUintSlice("passThroughPorts")
			if err != nil {
				t.logger.Error("failed to read the ports of outgoing calls to be ignored")
				return err
			}

			// port, err := cmd.Flags().GetUint32("port")
			// if err != nil {
			// 	t.logger.Error("failed to read the port of keploy server")
			// 	return err
			// }

			proxyPort, err := cmd.Flags().GetUint32("proxyport")
			if err != nil {
				t.logger.Error("failed to read the proxyport")
				return err
			}

			configPath, err := cmd.Flags().GetString("config-path")
			if err != nil {
				t.logger.Error("failed to read the config path")
				return err
			}

			generateTestReport, err := cmd.Flags().GetBool("generateTestReport")
			if err != nil {
				t.logger.Error("failed to read the generate test teport flag")
				return err
			}

			enableTele, err := cmd.Flags().GetBool("enableTele")
			if err != nil {
				t.logger.Error("failed to read the disable telemetry flag")
				return err
			}

			ignoreOrdering, err := cmd.Flags().GetBool("ignoreOrdering")
			if err != nil {
				t.logger.Error("failed to read the ignore ordering flag")
				return err
			}

			tests := map[string][]string{}

			testsets, err := cmd.Flags().GetStringSlice("testsets")
			if err != nil {
				t.logger.Error("Failed to read the testsets")
				return err
			}

			for _, testset := range testsets {
				tests[testset] = []string{}
			}

			globalNoise := make(models.GlobalNoise)
			testsetNoise := make(models.TestsetNoise)

			passThroughHosts := []models.Filters{}

			err = t.getTestConfig(&path, &proxyPort, &appCmd, &tests, &appContainer, &networkName, &delay, &buildDelay, &ports, &apiTimeout, &globalNoise, &testsetNoise, &coverageReportPath, &withCoverage, &generateTestReport, configPath, &ignoreOrdering, &passThroughHosts)
			if err != nil {
				if err == errFileNotFound {
					t.logger.Info("Keploy config not found, continuing without configuration")
				} else {
					t.logger.Error("", zap.Error(err))
				}
			}

			if appCmd == "" {
				t.logger.Error("Couldn't find appCmd")
				if isDockerCmd {
					t.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
				} else {
					t.logger.Info(fmt.Sprintf("Example usage: %s", cmd.Example))
				}
				return errors.New("missing required -c flag or appCmd in config file")
			}

			if delay <= 5 {
				t.logger.Warn(fmt.Sprintf("Delay is set to %d seconds, incase your app takes more time to start use --delay to set custom delay", delay))
				if isDockerCmd {
					t.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
				} else {
					t.logger.Info("Example usage: " + cmd.Example)
				}
			}

			if isDockerCmd && buildDelay <= 30*time.Second {
				t.logger.Warn(fmt.Sprintf("buildDelay is set to %v, incase your docker container takes more time to build use --buildDelay to set custom delay", buildDelay))
				t.logger.Info(`Example usage:keploy test -c "docker-compose up --build" --buildDelay 35s`)
			}

			if isDockerCmd && len(path) > 0 {
				curDir, err := os.Getwd()
				if err != nil {
					t.logger.Error("failed to get current working directory", zap.Error(err))
					return err
				}
				// Check if the path contains the moving up directory (..)
				if strings.Contains(path, "..") {
					path, err = filepath.Abs(filepath.Clean(path))
					if err != nil {
						t.logger.Error("failed to get the absolute path from relative path", zap.Error(err), zap.String("path:", path))
						return nil
					}
					relativePath, err := filepath.Rel(curDir, path)
					if err != nil {
						t.logger.Error("failed to get the relative path from absolute path", zap.Error(err), zap.String("path:", path))
						return nil
					}
					if relativePath == ".." || strings.HasPrefix(relativePath, "../") {
						t.logger.Error("path provided is not a subdirectory of current directory. Keploy only supports recording testcases in the current directory or its subdirectories", zap.String("path:", path))
						return nil
					}
				} else if strings.HasPrefix(path, "/") { // Check if the path is absolute path.
					// Check if the path is a subdirectory of current directory
					// Get the current directory path in docker.
					getDir := fmt.Sprintf(`docker inspect keploy-v2 --format '{{ range .Mounts }}{{ if eq .Destination "%s" }}{{ .Source }}{{ end }}{{ end }}'`, curDir)
					cmd := exec.Command("sh", "-c", getDir)
					out, err := cmd.Output()
					if err != nil {
						t.logger.Error("failed to get the current directory path in docker", zap.Error(err), zap.String("path:", path))
						return nil
					}
					currentDir := strings.TrimSpace(string(out))
					t.logger.Debug("This is the path after trimming", zap.String("currentDir:", currentDir))
					// Check if the path is a subdirectory of current directory
					if !strings.HasPrefix(path, currentDir) {
						t.logger.Error("path provided is not a subdirectory of current directory. Keploy only supports recording testcases in the current directory or its subdirectories", zap.String("path:", path))
						return nil
					}
					// Set the relative path.
					path, err = filepath.Rel(currentDir, path)
					if err != nil {
						t.logger.Error("failed to get the relative path for the subdirectory", zap.Error(err), zap.String("path:", path))
						return nil
					}
				}
			}

			mongoPassword, err := cmd.Flags().GetString("mongoPassword")
			if err != nil {
				t.logger.Error("failed to read the mongo password")
				return err
			}

			t.logger.Debug("the configuration for mocking mongo connection", zap.Any("password", mongoPassword))
			//Check if app command starts with docker or  docker-compose.
			dockerRelatedCmd, dockerCmd := utils.IsDockerRelatedCmd(appCmd)
			if !isDockerCmd && dockerRelatedCmd {
				isDockerCompose := false
				if dockerCmd == "docker-compose" {
					isDockerCompose = true
				}
				testCfg := utils.TestFlags{
					Path:               path,
					Proxyport:          proxyPort,
					Command:            appCmd,
					Testsets:           testsets,
					ContainerName:      appContainer,
					NetworkName:        networkName,
					Delay:              delay,
					BuildDelay:         buildDelay,
					ApiTimeout:         apiTimeout,
					PassThroughPorts:   ports,
					ConfigPath:         configPath,
					MongoPassword:      mongoPassword,
					CoverageReportPath: coverageReportPath,
					EnableTele:         enableTele,
					WithCoverage:       withCoverage,
				}
				utils.UpdateKeployToDocker("test", isDockerCompose, testCfg, t.logger)
				return nil
			}

			//if user provides relative path
			if len(path) > 0 && path[0] != '/' {
				absPath, err := filepath.Abs(path)
				if err != nil {
					t.logger.Error("failed to get the absolute path from relative path", zap.Error(err))
				}
				path = absPath
			} else if len(path) == 0 { // if user doesn't provide any path
				cdirPath, err := os.Getwd()
				if err != nil {
					t.logger.Error("failed to get the path of current directory", zap.Error(err))
				}
				path = cdirPath
			} else {
				// user provided the absolute path
			}

			path += "/keploy"
			t.logger.Info("", zap.Any("keploy test and mock path", path))

			testReportPath := ""

			if generateTestReport {
				testReportPath = path + "/testReports"
	
				testReportPath, err = pkg.GetNextTestReportDir(testReportPath, models.TestRunTemplateName)
					t.logger.Info("", zap.Any("keploy testReport path", testReportPath))
					if err != nil {
						t.logger.Error("failed to get the next test report directory", zap.Error(err))
						return err
					}
			}
			
			var hasContainerName bool
			if isDockerCmd {
				if strings.Contains(appCmd, "--name") {
					hasContainerName = true
				}
				if !hasContainerName && appContainer == "" {
					t.logger.Error("Couldn't find containerName")
					t.logger.Info(`Example usage: keploy test -c "docker run -p 8080:8080 --network myNetworkName myApplicationImageName" --delay 6`)
					return errors.New("missing required --containerName flag or containerName in config file")
				}
			}

			//flags like lang, pid, port cannot be used unless called the serve method
			// Check if the coverage flag is set

			t.logger.Debug("the ports are", zap.Any("ports", ports))

			if coverage {
				g := graph.NewGraph(t.logger)
				g.Serve(path, proxyPort, mongoPassword, testReportPath, generateTestReport, delay, pid, port, lang, ports, apiTimeout, appCmd, enableTele)
			} else {
				t.tester.Test(path, testReportPath, generateTestReport, appCmd, test.TestOptions{
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
			}

			return nil
		},
	}

	testCmd.Flags().StringP("path", "p", "", "Path to local directory where generated testcases/mocks are stored")

	testCmd.Flags().Uint32("port", 6789, "Port at which you want to run graphql Server")

	testCmd.Flags().Uint32("proxyport", 0, "Choose a port to run Keploy Proxy.")

	testCmd.Flags().StringP("command", "c", "", "Command to start the user application")

	testCmd.Flags().StringSliceP("testsets", "t", []string{}, "Testsets to run e.g. --testsets \"test-set-1, test-set-2\"")

	testCmd.Flags().String("containerName", "", "Name of the application's docker container")

	testCmd.Flags().StringP("networkName", "n", "", "Name of the application's docker network")
	testCmd.Flags().Uint64P("delay", "d", 5, "User provided time to run its application")

	testCmd.Flags().DurationP("buildDelay", "", 30*time.Second, "User provided time to wait docker container build")

	testCmd.Flags().Uint64("apiTimeout", 5, "User provided timeout for calling its application")

	testCmd.Flags().UintSlice("passThroughPorts", []uint{}, "Ports of Outgoing dependency calls to be ignored as mocks")

	testCmd.Flags().String("config-path", ".", "Path to the local directory where keploy configuration file is stored")

	testCmd.Flags().String("mongoPassword", "default123", "Authentication password for mocking MongoDB connection")

	testCmd.Flags().String("coverageReportPath", "", "Write a go coverage profile to the file in the given directory.")

	testCmd.Flags().StringP("language", "l", "", "application programming language")

	testCmd.Flags().Uint32("pid", 0, "Process id of your application.")

	testCmd.Flags().BoolP("generateTestReport", "g", true, "Generate of test report")

	testCmd.Flags().Bool("enableTele", true, "Switch for telemetry")

	testCmd.Flags().Bool("ignoreOrdering", true, "Ignore ordering of array in response")

	testCmd.Flags().MarkHidden("enableTele")

	testCmd.Flags().Bool("withCoverage", false, "Capture the code coverage of the go binary in the command flag.")

	testCmd.Flags().Lookup("withCoverage").NoOptDefVal = "true"

	testCmd.Flags().Bool("coverage", false, "Capture the code coverage of the go binary in the command flag.")
	testCmd.Flags().Lookup("coverage").NoOptDefVal = "true"
	testCmd.SilenceUsage = true
	testCmd.SilenceErrors = true

	return testCmd
}