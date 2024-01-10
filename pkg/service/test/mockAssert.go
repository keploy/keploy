package test

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.uber.org/zap"
)

func (t *tester) MockAssertion(cfg *TestConfig) (InitialiseTestReturn, error) {
	returnVal := InitialiseTestReturn{}
	return returnVal, nil
}

// have to look gracefully exiting of mock assertion this can be done by two ways either the total mocks get finish or the mocks of the baseline gets finished
func (t *tester) RunMockAssert(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, buildDelay time.Duration, pid uint32, ys platform.TestCaseDB, loadedHooks *hooks.Hook, testReportFS platform.TestReportDB, testRunChan chan string, apiTimeout uint64, ctx context.Context, testcases map[string]bool, noiseConfig models.GlobalNoise, serveTest bool,baseUrl string) models.TestRunStatus {
	cfg := &RunTestSetConfig{
		TestSet:        testSet,
		Path:           path,
		TestReportPath: testReportPath,
		AppCmd:         appCmd,
		AppContainer:   appContainer,
		AppNetwork:     appNetwork,
		Delay:          delay,
		BuildDelay:     buildDelay,
		Pid:            pid,
		YamlStore:      ys,
		LoadedHooks:    loadedHooks,
		TestReportFS:   testReportFS,
		TestRunChan:    testRunChan,
		ApiTimeout:     apiTimeout,
		Ctx:            ctx,
		ServeTest:      serveTest,
	}
	initialisedValues := t.InitialiseRunMockAssert(cfg)
	if initialisedValues.InitialStatus != "" {
		return initialisedValues.InitialStatus
	}


	// Recover from panic and gracfully shutdown
	defer loadedHooks.Recover(pkg.GenerateRandomID())
	defer func() {
		if len(appCmd) == 0 && pid != 0 {
			t.logger.Debug("no need to stop the user application when running keploy tests along with unit tests")
		} 
	}()

	
	var (
		status  = models.TestRunStatusPassed
	)

	var userIp string
	userIp = initialisedValues.UserIP
	t.logger.Debug("the userip of the user docker container", zap.Any("", userIp))

	var entTcs, nonKeployTcs []string
	for _, tc := range initialisedValues.Tcs {
		if _, ok := testcases[tc.Name]; !ok && len(testcases) != 0 {
			continue
		}
		// Filter the TCS Mocks based on the test case's request and response timestamp such that mock's timestamps lies between the test's timestamp and then, set the TCS Mocks.
		filteredTcsMocks, _ := cfg.YamlStore.ReadTcsMocks(tc, filepath.Join(cfg.Path, cfg.TestSet))
		readTcsMocks := []*models.Mock{}
		for _, mock := range filteredTcsMocks {
			tcsmock, ok := mock.(*models.Mock)
			if !ok {
				continue
			}
			readTcsMocks = append(readTcsMocks, tcsmock)
		}
		readTcsMocks = FilterTcsMocks(tc, readTcsMocks, t.logger)
		loadedHooks.SetTcsMocks(readTcsMocks)
		if tc.Version == "api.keploy-enterprise.io/v1beta1" {
			entTcs = append(entTcs, tc.Name)
		} else if tc.Version != "api.keploy.io/v1beta1" && tc.Version != "api.keploy.io/v1beta2" {
			nonKeployTcs = append(nonKeployTcs, tc.Name)
		}
		

	}
	
	
	status = models.TestRunStatusPassed
	return status
}

