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
func (t *tester) RunMockAssert(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, buildDelay time.Duration, pid uint32, ys platform.TestCaseDB, loadedHooks *hooks.Hook, testReportFS platform.TestReportDB, testRunChan chan string, apiTimeout uint64, ctx context.Context, testcases map[string]bool, noiseConfig models.GlobalNoise, serveTest bool, baseUrl string) models.TestRunStatus {
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

	return models.TestRunStatusPassed
}

func (t *tester) InitialiseRunMockAssert(cfg *RunTestSetConfig) InitialiseRunTestSetReturn {
	var returnVal InitialiseRunTestSetReturn
	var err error
	var readConfigMocks []*models.Mock
	configMocks, err := cfg.YamlStore.ReadConfigMocks(filepath.Join(cfg.Path, cfg.TestSet))
	if err != nil {
		t.logger.Error(err.Error())
		returnVal.InitialStatus = models.TestRunStatusFailed
		return returnVal
	}
	for _, mock := range configMocks {
		configMock, ok := mock.(*models.Mock)
		if !ok {
			continue
		}
		readConfigMocks = append(readConfigMocks, configMock)
	}

	var readTcsMocks []*models.Mock
	tcsMocks, err := cfg.YamlStore.ReadTcsMocks(nil, filepath.Join(cfg.Path, cfg.TestSet))

	if err != nil {
		t.logger.Error(err.Error())
		returnVal.InitialStatus = models.TestRunStatusFailed
		return returnVal
	}
	for _, mock := range tcsMocks {
		tcsMock, ok := mock.(*models.Mock)
		if !ok {
			continue
		}
		readTcsMocks = append(readTcsMocks, tcsMock)
	}
	tcsMocks, err = cfg.YamlStore.ReadResourceVersionMocks(filepath.Join(cfg.Path, cfg.TestSet))
	if err != nil {
		t.logger.Error(err.Error())
		returnVal.InitialStatus = models.TestRunStatusFailed
		return returnVal
	}
	var chunkedTime []int64
	var minTime int64
	for _, mock := range tcsMocks {
		resourceVersionMock, ok := mock.(*models.Mock)
		if !ok {
			continue
		}
		if resourceVersionMock.Spec.Metadata["chunkedLength"] != "" {
			chunkedTime = pkg.GetChunkTime(t.logger, resourceVersionMock.Spec.Metadata["chunkedTime"])
		}
		readTcsMocks = append(readTcsMocks, resourceVersionMock)
	}

	for _, chunktime := range chunkedTime {
		if chunktime < minTime {
			minTime = chunktime
		}
	}

	// Calculate average
	var sleepTime time.Duration
	if cfg.Delay > 0 && len(chunkedTime) > 0 {
		if minTime/int64(len(chunkedTime)) < int64(cfg.Delay) {
			t.logger.Error(fmt.Sprintf("Replay session duration provided is too long. Session duration replay can maximum be %ds", minTime/(int64(len(chunkedTime)*1000))))
			return returnVal
		} else {
			sleepTime = time.Duration(cfg.Delay / uint64(len(chunkedTime)))
		}
	} else if len(chunkedTime) > 0 {
		sleepTime = time.Duration(minTime / int64(len(chunkedTime)))
	} else {
		sleepTime = 10
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

	time.Sleep(time.Duration(sleepTime) * time.Second)
	return returnVal
}
