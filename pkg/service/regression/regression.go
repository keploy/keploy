package regression

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/k0kubun/pp/v3"
	"github.com/wI2L/jsondiff"
	grpcMock "go.keploy.io/server/grpc/mock"
	"go.keploy.io/server/grpc/utils"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/service/run"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func New(tdb models.TestCaseDB, rdb run.DB, log *zap.Logger, TestExport bool, mFS models.MockFS, tFS models.TestReportFS) *Regression {
	return &Regression{
		yamlTcs:      sync.Map{},
		tdb:          tdb,
		log:          log,
		rdb:          rdb,
		mockFS:       mFS,
		testReportFS: tFS,
		testExport:   TestExport,
	}
}

type Regression struct {
	yamlTcs      sync.Map
	tdb          models.TestCaseDB
	rdb          run.DB
	mockFS       models.MockFS
	testReportFS models.TestReportFS
	testExport   bool
	log          *zap.Logger
}

func (r *Regression) StartTestRun(ctx context.Context, runId, testCasePath, mockPath, testReportPath string) error {
	if !pkg.IsValidPath(testCasePath) || !pkg.IsValidPath(mockPath) {
		r.log.Error("file path should be absolute to read and write testcases and their mocks")
		return fmt.Errorf("file path should be absolute")
	}
	tcs, err := r.mockFS.ReadAll(ctx, testCasePath, mockPath)
	if err != nil {
		r.log.Error("failed to read and cache testcases from ", zap.String("testcase path", pkg.SanitiseInput(testCasePath)), zap.String("mock path", pkg.SanitiseInput(mockPath)), zap.Error(err))
		return err
	}

	tcsMap := sync.Map{}
	for _, j := range tcs {
		tcsMap.Store(j.ID, j)
	}
	r.yamlTcs.Store(runId, tcsMap)
	err = r.testReportFS.Write(ctx, testReportPath, models.TestReport{Name: runId, Total: len(tcs), Status: string(models.TestRunStatusRunning)})
	if err != nil {
		r.log.Error("failed to create test report file", zap.String("file path", testReportPath), zap.Error(err))
		return err
	}
	return nil
}

func (r *Regression) StopTestRun(ctx context.Context, runId, testReportPath string) error {
	r.yamlTcs.Delete(runId)
	testResults, err := r.testReportFS.GetResults(runId)
	if err != nil {
		r.log.Error(err.Error())
	}
	var (
		success = 0
		failure = 0
		status  = models.TestRunStatusPassed
	)
	for _, j := range testResults {
		if j.Status == models.TestStatusPassed {
			success++
		} else if j.Status == models.TestStatusFailed {
			failure++
			status = models.TestRunStatusFailed
		}
	}
	err = r.testReportFS.Write(ctx, testReportPath, models.TestReport{Name: runId, Total: len(testResults), Status: string(status), Tests: testResults, Success: success, Failure: failure})
	if err != nil {
		r.log.Error("failed to create test report file", zap.String("file path", testReportPath), zap.Error(err))
		return err
	}
	return nil
}

