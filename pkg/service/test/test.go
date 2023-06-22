package test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"net/url"

	"github.com/k0kubun/pp/v3"
	"github.com/wI2L/jsondiff"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
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
func (t *tester) Test(tcsPath, mockPath, testReportPath string, appCmd, appContainer, appNetwork string, Delay uint64) bool {
	models.SetMode(models.MODE_TEST)

	// println("called Test()")

	testReportFS := yaml.NewTestReportFS(t.logger)
	// fetch the recorded testcases with their mocks
	ys := yaml.NewYamlStore(tcsPath, mockPath, t.logger)
	// start the proxies
	ps := proxy.BootProxies(t.logger, proxy.Option{}, appCmd)
	// Initiate the hooks and update the vaccant ProxyPorts map
	// loadedHooks := hooks.NewHook(ps.PortList, ys, t.logger)
	loadedHooks := hooks.NewHook(ys, t.logger)
	if err := loadedHooks.LoadHooks(appCmd, appContainer); err != nil {
		return false
	}
	// proxy update its state in the ProxyPorts map
	ps.SetHook(loadedHooks)

	//Sending Proxy Ip & Port to the ebpf program
	if err := loadedHooks.SendProxyInfo(ps.IP, ps.Port); err != nil {
		return false
	}

	// start user application
	if err := loadedHooks.LaunchUserApplication(appCmd, appContainer, appNetwork, Delay); err != nil {
		return false
	}

	// Enable Pid Filtering
	loadedHooks.EnablePidFilter()
	ps.FilterPid = true

	tcs, err := ys.Read(nil)
	if err != nil {
		return false
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
		t.logger.Error(Emoji + err.Error())
		return false
	}
	var (
		success = 0
		failure = 0
		status  = models.TestRunStatusPassed
	)

	passed := true

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
	ok, _ := loadedHooks.IsDockerRelatedCmd(appCmd)
	if ok {
		userIp = loadedHooks.GetUserIp(appContainer, appNetwork)
		t.logger.Debug(Emoji, zap.Any("User Ip", userIp))
	}

	t.logger.Info(Emoji, zap.Any("no of test cases", len(tcs)))
	for _, tc := range tcs {
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

			t.logger.Debug(Emoji + "Before setting deps.... during testing...")
			loadedHooks.SetDeps(tc.Mocks)
			t.logger.Debug(Emoji+"Before simulating the request", zap.Any("Test case", tc))

			ok, _ := loadedHooks.IsDockerRelatedCmd(appCmd)
			if ok {
				//changing Ip address only in case of docker
				tc.HttpReq.URL = replaceHostToIP(tc.HttpReq.URL, userIp)
			}

			resp, err := pkg.SimulateHttp(*tc, t.logger, loadedHooks.GetResp)
			resp = loadedHooks.GetResp()
			if err != nil {
				t.logger.Info(Emoji+"result", zap.Any("testcase id", tc.Name), zap.Any("passed", "false"))
				continue
			}
			// println("before blocking simulate")

			// resp := loadedHooks.GetResp()
			// println("after blocking simulate")
			testPass, testResult := t.testHttp(*tc, resp)
			passed = passed && testPass
			t.logger.Info(Emoji+"result", zap.Any("testcase id", tc.Name), zap.Any("passed", testPass))
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
				TestCasePath: tcsPath,
				MockPath:     mockPath,
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
	if err != nil {
		t.logger.Error(Emoji+"failed to fetch test results", zap.Error(err))
		return passed
	}
	testReport.Total = len(testResults)
	testReport.Status = string(status)
	testReport.Tests = testResults
	testReport.Success = success
	testReport.Failure = failure
	err = testReportFS.Write(context.Background(), testReportPath, testReport)
	if err != nil {
		t.logger.Error(err.Error())
		return false
	}

	t.logger.Info(Emoji+"test run completed", zap.Bool("passed overall", passed))

	// stop listening for the eBPF events
	loadedHooks.Stop(true)

	//stop listening for proxy server
	ps.StopProxyServer()

	return true
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
	m, err := FlattenHttpResponse(pkg.ToHttpHeader(tc.HttpResp.Header), tc.HttpResp.Body)
	if err != nil {
		msg := "error in flattening http response"
		t.logger.Error(Emoji+msg, zap.Error(err))
		return false, res
	}
	// noise := httpSpec.Assertions["noise"]
	noise := tc.Noise
	noise = append(noise, FindNoisyFields(m, func(k string, vals []string) bool {
		// check if k is date
		for _, v := range vals {
			if pkg.IsTime(v) {
				return true
			}
		}

		// maybe we need to concatenate the values
		return pkg.IsTime(strings.Join(vals, ", "))
	})...)

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

	if !Contains(noise, "body") && bodyType == models.BodyTypeJSON {
		cleanExp, cleanAct, pass, err = Match(tc.HttpResp.Body, actualResponse.Body, bodyNoise, t.logger)
		if err != nil {
			return false, res
		}
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
					t.logger.Warn(Emoji+"failed to compute json diff", zap.Error(err))
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
		return currentURL
	}

	// Check if the URL host is "localhost"
	if parsedURL.Hostname() == "localhost" {
		// Replace "localhost" with the IP address
		parsedURL.Host = strings.Replace(parsedURL.Host, "localhost", ipAddress, 1)
	}

	// Return the modified URL
	return parsedURL.String()
}
