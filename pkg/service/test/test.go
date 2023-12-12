package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"net/url"

	"github.com/k0kubun/pp/v3"
	"github.com/wI2L/jsondiff"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/pkg/platform/fs"
	"go.keploy.io/server/pkg/platform/telemetry"
	"go.keploy.io/server/pkg/platform/yaml"
	"go.keploy.io/server/pkg/proxy"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"

type tester struct {
	logger *zap.Logger
	mutex  sync.Mutex
}
type TestOptions struct {
	MongoPassword      string
	Delay              uint64
	BuildDelay         time.Duration
	PassThroughPorts   []uint
	ApiTimeout         uint64
	Tests              map[string][]string
	AppContainer       string
	AppNetwork         string
	ProxyPort          uint32
	GlobalNoise        models.GlobalNoise
	TestsetNoise       models.TestsetNoise
	WithCoverage       bool
	CoverageReportPath string
}

func NewTester(logger *zap.Logger) Tester {
	return &tester{
		logger: logger,
		mutex:  sync.Mutex{},
	}
}

func (t *tester) InitialiseTest(cfg *TestConfig) (InitialiseTestReturn, error) {
	var returnVal InitialiseTestReturn

	// capturing the code coverage for go bianries built by go-version 1.20^
	if cfg.WithCoverage {

		// report path is provided via cmd flag by user
		if cfg.CoverageReportPath != "" {

			// handle relative path
			if !strings.HasPrefix(cfg.CoverageReportPath, "/") {
				absPath, err := filepath.Abs(cfg.CoverageReportPath)
				if err != nil {
					t.logger.Error("failed to resolve the relative path for go coverage directory", zap.Error(err), zap.Any("relative path", cfg.CoverageReportPath))
				}
				cfg.CoverageReportPath = absPath
			}
			cfg.CoverageReportPath = cfg.CoverageReportPath + "/coverage-reports"

			// validate the path is to directory or not. And create a directory if not exists
			dirInfo, err := os.Stat(cfg.CoverageReportPath)
			if err != nil && !os.IsNotExist(err) {
				t.logger.Error("failed to check that the goCoverDir path is a directory", zap.Error(err))
				return returnVal, err
			} else if err == nil && !dirInfo.IsDir() {
				t.logger.Error("the goCoverDir is not a directory. Please provide a valid path to a directory for go coverage binaries.")
				return returnVal, fmt.Errorf("the goCoverDir is not a directory. Please provide a valid path to a directory for go coverage binaries.")
			} else if err != nil && os.IsNotExist(err) {
				err := makeDirectory(cfg.CoverageReportPath)
				if err != nil {
					t.logger.Error("failed to create coverage directory to collect the go coverage", zap.Error(err), zap.Any("path", cfg.CoverageReportPath))
					return returnVal, err
				}
			}
		} else {
			// reports at the current directory
			cfg.CoverageReportPath = cfg.Path + "/coverage-reports"
			err := makeDirectory(cfg.CoverageReportPath)
			if err != nil {
				t.logger.Error("failed to create coverage directory to collect the go coverage", zap.Error(err), zap.Any("path", cfg.CoverageReportPath))
				return returnVal, err
			}
		}
		// set the go env variable to export the coverage-path of the runnable binaries
		os.Setenv("GOCOVERDIR", cfg.CoverageReportPath)
	}

	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGKILL)

	models.SetMode(models.MODE_TEST)

	teleFS := fs.NewTeleFS(t.logger)
	tele := telemetry.NewTelemetry(true, false, teleFS, t.logger, "", nil)

	returnVal.TestReportFS = yaml.NewTestReportFS(t.logger)
	// fetch the recorded testcases with their mocks
	returnVal.YamlStore = yaml.NewYamlStore(cfg.Path+"/tests", cfg.Path, "", "", t.logger, tele)

	routineId := pkg.GenerateRandomID()
	// Initiate the hooks
	returnVal.LoadedHooks = hooks.NewHook(returnVal.YamlStore, routineId, t.logger)

	select {
	case <-stopper:
		return returnVal, errors.New("Keploy was interupted by stopper")
	default:
		// load the ebpf hooks into the kernel
		if err := returnVal.LoadedHooks.LoadHooks(cfg.AppCmd, cfg.AppContainer, 0, context.Background(), nil); err != nil {
			return returnVal, err
		}
	}

	select {
	case <-stopper:
		returnVal.LoadedHooks.Stop(true)
		return returnVal, errors.New("Keploy was interupted by stopper")
	default:
		// start the proxy
		returnVal.ProxySet = proxy.BootProxy(t.logger, proxy.Option{Port: cfg.Proxyport, MongoPassword: cfg.MongoPassword}, cfg.AppCmd, cfg.AppContainer, 0, "", cfg.PassThroughPorts, returnVal.LoadedHooks, context.Background())
	}

	// proxy update its state in the ProxyPorts map
	//Sending Proxy Ip & Port to the ebpf program
	if err := returnVal.LoadedHooks.SendProxyInfo(returnVal.ProxySet.IP4, returnVal.ProxySet.Port, returnVal.ProxySet.IP6); err != nil {
		return returnVal, err
	}

	// filter the required destination ports
	if err := returnVal.LoadedHooks.SendPassThroughPorts(cfg.PassThroughPorts); err != nil {
		return returnVal, err
	}

	sessions, err := yaml.ReadSessionIndices(cfg.Path, t.logger)
	if err != nil {
		t.logger.Debug("failed to read the recorded sessions", zap.Error(err))
		return returnVal, err
	}
	t.logger.Debug(fmt.Sprintf("the session indices are:%v", sessions))
	returnVal.Sessions = sessions

	// Channels to communicate between different types of closing keploy
	returnVal.AbortStopHooksInterrupt = make(chan bool) // channel to stop closing of keploy via interrupt
	returnVal.AbortStopHooksForcefully = false          // boolen to stop closing of keploy via user app error
	returnVal.ExitCmd = make(chan bool)                 // channel to exit this command
	resultForTele := []int{0, 0}
	returnVal.Ctx = context.WithValue(context.Background(), "resultForTele", &resultForTele)

	go func() {
		select {
		case <-stopper:
			returnVal.AbortStopHooksForcefully = true
			returnVal.LoadedHooks.Stop(false)
			//Call the telemetry events.
			if resultForTele[0] != 0 || resultForTele[1] != 0 {
				tele.Testrun(resultForTele[0], resultForTele[1])
			}
			returnVal.ProxySet.StopProxyServer()
			returnVal.ExitCmd <- true
		case <-returnVal.AbortStopHooksInterrupt:
			//Call the telemetry events.
			if resultForTele[0] != 0 || resultForTele[1] != 0 {
				tele.Testrun(resultForTele[0], resultForTele[1])
			}
			return
		}
	}()

	return returnVal, nil
}