func (t *tester) InitialiseRunMockAssert(cfg *RunTestSetConfig) InitialiseRunTestSetReturn {
	var returnVal InitialiseRunTestSetReturn
	var err error
	var readTcs []*models.TestCase
	tcsMocks, err := cfg.YamlStore.ReadTestcase(filepath.Join(cfg.Path, cfg.TestSet, "tests"), nil, nil)
	for _, mock := range tcsMocks {
		tcs, ok := mock.(*models.TestCase)
		if !ok {
			continue
		}
		readTcs = append(readTcs, tcs)
	}
	if err != nil {
		t.logger.Error("Error in reading the testcase", zap.Error(err))
		returnVal.InitialStatus = models.TestRunStatusFailed
		return returnVal
	}
	returnVal.Tcs = readTcs


	var readConfigMocks []*models.Mock
	configMocks, err := cfg.YamlStore.ReadConfigMocks(filepath.Join(cfg.Path, cfg.TestSet))
	for _, mock := range configMocks {
		configMock, ok := mock.(*models.Mock)
		if !ok {
			continue
		}
		readConfigMocks = append(readConfigMocks, configMock)
	}
	
	var readTcsMocks []*models.Mock
	readTcsMockss, err := cfg.YamlStore.ReadTcsMocks(nil, filepath.Join(cfg.Path, cfg.TestSet))
	for _, mock := range readTcsMockss {
		configMock, ok := mock.(*models.Mock)
		if !ok {
			continue
		}
		readTcsMocks = append(readTcsMocks, configMock)
	}
	if err != nil {
		t.logger.Error(err.Error())
		returnVal.InitialStatus = models.TestRunStatusFailed
		return returnVal
	}
	t.logger.Debug(fmt.Sprintf("the config mocks for %s are: %v\nthe testcase mocks are: %v", cfg.TestSet, configMocks, returnVal.TcsMocks))
	cfg.LoadedHooks.SetConfigMocks(readConfigMocks)
	cfg.LoadedHooks.SetTcsMocks(readTcsMocks)
	returnVal.ErrChan = make(chan error, 1)
	t.logger.Debug("", zap.Any("app pid", cfg.Pid))

	if len(cfg.AppCmd) == 0 && cfg.Pid != 0 {
		t.logger.Debug("running keploy tests along with other unit tests")
	} else {
		t.logger.Info("running user application for", zap.Any("test-set", models.HighlightString(cfg.TestSet)))
		// start user application
		if !cfg.ServeTest {
			go func() {
				if err := cfg.LoadedHooks.LaunchUserApplication(cfg.AppCmd, cfg.AppContainer, cfg.AppNetwork, cfg.Delay, cfg.BuildDelay, false); err != nil {
					switch err {
					case hooks.ErrInterrupted:
						t.logger.Info("keploy terminated user application")
					case hooks.ErrCommandError:
					case hooks.ErrUnExpected:
						t.logger.Warn("user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is expected")
					default:
						t.logger.Error("unknown error recieved from application", zap.Error(err))
					}
					returnVal.ErrChan <- err
				}
			}()
		}
	}

	
	// //check if the user application is running docker container using IDE
	// returnVal.DockerID = (cfg.AppCmd == "" && len(cfg.AppContainer) != 0)

	// ok, _ := cfg.LoadedHooks.IsDockerRelatedCmd(cfg.AppCmd)
	// if ok || returnVal.DockerID {
	// 	returnVal.UserIP = cfg.LoadedHooks.GetUserIP()
	// 	t.logger.Debug("the userip of the user docker container", zap.Any("", returnVal.UserIP))
	// 	t.logger.Debug("", zap.Any("User Ip", returnVal.UserIP))
	// }

	// t.logger.Info("", zap.Any("no of test cases", len(returnVal.Tcs)), zap.Any("test-set", cfg.TestSet))
	// t.logger.Debug(fmt.Sprintf("the delay is %v", time.Duration(time.Duration(cfg.Delay)*time.Second)))
	// t.logger.Debug(fmt.Sprintf("the buildDelay is %v", time.Duration(time.Duration(cfg.BuildDelay)*time.Second)))

	// // added delay to hold running keploy tests until application starts
	// t.logger.Debug("the number of testcases for the test set", zap.Any("count", len(returnVal.Tcs)), zap.Any("test-set", cfg.TestSet))
	time.Sleep(time.Duration(cfg.Delay) * time.Second)
	return returnVal
}