func (r *Regression) test(ctx context.Context, cid, runId, id, app string, resp models.HttpResp) (bool, *models.Result, *models.TestCase, error) {
	var (
		tc  models.TestCase
		err error
	)
	switch r.testExport {
	case false:
		tc, err = r.tdb.Get(ctx, cid, id)
		if err != nil {
			r.log.Error("failed to get testcase from DB", zap.String("id", id), zap.String("cid", cid), zap.String("appID", app), zap.Error(err))
			return false, nil, nil, err
		}
	case true:
		if val, ok := r.yamlTcs.Load(runId); ok {
			tcsMap := val.(sync.Map)
			if val, ok := tcsMap.Load(id); ok {
				tc = val.(models.TestCase)
				tcsMap.Delete(id)
			} else {
				err := fmt.Errorf("failed to load testcase from tcs map coresponding to testcaseId: %s", pkg.SanitiseInput(id))
				r.log.Error(err.Error())
				return false, nil, nil, err
			}
		} else {
			err := fmt.Errorf("failed to load testcases coresponding to runId: %s", pkg.SanitiseInput(runId))
			r.log.Error(err.Error())
			return false, nil, nil, err
		}
	}
	bodyType := models.BodyTypePlain
	if json.Valid([]byte(resp.Body)) {
		bodyType = models.BodyTypeJSON
	}
	pass := true
	hRes := &[]models.HeaderResult{}

	res := &models.Result{
		StatusCode: models.IntResult{
			Normal:   false,
			Expected: tc.HttpResp.StatusCode,
			Actual:   resp.StatusCode,
		},
		BodyResult: models.BodyResult{
			Normal:   false,
			Type:     bodyType,
			Expected: tc.HttpResp.Body,
			Actual:   resp.Body,
		},
	}

	var (
		bodyNoise   []string
		headerNoise = map[string]string{}
	)

	for _, n := range tc.Noise {
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

	if !pkg.Contains(tc.Noise, "body") && bodyType == models.BodyTypeJSON {
		pass, err = pkg.Match(tc.HttpResp.Body, resp.Body, bodyNoise, r.log)
		if err != nil {
			return false, res, &tc, err
		}
	} else {
		if !pkg.Contains(tc.Noise, "body") && tc.HttpResp.Body != resp.Body {
			pass = false
		}
	}

	res.BodyResult.Normal = pass

	if !pkg.CompareHeaders(tc.HttpResp.Header, resp.Header, hRes, headerNoise) {

		pass = false
	}

	res.HeadersResult = *hRes
	if tc.HttpResp.StatusCode == resp.StatusCode {
		res.StatusCode.Normal = true
	} else {

		pass = false
	}
	if !pass {
		logger := pp.New()
		logger.WithLineInfo = false
		logger.SetColorScheme(models.FailingColorScheme)
		var logs = ""

		logs = logs + logger.Sprintf("Testrun failed for testcase with id: %s\n"+
			"Test Result:\n"+
			"\tInput Http Request: %+v\n\n"+
			"\tExpected Response: "+
			"%+v\n\n"+"\tActual Response: "+
			"%+v\n\n"+"DIFF: \n", tc.ID, tc.HttpReq, tc.HttpResp, resp)

		if !res.StatusCode.Normal {
			logs += logger.Sprintf("\tExpected StatusCode: %s"+"\n\tActual StatusCode: %s\n\n", res.StatusCode.Expected, res.StatusCode.Actual)

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
			logs += "\t Response Headers: {\n"
			for i, j := range expectedHeader {
				logs += logger.Sprintf("\t\t%s"+": {\n\t\t\tExpected value: %+v"+"\n\t\t\tActual value: %+v\n\t\t}\n", i, fmt.Sprintf("%v", j), fmt.Sprintf("%v", actualHeader[i]))
			}
			logs += "\t}\n"
		}

		if !res.BodyResult.Normal {
			logs += "\tResponse body: {\n"
			if json.Valid([]byte(resp.Body)) {
				// compute and log body's json diff
				expected, actual := pkg.RemoveNoise(tc.HttpResp.Body, resp.Body, bodyNoise, r.log)
				patch, _ := jsondiff.Compare(expected, actual)
				for _, op := range patch {
					keyStr := op.Path.String()
					if len(keyStr) > 1 && keyStr[0] == '/' {
						keyStr = keyStr[1:]
					}
					logs += logger.Sprintf("\t\t%s"+": {\n\t\t\tExpected value: %+v"+"\n\t\t\tActual value: %+v\n\t\t}\n", keyStr, op.OldValue, op.Value)
				}
				logs += "\t}\n"
			} else {
				// just log both the bodies as plain text without really computing the diff
				logs += logger.Sprintf("{\n\t\t\tExpected value: %+v"+"\n\t\t\tActual value: %+v\n\t\t}\n", tc.HttpResp.Body, resp.Body)

			}
		}
		logs += "--------------------------------------------------------------------\n\n"
		logger.Printf(logs)
	} else {
		logger := pp.New()
		logger.WithLineInfo = false
		logger.SetColorScheme(models.PassingColorScheme)
		var log2 = ""
		log2 += logger.Sprintf("Testrun passed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.ID)
		logger.Printf(log2)

	}
	return pass, res, &tc, nil
}

func (r *Regression) testGrpc(ctx context.Context, cid, runId, id, app string, resp string) (bool, *models.Result, *models.TestCase, error) {
	var (
		tc  models.TestCase
		err error
	)
	tc, err = r.tdb.Get(ctx, cid, id)
	if err != nil {
		r.log.Error("failed to get testcase from DB", zap.String("id", id), zap.String("cid", cid), zap.String("appID", app), zap.Error(err))
		return false, nil, nil, err
	}
	bodyType := models.BodyTypePlain
	if json.Valid([]byte(resp)) {
		bodyType = models.BodyTypeJSON
	}
	pass := true

	res := &models.Result{
		BodyResult: models.BodyResult{
			Normal:   false,
			Type:     bodyType,
			Expected: tc.GrpcResp,
			Actual:   resp,
		},
	}

	var (
		bodyNoise   []string
		headerNoise = map[string]string{}
	)

	for _, n := range tc.Noise {
		a := strings.Split(n, ".")
		if len(a) > 1 && a[0] == "body" {
			x := strings.Join(a[1:], ".")
			bodyNoise = append(bodyNoise, x)
		} else if a[0] == "header" {
			headerNoise[a[len(a)-1]] = a[len(a)-1]
		}
	}

	if !pkg.Contains(tc.Noise, "body") && bodyType == models.BodyTypeJSON {
		pass, err = pkg.Match(tc.GrpcResp, resp, bodyNoise, r.log)
		if err != nil {
			return false, res, &tc, err
		}
	} else {
		if !pkg.Contains(tc.Noise, "body") && tc.GrpcResp != resp {
			pass = false
		}
	}

	res.BodyResult.Normal = pass
	if !pass {
		logger := pp.New()
		logger.WithLineInfo = false
		logger.SetColorScheme(models.FailingColorScheme)
		var logs = ""

		logs = logs + logger.Sprintf("Testrun failed for testcase with id: %s\n"+
			"Test Result:\n"+
			"\tInput Grpc Request: %+v\n\n"+
			"\tExpected Response: "+
			"%+v\n\n"+"\tActual Response: "+
			"%+v\n\n"+"DIFF: \n", tc.ID, tc.GrpcReq, tc.GrpcResp, resp)

		if !res.BodyResult.Normal {

			expected, actual := pkg.RemoveNoise(tc.GrpcResp, resp, bodyNoise, r.log)

			patch, _ := jsondiff.Compare(expected, actual)
			logs += "\tResponse body: {\n"
			for _, op := range patch {
				keyStr := op.Path.String()
				if len(keyStr) > 1 && keyStr[0] == '/' {
					keyStr = keyStr[1:]
				}
				logs += logger.Sprintf("\t\t%s"+": {\n\t\t\tExpected value: %+v"+"\n\t\t\tActual value: %+v\n\t\t}\n", keyStr, op.OldValue, op.Value)
			}
			logs += "\t}\n"

		}
		logs += "--------------------------------------------------------------------\n\n"
		logger.Printf(logs)
	} else {
		logger := pp.New()
		logger.WithLineInfo = false
		logger.SetColorScheme(models.PassingColorScheme)
		var log2 = ""
		log2 += logger.Sprintf("Testrun passed for testcase with id: %s\n\n--------------------------------------------------------------------\n\n", tc.ID)
		logger.Printf(log2)

	}
	return pass, res, &tc, nil
}

func (r *Regression) Test(ctx context.Context, cid, app, runID, id, testCasePath, mockPath string, resp models.HttpResp) (bool, error) {
	var t *run.Test
	started := time.Now().UTC()
	ok, res, tc, err := r.test(ctx, cid, runID, id, app, resp)
	if tc != nil {
		t = &run.Test{
			ID:         uuid.New().String(),
			Started:    started.Unix(),
			RunID:      runID,
			TestCaseID: id,
			URI:        tc.URI,
			Req:        tc.HttpReq,
			Dep:        tc.Deps,
			Resp:       resp,
			Result:     *res,
			Noise:      tc.Noise,
		}
	}
	t.Completed = time.Now().UTC().Unix()
	defer func() {
		if r.testExport {
			mockIds := []string{}
			for i := 0; i < len(tc.Mocks); i++ {
				mockIds = append(mockIds, tc.Mocks[i].Name)
			}
			// r.store.WriteTestReport(ctx, testReportPath, models.TestReport{})
			r.testReportFS.SetResult(runID, models.TestResult{
				Name:       runID,
				Status:     t.Status,
				Started:    t.Started,
				Completed:  t.Completed,
				TestCaseID: id,
				Req: models.MockHttpReq{
					Method:     t.Req.Method,
					ProtoMajor: t.Req.ProtoMajor,
					ProtoMinor: t.Req.ProtoMinor,
					URL:        t.Req.URL,
					URLParams:  t.Req.URLParams,
					Header:     grpcMock.ToMockHeader(t.Req.Header),
					Body:       t.Req.Body,
				},
				Res: models.MockHttpResp{
					StatusCode:    t.Resp.StatusCode,
					Header:        grpcMock.ToMockHeader(t.Resp.Header),
					Body:          t.Resp.Body,
					StatusMessage: t.Resp.StatusMessage,
					ProtoMajor:    t.Resp.ProtoMajor,
					ProtoMinor:    t.Resp.ProtoMinor,
				},
				Mocks:        mockIds,
				TestCasePath: testCasePath,
				MockPath:     mockPath,
				Noise:        tc.Noise,
				Result:       *res,
			})
		} else {
			err2 := r.saveResult(ctx, t)
			if err2 != nil {
				r.log.Error("failed test result to db", zap.Error(err2), zap.String("cid", cid), zap.String("app", app))
			}
		}
	}()

	if err != nil {
		r.log.Error("failed to run the testcase", zap.Error(err), zap.String("cid", cid), zap.String("app", app))
		t.Status = models.TestStatusFailed
	}
	if ok {
		t.Status = models.TestStatusPassed
		return ok, nil
	}
	t.Status = models.TestStatusFailed
	return false, nil
}

func (r *Regression) TestGrpc(ctx context.Context, cid, app, runID, id, resp string) (bool, error) {
	var t *run.Test
	started := time.Now().UTC()
	ok, res, tc, err := r.testGrpc(ctx, cid, runID, id, app, resp)
	if tc != nil {
		t = &run.Test{
			ID:         uuid.New().String(),
			Started:    started.Unix(),
			RunID:      runID,
			TestCaseID: id,
			GrpcMethod: tc.GrpcMethod,
			GrpcReq:    tc.GrpcReq,
			Dep:        tc.Deps,
			GrpcResp:   resp,
			Result:     *res,
			Noise:      tc.Noise,
		}
	}
	t.Completed = time.Now().UTC().Unix()
	defer func() {
		err2 := r.saveResult(ctx, t)
		if err2 != nil {
			r.log.Error("failed test result to db", zap.Error(err2), zap.String("cid", cid), zap.String("app", app))
		}
	}()

	if err != nil {
		r.log.Error("failed to run the testcase", zap.Error(err), zap.String("cid", cid), zap.String("app", app))
		t.Status = models.TestStatusFailed
	}
	if ok {
		t.Status = models.TestStatusPassed
		return ok, nil
	}
	t.Status = models.TestStatusFailed
	return false, nil
}

func (r *Regression) saveResult(ctx context.Context, t *run.Test) error {
	err := r.rdb.PutTest(ctx, *t)
	if err != nil {
		return err
	}
	if t.Status == models.TestStatusFailed {
		err = r.rdb.Increment(ctx, false, true, t.RunID)
	} else {
		err = r.rdb.Increment(ctx, true, false, t.RunID)
	}

	if err != nil {
		return err
	}
	return nil
}

func (r *Regression) deNoiseYaml(ctx context.Context, id, path, body string, h http.Header) error {
	tcs, err := r.mockFS.Read(ctx, path, id, false)
	if err != nil {
		r.log.Error("failed to read testcase from yaml", zap.String("id", id), zap.String("path", path), zap.Error(err))
		return err
	}
	if len(tcs) == 0 {
		r.log.Error("no testcase exists with", zap.String("id", id), zap.String("at path", path), zap.Error(err))
		return err
	}
	docs, err := grpcMock.Decode(tcs)
	if err != nil {
		r.log.Error(err.Error())
		return err
	}
	tc := docs[0]

	a, b := map[string][]string{}, map[string][]string{}

	// add headers
	for k, v := range utils.GetStringMap(tc.Spec.Res.Header) {
		a["header."+k] = []string{strings.Join(v, "")}
	}

	for k, v := range h {
		b["header."+k] = []string{strings.Join(v, "")}
	}

	err = addBody(tc.Spec.Res.Body, a)
	if err != nil {
		r.log.Error("failed to parse response body", zap.String("id", id), zap.Error(err))
		return err
	}

	err = addBody(body, b)
	if err != nil {
		r.log.Error("failed to parse response body", zap.String("id", id), zap.Error(err))
		return err
	}
	// r.log.Debug("denoise between",zap.Any("stored object",a),zap.Any("coming object",b))
	var noise []string
	for k, v := range a {
		v2, ok := b[k]
		if !ok {
			noise = append(noise, k)
			continue
		}
		if !reflect.DeepEqual(v, v2) {
			noise = append(noise, k)
		}
	}
	// r.log.Debug("Noise Array : ",zap.Any("",noise))
	tc.Spec.Assertions["noise"] = utils.ToStrArr(noise)
	doc, err := grpcMock.Encode(tc)
	if err != nil {
		r.log.Error(err.Error())
		return err
	}
	enc := doc
	d, err := yaml.Marshal(enc)
	if err != nil {
		r.log.Error("failed to marshal document to yaml", zap.Any("error", err))
		return err
	}
	err = os.WriteFile(filepath.Join(path, id+".yaml"), d, os.ModePerm)
	if err != nil {
		r.log.Error("failed to write test to yaml file", zap.String("id", id), zap.String("path", path), zap.Error(err))
	}

	return nil
}

func (r *Regression) DeNoise(ctx context.Context, cid, id, app, body string, h http.Header, path string) error {

	if r.testExport {
		return r.deNoiseYaml(ctx, id, path, body, h)
	}
	tc, err := r.tdb.Get(ctx, cid, id)
	reqType := ctx.Value("reqType")
	var tcRespBody string
	switch reqType {
	case "http":
		tcRespBody = tc.HttpResp.Body

	case "grpc":
		tcRespBody = tc.GrpcResp
	}
	if err != nil {
		r.log.Error("failed to get testcase from DB", zap.String("id", id), zap.String("cid", cid), zap.String("appID", app), zap.Error(err))
		return err
	}

	a, b := map[string][]string{}, map[string][]string{}

	if reqType == "http" {
		// add headers
		for k, v := range tc.HttpResp.Header {
			a["header."+k] = []string{strings.Join(v, "")}
		}

		for k, v := range h {
			b["header."+k] = []string{strings.Join(v, "")}
		}
	}

	err = addBody(tcRespBody, a)
	if err != nil {
		r.log.Error("failed to parse response body", zap.String("id", id), zap.String("cid", cid), zap.String("appID", app), zap.Error(err))
		return err
	}

	err = addBody(body, b)
	if err != nil {
		r.log.Error("failed to parse response body", zap.String("id", id), zap.String("cid", cid), zap.String("appID", app), zap.Error(err))
		return err
	}
	// r.log.Debug("denoise between",zap.Any("stored object",a),zap.Any("coming object",b))
	var noise []string
	for k, v := range a {
		v2, ok := b[k]
		if !ok {
			noise = append(noise, k)
			continue
		}
		if !reflect.DeepEqual(v, v2) {
			noise = append(noise, k)
		}
	}
	// r.log.Debug("Noise Array : ",zap.Any("",noise))
	tc.Noise = noise
	err = r.tdb.Upsert(ctx, tc)
	if err != nil {
		r.log.Error("failed to update noise fields for testcase", zap.String("id", id), zap.String("cid", cid), zap.String("appID", app), zap.Error(err))
		return err
	}
	return nil
}

func addBody(body string, m map[string][]string) error {
	// add body
	if json.Valid([]byte(body)) {
		var result interface{}

		err := json.Unmarshal([]byte(body), &result)
		if err != nil {
			return err
		}
		j := flatten(result)
		for k, v := range j {
			nk := "body"
			if k != "" {
				nk = nk + "." + k
			}
			m[nk] = v
		}
	} else {
		// add it as raw text
		m["body"] = []string{body}
	}
	return nil
}

// Flatten takes a map and returns a new one where nested maps are replaced
// by dot-delimited keys.
// examples of valid jsons - https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/JSON/parse#examples
func flatten(j interface{}) map[string][]string {
	if j == nil {
		return map[string][]string{"": {""}}
	}
	o := make(map[string][]string)
	x := reflect.ValueOf(j)
	switch x.Kind() {
	case reflect.Map:
		m, ok := j.(map[string]interface{})
		if !ok {
			return map[string][]string{}
		}
		for k, v := range m {
			nm := flatten(v)
			for nk, nv := range nm {
				fk := k
				if nk != "" {
					fk = fk + "." + nk
				}
				o[fk] = nv
			}
		}
	case reflect.Bool:
		o[""] = []string{strconv.FormatBool(x.Bool())}
	case reflect.Float64:
		o[""] = []string{strconv.FormatFloat(x.Float(), 'E', -1, 64)}
	case reflect.String:
		o[""] = []string{x.String()}
	case reflect.Slice:
		child, ok := j.([]interface{})
		if !ok {
			return map[string][]string{}
		}
		for _, av := range child {
			nm := flatten(av)
			for nk, nv := range nm {
				if ov, exists := o[nk]; exists {
					o[nk] = append(ov, nv...)
				} else {
					o[nk] = nv
				}
			}
		}
	default:
		fmt.Println("found invalid value in json", j, x.Kind())
	}
	return o
}