func (t *tester) Test(path string, testReportPath string, appCmd string, options TestOptions) bool {

	testRes := false
	result := true
	exitLoop := false

	cfg := &TestConfig{
		Path:               path,
		Proxyport:          options.ProxyPort,
		TestReportPath:     testReportPath,
		AppCmd:             appCmd,
		AppContainer:       options.AppContainer,
		AppNetwork:         options.AppContainer,
		Delay:              options.Delay,
		BuildDelay:         options.BuildDelay,
		PassThroughPorts:   options.PassThroughPorts,
		ApiTimeout:         options.ApiTimeout,
		MongoPassword:      options.MongoPassword,
		WithCoverage:       options.WithCoverage,
		CoverageReportPath: options.CoverageReportPath,
	}
	initialisedValues, err := t.InitialiseTest(cfg)
	// Recover from panic and gracfully shutdown
	defer initialisedValues.LoadedHooks.Recover(pkg.GenerateRandomID())
	if err != nil {
		t.logger.Error("failed to initialise the test", zap.Error(err))
		return false
	}
	for _, sessionIndex := range initialisedValues.Sessions {
		// checking whether the provided testset match with a recorded testset.
		testcases := ArrayToMap(options.Tests[sessionIndex])
		if _, ok := options.Tests[sessionIndex]; !ok && len(options.Tests) != 0 {
			continue
		}
		noiseConfig := options.GlobalNoise
		if tsNoise, ok := options.TestsetNoise[sessionIndex]; ok {
			noiseConfig = LeftJoinNoise(options.GlobalNoise, tsNoise)
		}

		testRunStatus := t.RunTestSet(sessionIndex, path, testReportPath, appCmd, options.AppContainer, options.AppNetwork, options.Delay, options.BuildDelay, 0, initialisedValues.YamlStore, initialisedValues.LoadedHooks, initialisedValues.TestReportFS, nil, options.ApiTimeout, initialisedValues.Ctx, testcases, noiseConfig, false)

		switch testRunStatus {
		case models.TestRunStatusAppHalted:
			testRes = false
			exitLoop = true
		case models.TestRunStatusFaultUserApp:
			testRes = false
			exitLoop = true
		case models.TestRunStatusUserAbort:
			return false
		case models.TestRunStatusFailed:
			testRes = false
		case models.TestRunStatusPassed:
			testRes = true
		}
		result = result && testRes
		if exitLoop {
			break
		}
	}
	t.logger.Info("test run completed", zap.Bool("passed overall", result))
	// log the overall code coverage for the test run of go binaries
	if options.WithCoverage {
		t.logger.Info("there is a opportunity to get the coverage here")
		// logs the coverage using covdata
		coverCmd := exec.Command("go", "tool", "covdata", "percent", "-i="+os.Getenv("GOCOVERDIR"))
		output, err := coverCmd.Output()
		if err != nil {
			t.logger.Error("failed to get the coverage of the go binary", zap.Error(err), zap.Any("cmd", coverCmd.String()))
		}
		t.logger.Sugar().Infoln("\n", models.HighlightPassingString(string(output)))

		// merges the coverage files into a single txt file which can be merged with the go-test coverage
		generateCovTxtCmd := exec.Command("go", "tool", "covdata", "textfmt", "-i="+os.Getenv("GOCOVERDIR"), "-o="+os.Getenv("GOCOVERDIR")+"/total-coverage.txt")
		output, err = generateCovTxtCmd.Output()
		if err != nil {
			t.logger.Error("failed to get the coverage of the go binary", zap.Error(err), zap.Any("cmd", coverCmd.String()))
		}
		if len(output) > 0 {
			t.logger.Sugar().Infoln("\n", models.HighlightFailingString(string(output)))
		}
	}

	if !initialisedValues.AbortStopHooksForcefully {
		initialisedValues.AbortStopHooksInterrupt <- true
		// stop listening for the eBPF events
		initialisedValues.LoadedHooks.Stop(true)
		//stop listening for proxy server
		initialisedValues.ProxySet.StopProxyServer()
		return true
	}

	<-initialisedValues.ExitCmd
	return false
}

