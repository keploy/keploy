package test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"net/url"

	"github.com/k0kubun/pp/v3"
	"github.com/wI2L/jsondiff"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
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

// func (t *tester) Test(tcsPath, mockPath, testReportPath string, pid uint32) bool {
// func (t *tester) Test(tcsPath, mockPath, testReportPath string, appCmd, appContainer, appNetwork string, Delay uint64) bool {
func (t *tester) Test(path, testReportPath string, appCmd, appContainer, appNetwork string, Delay uint64, passThorughPorts []uint, apiTimeout uint64) bool {
	models.SetMode(models.MODE_TEST)

	testReportFS := yaml.NewTestReportFS(t.logger)
	// fetch the recorded testcases with their mocks
	// ys := yaml.NewYamlStore(tcsPath, mockPath, t.logger)
	ys := yaml.NewYamlStore(path+"/tests", path, "", "", t.logger)

	routineId := pkg.GenerateRandomID()
	// Initiate the hooks
	loadedHooks := hooks.NewHook(ys, routineId, t.logger)

	// Recover from panic and gracfully shutdown
	defer loadedHooks.Recover(routineId)

	// load the ebpf hooks into the kernel
	if err := loadedHooks.LoadHooks(appCmd, appContainer, 0); err != nil {
		return false
	}

	// start the proxy
	ps := proxy.BootProxy(t.logger, proxy.Option{}, appCmd, appContainer, 0, "", passThorughPorts, loadedHooks)

	// proxy update its state in the ProxyPorts map
	// ps.SetHook(loadedHooks)

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

	stopHooksAbort := make(chan bool)
	stopProxyServerAbort := make(chan bool)

	go func() {
		loadedHooks.Stop(false, stopHooksAbort)
		select {
		case <-stopProxyServerAbort:
		default:
			ps.StopProxyServer()
		}
	}()

	testRes := false

	exitLoop := false

	for _, sessionIndex := range sessions {
		testRunStatus := t.RunTestSet(sessionIndex, path, testReportPath, appCmd, appContainer, appNetwork, Delay, 0, ys, loadedHooks, testReportFS, nil, apiTimeout)
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

	// stop listening for the eBPF events
	loadedHooks.Stop(true, nil)

	//stop listening for proxy server
	ps.StopProxyServer()

	stopHooksAbort <- true
	stopProxyServerAbort <- true

	return true
}

func (t *tester) RunTestSet(testSet, path, testReportPath, appCmd, appContainer, appNetwork string, delay uint64, pid uint32, ys platform.TestCaseDB, loadedHooks *hooks.Hook, testReportFS yaml.TestReportFS, testRunChan chan string, apiTimeout uint64) models.TestRunStatus {

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

	errChan := make(chan error)
	t.logger.Debug("", zap.Any("app pid", pid))
	if len(appCmd) == 0 && pid != 0 {
		t.logger.Debug("running keploy tests along with other unit tests")
	} else {
		// start user application
		t.logger.Info("running user application for test run of test set", zap.Any("test-set", testSet))
		go func() {
			if err := loadedHooks.LaunchUserApplication(appCmd, appContainer, appNetwork, delay); err != nil {
				switch err {
				case hooks.ErrInterrupted:
					t.logger.Info("keploy terminated user application")
				case hooks.ErrCommandError:
					t.logger.Error("failed to run user application hence stopping keploy", zap.Error(err))
				case hooks.ErrUnExpected:
					t.logger.Error("user application terminated unexpectedly")
				default:
					t.logger.Error("unknown error recieved from application")
				}
				errChan <- err
			}
		}()
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
	if len(appCmd) == 0 && pid != 0 && testRunChan != nil {
		testRunChan <- testReport.Name
	}

	var (
		success = 0
		failure = 0
		status  = models.TestRunStatusPassed
	)

	// sort the testcases in
	// sort.Slice(tcs, func(i, j int) bool {
	// 	// if tcs[i].Kind == models.HTTP && tcs[j].Kind == models.HTTP {
	// 		// iHttpSpec := &spec.HttpSpec{}
	// 		// tcs[i].Spec.Decode(iHttpSpec)

	// 		// jHttpSpec := &spec.HttpSpec{}
	// 		// tcs[j].Spec.Decode(jHttpSpec)
	// 		// return iHttpSpec.Created < jHttpSpec.Created
	// 	// }
	// 	// return true
	// 	return tcs[i].Created < tcs[j].Created
	// })

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
		select {
		case err = <-errChan:
			switch err {
			case hooks.ErrInterrupted:
				exitLoop = true
				status = models.TestRunStatusUserAbort
			case hooks.ErrCommandError:
				exitLoop = true
				status = models.TestRunStatusFaultUserApp
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
			// httpSpec := &spec.HttpSpec{}
			// err := tc.Spec.Decode(httpSpec)
			// if err != nil {
			// 	t.logger.Error("failed to unmarshal yaml doc for simulation of http request", zap.Error(err))
			// 	return false
			// }
			started := time.Now().UTC()
			// for i, _ := range mocks[tc.Name] {
			// 	loadedHooks.AppendDeps(&mocks[tc.Name][i])
			// }

			// t.logger.Debug("Before setting deps.... during testing...")
			// loadedHooks.SetDeps(tc.Mocks)
			t.logger.Debug("Before simulating the request", zap.Any("Test case", tc))

			ok, _ := loadedHooks.IsDockerRelatedCmd(appCmd)
			if ok || dIDE {
				//changing Ip address only in case of docker
				tc.HttpReq.URL = replaceHostToIP(tc.HttpReq.URL, userIp)
				t.logger.Debug("", zap.Any("replaced URL in case of docker env", tc.HttpReq.URL))
			}
			t.logger.Debug(fmt.Sprintf("the url of the testcase: %v", tc.HttpReq.URL))
			// time.Sleep(10 * time.Second)
			resp, err := pkg.SimulateHttp(*tc, t.logger, apiTimeout)
			t.logger.Debug("After simulating the request", zap.Any("test case id", tc.Name))
			t.logger.Debug("After GetResp of the request", zap.Any("test case id", tc.Name))

			if err != nil {
				t.logger.Info("result", zap.Any("testcase id", tc.Name), zap.Any("passed", "false"))
				continue
			}
			testPass, testResult := t.testHttp(*tc, resp)
			t.logger.Info("result", zap.Any("testcase id", tc.Name), zap.Any("passed", testPass))
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
				TestCasePath: path + testSet,
				// MockPath:     mockPath,
				// Noise:        httpSpec.Assertions["noise"],
				Noise:  tc.Noise,
				Result: *testResult,
			})
			testReportFS.Lock()
			testReportFS.Unlock()
			// 		spec := &spec.HttpSpec{}
			// 		err := tc.Spec.Decode(spec)
			// 		if err!=nil {
			// 			t.logger.Error("failed to unmarshal yaml doc for simulation of http request", zap.Error(err))
			// 			return false
			// 		}
			// 		req, err := http.NewRequest(string(spec.Request.Method), "http://localhost"+":"+k.cfg.App.Port+spec.Request.URL, bytes.NewBufferString(spec.Request.Body))
			// 		if err != nil {
			// 			panic(err)
			// 		}
			// 		req.Header = tc.HttpReq.Header
			// 		req.Header.Set("KEPLOY_TEST_ID", tc.ID)
			// 		req.ProtoMajor = tc.HttpReq.ProtoMajor
			// 		req.ProtoMinor = tc.HttpReq.ProtoMinor
			// 		req.Close = true

			// 		// httpresp, err := k.client.Do(req)
			// 		k.client.Do(req)
			// 		if err != nil {
			// 			k.Log.Error("failed sending testcase request to app", zap.Error(err))
			// 			return nil, err
			// 		}
			// 		// defer httpresp.Body.Close()
			// 		println("before blocking simulate")

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
	err = testReportFS.Write(context.Background(), testReportPath, testReport)
	if err != nil {
		t.logger.Error(err.Error())
		return models.TestRunStatusFailed
	}

	t.logger.Debug("the result before", zap.Any("", testReport.Status), zap.Any("testreport name", testReport.Name))
	t.logger.Debug("the result after", zap.Any("", testReport.Status), zap.Any("testreport name", testReport.Name))

	if len(appCmd) == 0 && pid != 0 {
		t.logger.Debug("no need to stop the user application when running keploy tests along with unit tests")
	} else {
		// stop the user application
		loadedHooks.StopUserApplication()
	}

	return status
}

func (t *tester) testHttp(tc models.TestCase, actualResponse *models.HttpResp) (bool, *models.Result) {
	// httpSpec := &spec.HttpSpec{}
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
	// err := tc.Spec.Decode(httpSpec)
	// if err!=nil {
	// 	t.logger.Error("failed to unmarshal yaml doc for simulation of http request", zap.Error(err))
	// 	return false, res
	// }
	// find noisy fields
	// _, err := FlattenHttpResponse(pkg.ToHttpHeader(tc.HttpResp.Header), tc.HttpResp.Body)
	// if err != nil {
	// 	msg := "error in flattening http response"
	// 	t.logger.Error(msg, zap.Error(err))
	// 	return false, res
	// }
	// noise := httpSpec.Assertions["noise"]
	noise := tc.Noise

	var (
		bodyNoise   []string
		headerNoise = map[string]string{}
	)

	for _, n := range noise {
		a := strings.Split(n, ".")
		if len(a) > 1 && a[0] == "body" {
			x := strings.Join(a[1:], ".")
			bodyNoise = append(bodyNoise, x)
		} else if a[0] == "header" {
			// if len(a) == 2 {
			//  headerNoise[a[1]] = a[1]
			//  continue
			// }
			headerNoise[a[len(a)-1]] = a[len(a)-1]
			// headerNoise[a[0]] = a[0]
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
		// "Test Result:\n"+
		// "\tInput Http Request: %+v\n\n"+
		// "\tExpected Response: "+
		// "%+v\n\n"+"\tActual Response: "+
		// , tc.ID)

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
				logDiffs.PushHeaderDiff(fmt.Sprint(j), fmt.Sprint(actualHeader[i]), headerNoise)
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
		// time.Sleep(time.Second * time.Duration(delay)) // race condition bugging and mixing outputs
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

	// t.logger.Info("", zap.Any("result of test", res))

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
