package test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

func NewTester(logger *zap.Logger) Tester {
	return &tester{
		logger: logger,
		mutex:  sync.Mutex{},
	}
}

func (t *tester) Test(path string, proxyPort uint32, testReportPath string, appCmd string, testsets []string, appContainer, appNetwork string, Delay uint64, passThorughPorts []uint, apiTimeout uint64, noiseConfig map[string]interface{}) bool {

	var ps *proxy.ProxySet

	stopper := make(chan os.Signal, 1)
	signal.Notify(stopper, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGKILL)

	models.SetMode(models.MODE_TEST)

	teleFS := fs.NewTeleFS()
	tele := telemetry.NewTelemetry(true, false, teleFS, t.logger, "", nil)
	tele.Ping(false)

	testReportFS := yaml.NewTestReportFS(t.logger)
	// fetch the recorded testcases with their mocks
	ys := yaml.NewYamlStore(path+"/tests", path, "", "", t.logger, tele)

	routineId := pkg.GenerateRandomID()
	// Initiate the hooks
	loadedHooks := hooks.NewHook(ys, routineId, t.logger)

	// Recover from panic and gracfully shutdown
	defer loadedHooks.Recover(routineId)

	select {
	case <-stopper:
		return false
	default:
		// load the ebpf hooks into the kernel
		if err := loadedHooks.LoadHooks(appCmd, appContainer, 0, context.Background(), nil); err != nil {
			return false
		}
	}

	select {
	case <-stopper:
		loadedHooks.Stop(true)
		return false
	default:
		// start the proxy
		ps = proxy.BootProxy(t.logger, proxy.Option{Port: proxyPort}, appCmd, appContainer, 0, "", passThorughPorts, loadedHooks, context.Background())
	}

	// proxy update its state in the ProxyPorts map
	//Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP4, ps.Port, ps.IP6); err != nil {
		return false
	}

	sessions, err := yaml.ReadSessionIndices(path, t.logger)
	if err != nil {
		t.logger.Debug("failed to read the recorded sessions", zap.Error(err))
		return false
	}
	t.logger.Debug(fmt.Sprintf("the session indices are:%v", sessions))

	result := true

	// Channels to communicate between different types of closing keploy
	abortStopHooksInterrupt := make(chan bool) // channel to stop closing of keploy via interrupt
	abortStopHooksForcefully := false          // boolen to stop closing of keploy via user app error
	exitCmd := make(chan bool)                 // channel to exit this command
	resultForTele := []int{0, 0}
	ctx := context.WithValue(context.Background(), "resultForTele", &resultForTele)

	go func() {
		select {
		case <-stopper:
			abortStopHooksForcefully = true
			loadedHooks.Stop(false)
			//Call the telemetry events.
			if resultForTele[0] != 0 || resultForTele[1] != 0 {
				tele.Testrun(resultForTele[0], resultForTele[1])
			}
			ps.StopProxyServer()
			exitCmd <- true
		case <-abortStopHooksInterrupt:
			//Call the telemetry events.
			if resultForTele[0] != 0 || resultForTele[1] != 0 {
				tele.Testrun(resultForTele[0], resultForTele[1])
			}
			return
		}
	}()

	testRes := false

	exitLoop := false

	if len(testsets) == 0 {
		// by default, run all the recorded test sets
		testsets = sessions
	}

	sessionsMap := map[string]string{}

	for _, sessionIndex := range sessions {
		sessionsMap[sessionIndex] = sessionIndex
	}

	for _, sessionIndex := range testsets {
		// checking whether the provided testset match with a recorded testset.
		if _, ok := sessionsMap[sessionIndex]; !ok {
			t.logger.Info("no testset found with: ", zap.Any("name", sessionIndex))
			continue
		}
		testRunStatus := t.RunTestSet(sessionIndex, path, testReportPath, appCmd, appContainer, appNetwork, Delay, 0, ys, loadedHooks, testReportFS, nil, apiTimeout, ctx, noiseConfig, false)
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

	if !abortStopHooksForcefully {
		abortStopHooksInterrupt <- true
		// stop listening for the eBPF events
		loadedHooks.Stop(true)
		//stop listening for proxy server
		ps.StopProxyServer()
		return true
	}

	<-exitCmd
	return false
}