func (t *tester) InitialiseRunTestSet(cfg *RunTestSetConfig) InitialiseRunTestSetReturn {
	var returnVal InitialiseRunTestSetReturn
	var err error
	returnVal.Tcs, err = cfg.YamlStore.ReadTestcase(filepath.Join(cfg.Path, cfg.TestSet, "tests"), nil)
	if err != nil {
		t.logger.Error("Error in reading the testcase", zap.Error(err))
		returnVal.InitialStatus = models.TestRunStatusFailed
		return returnVal
	}
	if len(returnVal.Tcs) == 0 {
		t.logger.Info("No testcases are recorded for the user application", zap.Any("for session", cfg.TestSet))
		returnVal.InitialStatus = models.TestRunStatusFailed
		return returnVal
	}

	t.logger.Debug(fmt.Sprintf("the testcases for %s are: %v", cfg.TestSet, returnVal.Tcs))
	var configMocks []*models.Mock
	configMocks, returnVal.TcsMocks, err = cfg.YamlStore.ReadMocks(filepath.Join(cfg.Path, cfg.TestSet))
	if err != nil {
		t.logger.Error(err.Error())
		returnVal.InitialStatus = models.TestRunStatusFailed
		return returnVal
	}
	t.logger.Debug(fmt.Sprintf("the config mocks for %s are: %v\nthe testcase mocks are: %v", cfg.TestSet, configMocks, returnVal.TcsMocks))
	cfg.LoadedHooks.SetConfigMocks(configMocks)
	cfg.LoadedHooks.SetTcsMocks(returnVal.TcsMocks)
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
	// testReport stores the result of all testruns
	returnVal.TestReport = &models.TestReport{
		Version: models.GetVersion(),
		// Name:    runId,
		Total:  len(returnVal.Tcs),
		Status: string(models.TestRunStatusRunning),
	}

	// starts the testrun
	err = cfg.TestReportFS.Write(context.Background(), cfg.TestReportPath, returnVal.TestReport)
	if err != nil {
		t.logger.Error(err.Error())
		returnVal.InitialStatus = models.TestRunStatusFailed
		return returnVal
	}

	//if running keploy-tests along with unit tests
	if cfg.ServeTest && cfg.TestRunChan != nil {
		cfg.TestRunChan <- returnVal.TestReport.Name
	}

	//check if the user application is running docker container using IDE
	returnVal.DockerID = (cfg.AppCmd == "" && len(cfg.AppContainer) != 0)

	ok, _ := cfg.LoadedHooks.IsDockerRelatedCmd(cfg.AppCmd)
	if ok || returnVal.DockerID {
		returnVal.UserIP = cfg.LoadedHooks.GetUserIP()
		t.logger.Debug("the userip of the user docker container", zap.Any("", returnVal.UserIP))
		t.logger.Debug("", zap.Any("User Ip", returnVal.UserIP))
	}

	t.logger.Info("", zap.Any("no of test cases", len(returnVal.Tcs)), zap.Any("test-set", cfg.TestSet))
	t.logger.Debug(fmt.Sprintf("the delay is %v", time.Duration(time.Duration(cfg.Delay)*time.Second)))
	t.logger.Debug(fmt.Sprintf("the buildDelay is %v", time.Duration(time.Duration(cfg.BuildDelay)*time.Second)))

	// added delay to hold running keploy tests until application starts
	t.logger.Debug("the number of testcases for the test set", zap.Any("count", len(returnVal.Tcs)), zap.Any("test-set", cfg.TestSet))
	time.Sleep(time.Duration(cfg.Delay) * time.Second)
	return returnVal
}

func (t *tester) SimulateRequest(cfg *SimulateRequestConfig) {
	switch cfg.Tc.Kind {
	case models.HTTP:
		started := time.Now().UTC()
		t.logger.Debug("Before simulating the request", zap.Any("Test case", cfg.Tc))

		ok, _ := cfg.LoadedHooks.IsDockerRelatedCmd(cfg.AppCmd)
		if ok || cfg.DockerID {
			var err error
			cfg.Tc.HttpReq.URL, err = replaceHostToIP(cfg.Tc.HttpReq.URL, cfg.UserIP)
			if err != nil {
				t.logger.Error("failed to replace host to docker container's IP", zap.Error(err))
			}
			t.logger.Debug("", zap.Any("replaced URL in case of docker env", cfg.Tc.HttpReq.URL))
		}
		t.logger.Debug(fmt.Sprintf("the url of the testcase: %v", cfg.Tc.HttpReq.URL))
		resp, err := pkg.SimulateHttp(*cfg.Tc, cfg.TestSet, t.logger, cfg.ApiTimeout)
		t.logger.Debug("After simulating the request", zap.Any("test case id", cfg.Tc.Name))
		t.logger.Debug("After GetResp of the request", zap.Any("test case id", cfg.Tc.Name))

		if err != nil {
			t.logger.Info("result", zap.Any("testcase id", models.HighlightFailingString(cfg.Tc.Name)), zap.Any("testset id", models.HighlightFailingString(cfg.TestSet)), zap.Any("passed", models.HighlightFailingString("false")))
			return
		}
		testPass, testResult := t.testHttp(*cfg.Tc, resp, cfg.NoiseConfig)

		if !testPass {
			t.logger.Info("result", zap.Any("testcase id", models.HighlightFailingString(cfg.Tc.Name)), zap.Any("testset id", models.HighlightFailingString(cfg.TestSet)), zap.Any("passed", models.HighlightFailingString(testPass)))
		} else {
			t.logger.Info("result", zap.Any("testcase id", models.HighlightPassingString(cfg.Tc.Name)), zap.Any("testset id", models.HighlightPassingString(cfg.TestSet)), zap.Any("passed", models.HighlightPassingString(testPass)))
		}

		testStatus := models.TestStatusPending
		if testPass {
			testStatus = models.TestStatusPassed
			*cfg.Success++
		} else {
			testStatus = models.TestStatusFailed
			*cfg.Failure++
			*cfg.Status = models.TestRunStatusFailed
		}

		cfg.TestReportFS.Lock()
		cfg.TestReportFS.SetResult(cfg.TestReport.Name, models.TestResult{
			Kind:       models.HTTP,
			Name:       cfg.TestReport.Name,
			Status:     testStatus,
			Started:    started.Unix(),
			Completed:  time.Now().UTC().Unix(),
			TestCaseID: cfg.Tc.Name,
			Req: models.HttpReq{
				Method:     cfg.Tc.HttpReq.Method,
				ProtoMajor: cfg.Tc.HttpReq.ProtoMajor,
				ProtoMinor: cfg.Tc.HttpReq.ProtoMinor,
				URL:        cfg.Tc.HttpReq.URL,
				URLParams:  cfg.Tc.HttpReq.URLParams,
				Header:     cfg.Tc.HttpReq.Header,
				Body:       cfg.Tc.HttpReq.Body,
			},
			Res: models.HttpResp{
				StatusCode:    cfg.Tc.HttpResp.StatusCode,
				Header:        cfg.Tc.HttpResp.Header,
				Body:          cfg.Tc.HttpResp.Body,
				StatusMessage: cfg.Tc.HttpResp.StatusMessage,
				ProtoMajor:    cfg.Tc.HttpResp.ProtoMajor,
				ProtoMinor:    cfg.Tc.HttpResp.ProtoMinor,
			},
			// Mocks:        httpSpec.Mocks,
			// TestCasePath: tcsPath,
			TestCasePath: cfg.Path + "/" + cfg.TestSet,
			// MockPath:     mockPath,
			// Noise:        httpSpec.Assertions["noise"],
			Noise:  cfg.Tc.Noise,
			Result: *testResult,
		})
		cfg.TestReportFS.Lock()
		cfg.TestReportFS.Unlock()

	}
}