func (t *tester) RunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, pid uint32, ys platform.TestCaseDB, loadedHooks *hooks.Hook, testReportFS yaml.TestReportFS, testRunChan chan string, apiTimeout uint64, ctx context.Context, noiseConfig map[string]interface{}, serveTest bool) models.TestRunStatus {
	// Recover from panic and gracfully shutdown
	defer loadedHooks.Recover(pkg.GenerateRandomID())

	tcs, err := ys.ReadTestcase(filepath.Join(path, testSet, "tests"), nil)
	if err != nil {
		return models.TestRunStatusFailed
	}

	if len(tcs) == 0 {
		t.logger.Info("No testcases are recorded for the user application", zap.Any("for session", testSet))
		return models.TestRunStatusPassed
	}

	t.logger.Debug(fmt.Sprintf("the testcases for %s are: %v", testSet, tcs))

	configMocks, tcsMocks, err := ys.ReadMocks(filepath.Join(path, testSet))
	if err != nil {
		t.logger.Error(err.Error())
		return models.TestRunStatusFailed
	}

	t.logger.Debug(fmt.Sprintf("the config mocks for %s are: %v\nthe testcase mocks are: %v", testSet, configMocks, tcsMocks))
	loadedHooks.SetConfigMocks(configMocks)
	loadedHooks.SetTcsMocks(tcsMocks)

	errChan := make(chan error, 1)
	t.logger.Debug("", zap.Any("app pid", pid))

	isApplicationStopped := false

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

	if len(appCmd) == 0 && pid != 0 {
		t.logger.Debug("running keploy tests along with other unit tests")
	} else {
		t.logger.Info("running user application for", zap.Any("test-set", models.HighlightString(testSet)))
		// start user application
		if !serveTest {
			go func() {
				if err := loadedHooks.LaunchUserApplication(appCmd, appContainer, appNetwork, delay); err != nil {
					switch err {
					case hooks.ErrInterrupted:
						t.logger.Info("keploy terminated user application")
					case hooks.ErrCommandError:
					case hooks.ErrUnExpected:
						t.logger.Warn("user application terminated unexpectedly hence stopping keploy, please check application logs if this behaviour is expected")
					default:
						t.logger.Error("unknown error recieved from application", zap.Error(err))
					}
					errChan <- err
				}
			}()
		}
	}

	// testReport stores the result of all testruns
	testReport := &models.TestReport{
		Version: models.V1Beta1,
		// Name:    runId,
		Total:  len(tcs),
		Status: string(models.TestRunStatusRunning),
	}

	// starts the testrun
	err = testReportFS.Write(context.Background(), testReportPath, testReport)
	if err != nil {
		t.logger.Error(err.Error())
		return models.TestRunStatusPassed
	}

	//if running keploy-tests along with unit tests
	if serveTest && testRunChan != nil {
		testRunChan <- testReport.Name
	}

	var (
		success = 0
		failure = 0
		status  = models.TestRunStatusPassed
	)

	var userIp string

	//check if the user application is running docker container using IDE
	dIDE := (appCmd == "" && len(appContainer) != 0)

	ok, _ := loadedHooks.IsDockerRelatedCmd(appCmd)
	if ok || dIDE {
		userIp = loadedHooks.GetUserIP()
		t.logger.Debug("the userip of the user docker container", zap.Any("", userIp))
		t.logger.Debug("", zap.Any("User Ip", userIp))
	}

	t.logger.Info("", zap.Any("no of test cases", len(tcs)), zap.Any("test-set", testSet))
	t.logger.Debug(fmt.Sprintf("the delay is %v", time.Duration(time.Duration(delay)*time.Second)))

	// added delay to hold running keploy tests until application starts
	t.logger.Debug("the number of testcases for the test set", zap.Any("count", len(tcs)), zap.Any("test-set", testSet))
	time.Sleep(time.Duration(delay) * time.Second)
	exitLoop := false
	for _, tc := range tcs {
		// Filter the TCS Mocks based on the test case's request and response timestamp such that mock's timestamps lies between the test's timestamp and then, set the TCS Mocks.
		filteredTcsMocks := filterTcsMocks(tc, tcsMocks, t.logger)
		loadedHooks.SetTcsMocks(filteredTcsMocks)

		select {
		case err = <-errChan:
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
		switch tc.Kind {
		case models.HTTP:
			started := time.Now().UTC()
			t.logger.Debug("Before simulating the request", zap.Any("Test case", tc))

			ok, _ := loadedHooks.IsDockerRelatedCmd(appCmd)
			if ok || dIDE {
				tc.HttpReq.URL = replaceHostToIP(tc.HttpReq.URL, userIp)
				t.logger.Debug("", zap.Any("replaced URL in case of docker env", tc.HttpReq.URL))
			}
			t.logger.Debug(fmt.Sprintf("the url of the testcase: %v", tc.HttpReq.URL))
			resp, err := pkg.SimulateHttp(*tc, testSet, t.logger, apiTimeout)
			t.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
			t.logger.Debug("After GetResp of the request", zap.Any("test case id", tc.Name))

			if err != nil {
				t.logger.Info("result", zap.Any("testcase id", models.HighlightFailingString(tc.Name)), zap.Any("testset id", models.HighlightFailingString(testSet)), zap.Any("passed", models.HighlightFailingString("false")))
				continue
			}

			testPass, testResult := t.testHttp(*tc, resp, noiseConfig)

			if !testPass {
				t.logger.Info("result", zap.Any("testcase id", models.HighlightFailingString(tc.Name)), zap.Any("testset id", models.HighlightFailingString(testSet)), zap.Any("passed", models.HighlightFailingString(testPass)))
			} else {
				t.logger.Info("result", zap.Any("testcase id", models.HighlightPassingString(tc.Name)), zap.Any("testset id", models.HighlightPassingString(testSet)), zap.Any("passed", models.HighlightPassingString(testPass)))
			}

			testStatus := models.TestStatusPending
			if testPass {
				testStatus = models.TestStatusPassed
				success++
			} else {
				testStatus = models.TestStatusFailed
				failure++
				status = models.TestRunStatusFailed
			}

			testReportFS.Lock()
			testReportFS.SetResult(testReport.Name, models.TestResult{
				Kind:       models.HTTP,
				Name:       testReport.Name,
				Status:     testStatus,
				Started:    started.Unix(),
				Completed:  time.Now().UTC().Unix(),
				TestCaseID: tc.Name,
				Req: models.HttpReq{
					Method:     tc.HttpReq.Method,
					ProtoMajor: tc.HttpReq.ProtoMajor,
					ProtoMinor: tc.HttpReq.ProtoMinor,
					URL:        tc.HttpReq.URL,
					URLParams:  tc.HttpReq.URLParams,
					Header:     tc.HttpReq.Header,
					Body:       tc.HttpReq.Body,
				},
				Res: models.HttpResp{
					StatusCode:    tc.HttpResp.StatusCode,
					Header:        tc.HttpResp.Header,
					Body:          tc.HttpResp.Body,
					StatusMessage: tc.HttpResp.StatusMessage,
					ProtoMajor:    tc.HttpResp.ProtoMajor,
					ProtoMinor:    tc.HttpResp.ProtoMinor,
				},
				// Mocks:        httpSpec.Mocks,
				// TestCasePath: tcsPath,
				TestCasePath: path + "/" + testSet,
				// MockPath:     mockPath,
				// Noise:        httpSpec.Assertions["noise"],
				Noise:  tc.Noise,
				Result: *testResult,
			})
			testReportFS.Lock()
			testReportFS.Unlock()

		}
	}

	// store the result of the testrun as test-report
	testResults, err := testReportFS.GetResults(testReport.Name)
	if err != nil && (status == models.TestRunStatusFailed || status == models.TestRunStatusPassed) && (success+failure == 0) {
		t.logger.Error("failed to fetch test results", zap.Error(err))
		return models.TestRunStatusFailed
	}
	testReport.TestSet = testSet
	testReport.Total = len(testResults)
	testReport.Status = string(status)
	testReport.Tests = testResults
	testReport.Success = success
	testReport.Failure = failure

	resultForTele, ok := ctx.Value("resultForTele").(*[]int)
	if !ok {
		t.logger.Debug("resultForTele is not of type *[]int")
	}
	(*resultForTele)[0] += success
	(*resultForTele)[1] += failure

	err = testReportFS.Write(context.Background(), testReportPath, testReport)

	t.logger.Info("test report for "+testSet+": ", zap.Any("name: ", testReport.Name), zap.Any("path: ", path+"/"+testReport.Name))

	if status == models.TestRunStatusFailed {
		pp.SetColorScheme(models.FailingColorScheme)
	} else {
		pp.SetColorScheme(models.PassingColorScheme)
	}

	pp.Printf("\n <=========================================> \n  TESTRUN SUMMARY. For testrun with id: %s\n"+"\tTotal tests: %s\n"+"\tTotal test passed: %s\n"+"\tTotal test failed: %s\n <=========================================> \n\n", testReport.TestSet, testReport.Total, testReport.Success, testReport.Failure)

	if err != nil {
		t.logger.Error(err.Error())
		return models.TestRunStatusFailed
	}

	t.logger.Debug("the result before", zap.Any("", testReport.Status), zap.Any("testreport name", testReport.Name))
	t.logger.Debug("the result after", zap.Any("", testReport.Status), zap.Any("testreport name", testReport.Name))

	return status
}

func (t *tester) testHttp(tc models.TestCase, actualResponse *models.HttpResp, noiseConfig map[string]interface{}) (bool, *models.Result) {
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
		bodyNoise   = map[string][]string{}
		headerNoise = map[string][]string{}
	)

	for _, n := range noise {
		a := strings.Split(n, ".")
		if len(a) > 1 && a[0] == "body" {
			x := strings.Join(a[1:], ".")
			bodyNoise[x] = []string{}
		} else if a[0] == "header" {
			headerNoise[a[len(a)-1]] = []string{}
		}
	}

	for k, v := range noiseConfig {
		if k == "body" {
			for k1, v1 := range v.(map[string]interface{}) {
				bodyNoise[k1] = []string{}
				for _, v2 := range v1.([]interface{}) {
					bodyNoise[k1] = append(bodyNoise[k1], v2.(string))
				}
			}
		}
		if k == "header" {
			for k1, v1 := range v.(map[string]interface{}) {
				headerNoise[k1] = []string{}
				for _, v2 := range v1.([]interface{}) {
					headerNoise[k1] = append(headerNoise[k1], v2.(string))
				}
			}
		}
	}

	// stores the json body after removing the noise
	cleanExp, cleanAct := "", ""
	var err error
	if !Contains(noise, "body") && bodyType == models.BodyTypeJSON {
		cleanExp, cleanAct, pass, err = Match(tc.HttpResp.Body, actualResponse.Body, bodyNoise, t.logger)
		if err != nil {
			return false, res
		}
		// debug log for cleanExp and cleanAct
		t.logger.Debug("cleanExp", zap.Any("", cleanExp))
		t.logger.Debug("cleanAct", zap.Any("", cleanAct))
	} else {
		if !Contains(noise, "body") && tc.HttpResp.Body != actualResponse.Body {
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

func replaceHostToIP(currentURL string, ipAddress string) string {
	// Parse the current URL
	parsedURL, err := url.Parse(currentURL)
	if err != nil {
		// Return the original URL if parsing fails
		return currentURL
	}

	if ipAddress == "" {
		fmt.Errorf(Emoji, "failed to replace url in case of docker env")
		return currentURL
	}

	// Replace hostname with the IP address
	parsedURL.Host = strings.Replace(parsedURL.Host, parsedURL.Hostname(), ipAddress, 1)

	// Return the modified URL
	return parsedURL.String()
}