func (t *tester) FetchTestResults(cfg *FetchTestResultsConfig) models.TestRunStatus {
	// store the result of the testrun as test-report
	testResults, err := cfg.TestReportFS.GetResults(cfg.TestReport.Name)
	if err != nil && (*cfg.Status == models.TestRunStatusFailed || *cfg.Status == models.TestRunStatusPassed) && (*cfg.Success+*cfg.Failure == 0) {
		t.logger.Error("failed to fetch test results", zap.Error(err))
		return models.TestRunStatusFailed
	}
	cfg.TestReport.TestSet = cfg.TestSet
	cfg.TestReport.Total = len(testResults)
	cfg.TestReport.Status = string(*cfg.Status)
	cfg.TestReport.Tests = testResults
	cfg.TestReport.Success = *cfg.Success
	cfg.TestReport.Failure = *cfg.Failure

	resultForTele, ok := cfg.Ctx.Value("resultForTele").(*[]int)
	if !ok {
		t.logger.Debug("resultForTele is not of type *[]int")
	}
	(*resultForTele)[0] += *cfg.Success
	(*resultForTele)[1] += *cfg.Failure

	err = cfg.TestReportFS.Write(context.Background(), cfg.TestReportPath, cfg.TestReport)

	t.logger.Info("test report for "+cfg.TestSet+": ", zap.Any("name: ", cfg.TestReport.Name), zap.Any("path: ", cfg.Path+"/"+cfg.TestReport.Name))

	if *cfg.Status == models.TestRunStatusFailed {
		pp.SetColorScheme(models.FailingColorScheme)
	} else {
		pp.SetColorScheme(models.PassingColorScheme)
	}

	pp.Printf("\n <=========================================> \n  TESTRUN SUMMARY. For testrun with id: %s\n"+"\tTotal tests: %s\n"+"\tTotal test passed: %s\n"+"\tTotal test failed: %s\n <=========================================> \n\n", cfg.TestReport.TestSet, cfg.TestReport.Total, cfg.TestReport.Success, cfg.TestReport.Failure)

	if err != nil {
		t.logger.Error(err.Error())
		return models.TestRunStatusFailed
	}

	t.logger.Debug("the result before", zap.Any("", cfg.TestReport.Status), zap.Any("testreport name", cfg.TestReport.Name))
	t.logger.Debug("the result after", zap.Any("", cfg.TestReport.Status), zap.Any("testreport name", cfg.TestReport.Name))
	return *cfg.Status
}

// testSet, path, testReportPath, appCmd, appContainer, appNetwork, delay, pid, ys, loadedHooks, testReportFS, testRunChan, apiTimeout, ctx
func (t *tester) RunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, buildDelay time.Duration, pid uint32, ys platform.TestCaseDB, loadedHooks *hooks.Hook, testReportFS yaml.TestReportFS, testRunChan chan string, apiTimeout uint64, ctx context.Context, testcases map[string]bool, noiseConfig models.GlobalNoise, serveTest bool) models.TestRunStatus {
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
	initialisedValues := t.InitialiseRunTestSet(cfg)
	if initialisedValues.InitialStatus != "" {
		return initialisedValues.InitialStatus
	}

	isApplicationStopped := false
	// Recover from panic and gracfully shutdown
	defer loadedHooks.Recover(pkg.GenerateRandomID())
	defer func() {
		if len(appCmd) == 0 && pid != 0 {
			t.logger.Debug("no need to stop the user application when running keploy tests along with unit tests")
		} else {
			// stop the user application
			if !isApplicationStopped && !serveTest {
				loadedHooks.StopUserApplication()
			}
		}
	}()

	exitLoop := false
	var (
		success = 0
		failure = 0
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
		filteredTcsMocks := FilterTcsMocks(tc, initialisedValues.TcsMocks, t.logger)
		loadedHooks.SetTcsMocks(filteredTcsMocks)

		if tc.Version == "api.keploy-enterprise.io/v1beta1" {
			entTcs = append(entTcs, tc.Name)
		} else if tc.Version != "api.keploy.io/v1beta1" && tc.Version != "api.keploy.io/v1beta2" {
			nonKeployTcs = append(nonKeployTcs, tc.Name)
		}
		select {
		case err := <-initialisedValues.ErrChan:
			isApplicationStopped = true
			switch err {
			case hooks.ErrInterrupted:
				exitLoop = true
				status = models.TestRunStatusUserAbort
			case hooks.ErrCommandError:
				exitLoop = true
				status = models.TestRunStatusFaultUserApp
			case hooks.ErrUnExpected:
				exitLoop = true
				status = models.TestRunStatusAppHalted
				t.logger.Warn("stopping testrun for the test set:", zap.Any("test-set", testSet))
			default:
				exitLoop = true
				status = models.TestRunStatusAppHalted
				t.logger.Error("stopping testrun for the test set:", zap.Any("test-set", testSet))
			}
		default:
		}

		if exitLoop {
			break
		}

		cfg := &SimulateRequestConfig{
			Tc:           tc,
			LoadedHooks:  loadedHooks,
			AppCmd:       appCmd,
			UserIP:       userIp,
			TestSet:      testSet,
			ApiTimeout:   apiTimeout,
			Success:      &success,
			Failure:      &failure,
			Status:       &status,
			TestReportFS: testReportFS,
			TestReport:   initialisedValues.TestReport,
			Path:         path,
			DockerID:     initialisedValues.DockerID,
			NoiseConfig:  noiseConfig,
		}
		t.SimulateRequest(cfg)
	}
	if len(entTcs) > 0 {
		t.logger.Warn("These testcases have been recorded with Keploy Enterprise, may not work properly with the open-source version", zap.Strings("enterprise mocks:", entTcs))
	}
	if len(nonKeployTcs) > 0 {
		t.logger.Warn("These testcases have not been recorded by Keploy, may not work properly with Keploy.", zap.Strings("non-keploy mocks:", nonKeployTcs))
	}
	resultsCfg := &FetchTestResultsConfig{
		TestReportFS:   testReportFS,
		TestReport:     initialisedValues.TestReport,
		Status:         &status,
		TestSet:        testSet,
		Success:        &success,
		Failure:        &failure,
		Ctx:            ctx,
		TestReportPath: testReportPath,
		Path:           path,
	}
	status = t.FetchTestResults(resultsCfg)
	return status
}

func (t *tester) testHttp(tc models.TestCase, actualResponse *models.HttpResp, noiseConfig models.GlobalNoise) (bool, *models.Result) {

	bodyType := models.BodyTypePlain
	if json.Valid([]byte(actualResponse.Body)) {
		bodyType = models.BodyTypeJSON
	}
	pass := true
	hRes := &[]models.HeaderResult{}

	res := &models.Result{
		StatusCode: models.IntResult{
			Normal:   false,
			Expected: tc.HttpResp.StatusCode,
			Actual:   actualResponse.StatusCode,
		},
		BodyResult: []models.BodyResult{{
			Normal:   false,
			Type:     bodyType,
			Expected: tc.HttpResp.Body,
			Actual:   actualResponse.Body,
		}},
	}
	noise := tc.Noise

	var (
		bodyNoise   = noiseConfig["body"]
		headerNoise = noiseConfig["header"]
	)

	if bodyNoise == nil {
		bodyNoise = map[string][]string{}
	}
	if headerNoise == nil {
		headerNoise = map[string][]string{}
	}

	for field, regexArr := range noise {
		a := strings.Split(field, ".")
		if len(a) > 1 && a[0] == "body" {
			x := strings.Join(a[1:], ".")
			bodyNoise[x] = regexArr
		} else if a[0] == "header" {
			headerNoise[a[len(a)-1]] = regexArr
		}
	}

	// stores the json body after removing the noise
	cleanExp, cleanAct := "", ""
	var err error
	if !Contains(MapToArray(noise), "body") && bodyType == models.BodyTypeJSON {
		cleanExp, cleanAct, pass, err = Match(tc.HttpResp.Body, actualResponse.Body, bodyNoise, t.logger)
		if err != nil {
			return false, res
		}
		// debug log for cleanExp and cleanAct
		t.logger.Debug("cleanExp", zap.Any("", cleanExp))
		t.logger.Debug("cleanAct", zap.Any("", cleanAct))
	} else {
		if !Contains(MapToArray(noise), "body") && tc.HttpResp.Body != actualResponse.Body {
			pass = false
		}
	}

	res.BodyResult[0].Normal = pass

	if !CompareHeaders(pkg.ToHttpHeader(tc.HttpResp.Header), pkg.ToHttpHeader(actualResponse.Header), hRes, headerNoise) {

		pass = false
	}

	res.HeadersResult = *hRes
	if tc.HttpResp.StatusCode == actualResponse.StatusCode {
		res.StatusCode.Normal = true
	} else {

		pass = false
	}

	if !pass {
		logDiffs := NewDiffsPrinter(tc.Name)

		logger := pp.New()
		logger.WithLineInfo = false
		logger.SetColorScheme(models.FailingColorScheme)
		var logs = ""

		logs = logs + logger.Sprintf("Testrun failed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.Name)

		// ------------ DIFFS RELATED CODE -----------
		if !res.StatusCode.Normal {
			logDiffs.PushStatusDiff(fmt.Sprint(res.StatusCode.Expected), fmt.Sprint(res.StatusCode.Actual))
		}

		var (
			actualHeader   = map[string][]string{}
			expectedHeader = map[string][]string{}
			unmatched      = true
		)

		for _, j := range res.HeadersResult {
			if !j.Normal {
				unmatched = false
				actualHeader[j.Actual.Key] = j.Actual.Value
				expectedHeader[j.Expected.Key] = j.Expected.Value
			}
		}

		if !unmatched {
			for i, j := range expectedHeader {
				logDiffs.PushHeaderDiff(fmt.Sprint(j), fmt.Sprint(actualHeader[i]), i, headerNoise)
			}
		}

		if !res.BodyResult[0].Normal {

			if json.Valid([]byte(actualResponse.Body)) {
				patch, err := jsondiff.Compare(cleanExp, cleanAct)
				if err != nil {
					t.logger.Warn("failed to compute json diff", zap.Error(err))
				}
				for _, op := range patch {
					keyStr := op.Path
					if len(keyStr) > 1 && keyStr[0] == '/' {
						keyStr = keyStr[1:]
					}
					logDiffs.PushBodyDiff(fmt.Sprint(op.OldValue), fmt.Sprint(op.Value), bodyNoise)

				}
			} else {
				logDiffs.PushBodyDiff(fmt.Sprint(tc.HttpResp.Body), fmt.Sprint(actualResponse.Body), bodyNoise)
			}
		}
		t.mutex.Lock()
		logger.Printf(logs)
		logDiffs.Render()
		t.mutex.Unlock()

	} else {
		logger := pp.New()
		logger.WithLineInfo = false
		logger.SetColorScheme(models.PassingColorScheme)
		var log2 = ""
		log2 += logger.Sprintf("Testrun passed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.Name)
		t.mutex.Lock()
		logger.Printf(log2)
		t.mutex.Unlock()

	}

	return pass, res
}

func replaceHostToIP(currentURL string, ipAddress string) (string, error) {
	// Parse the current URL
	parsedURL, err := url.Parse(currentURL)

	if err != nil {
		// Return the original URL if parsing fails
		return currentURL, err
	}

	if ipAddress == "" {
		return currentURL, fmt.Errorf("failed to replace url in case of docker env")
	}

	// Replace hostname with the IP address
	parsedURL.Host = strings.Replace(parsedURL.Host, parsedURL.Hostname(), ipAddress, 1)
	// Return the modified URL
	return parsedURL.String(), nil
}
