package regression

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
	"go.keploy.io/server/pkg/platform/telemetry"
	"go.keploy.io/server/pkg/service/run"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

func New(tdb models.TestCaseDB, rdb run.DB, log *zap.Logger, EnableDeDup bool, adb telemetry.Service, client http.Client, TestExport bool, store models.MockStore) *Regression {
	return &Regression{
		yamlTcs:     sync.Map{},
		tdb:         tdb,
		tele:        adb,
		log:         log,
		rdb:         rdb,
		store:       store,
		testExport:  TestExport,
		client:      client,
		mu:          sync.Mutex{},
		anchors:     map[string][]map[string][]string{},
		noisyFields: map[string]map[string]bool{},
		fieldCounts: map[string]map[string]map[string]int{},
		EnableDeDup: EnableDeDup,
	}
}

type Regression struct {
	yamlTcs    sync.Map
	tdb        models.TestCaseDB
	tele       telemetry.Service
	rdb        run.DB
	store      models.MockStore
	testExport bool
	client     http.Client
	log        *zap.Logger
	mu         sync.Mutex
	appCount   int
	// index is `cid-appID-uri`
	//
	// anchors is map[index][]map[key][]value or map[index]combinationOfAnchors
	// anchors stores all the combinations of anchor fields for a particular index
	// anchor field is a low variance field which is used in the deduplication algorithm.
	// example: user-type or blood-group could be good anchor fields whereas timestamps
	// and usernames are bad anchor fields.
	// during deduplication only anchor fields are compared for new requests to determine whether its a duplicate or not.
	// other fields are ignored.
	anchors map[string][]map[string][]string
	// noisyFields is map[index][key]bool
	noisyFields map[string]map[string]bool
	// fieldCounts is map[index][key][value]count
	// fieldCounts stores the count of all values of a particular field in an index.
	// eg: lets say field is bloodGroup then the value would be {A+: 20, B+: 10,...}
	fieldCounts map[string]map[string]map[string]int
	EnableDeDup bool
}

func (r *Regression) DeleteTC(ctx context.Context, cid, id string) error {
	// reset cache
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.tdb.Get(ctx, cid, id)
	if err != nil {
		r.log.Error("failed to get testcases from the DB", zap.String("cid", cid), zap.Error(err))
		return errors.New("internal failure")
	}
	index := fmt.Sprintf("%s-%s-%s", t.CID, t.AppID, t.URI)
	delete(r.anchors, index)
	err = r.tdb.Delete(ctx, id)
	if err != nil {
		r.log.Error("failed to delete testcase from the DB", zap.String("cid", cid), zap.String("appID", t.AppID), zap.Error(err))
		return errors.New("internal failure")
	}

	r.tele.DeleteTc(r.client, ctx)
	return nil
}

func (r *Regression) GetApps(ctx context.Context, cid string) ([]string, error) {
	apps, err := r.tdb.GetApps(ctx, cid)
	if apps != nil && len(apps) != r.appCount {
		r.tele.GetApps(len(apps), r.client, ctx)
		r.appCount = len(apps)
	}
	return apps, err
}

func (r *Regression) Get(ctx context.Context, cid, appID, id string) (models.TestCase, error) {
	if r.testExport {
		return models.TestCase{}, nil
	}
	tcs, err := r.tdb.Get(ctx, cid, id)
	if err != nil {
		sanitizedAppID := pkg.SanitiseInput(appID)
		r.log.Error("failed to get testcases from the DB", zap.String("cid", cid), zap.String("appID", sanitizedAppID), zap.Error(err))
		return models.TestCase{}, errors.New("internal failure")
	}
	return tcs, nil
}

func (r *Regression) StartTestRun(ctx context.Context, runId, testCasePath, mockPath string) {
	if !pkg.IsValidPath(testCasePath) || !pkg.IsValidPath(mockPath) {
		return
	}
	tcs, err := r.store.ReadAll(ctx, testCasePath, mockPath)
	if err != nil {
		r.log.Error(err.Error())
		return
	}

	tcsMap := sync.Map{}
	for _, j := range tcs {
		tcsMap.Store(j.ID, j)
	}
	r.yamlTcs.Store(runId, tcsMap)
}

func (r *Regression) ReadTCS(ctx context.Context, testCasePath, mockPath string) ([]models.TestCase, error) {
	if !pkg.IsValidPath(testCasePath) || !pkg.IsValidPath(mockPath) {
		return nil, fmt.Errorf("file path should be absolute. got testcase path: %s and mock path: %s", pkg.SanitiseInput(testCasePath), pkg.SanitiseInput(mockPath))
	}
	res, err := r.store.ReadAll(ctx, testCasePath, mockPath)
	if err != nil {
		r.log.Error(err.Error())
	}
	return res, err
}

func (r *Regression) StopTestRun(ctx context.Context, runId string) {
	r.yamlTcs.Delete(runId)
}

func (r *Regression) GetAll(ctx context.Context, cid, appID string, offset *int, limit *int) ([]models.TestCase, error) {
	off, lim := 0, 25
	if offset != nil {
		off = *offset
	}
	if limit != nil {
		lim = *limit
	}

	tcs, err := r.tdb.GetAll(ctx, cid, appID, false, off, lim)

	if err != nil {
		sanitizedAppID := pkg.SanitiseInput(appID)
		r.log.Error("failed to get testcases from the DB", zap.String("cid", cid), zap.String("appID", sanitizedAppID), zap.Error(err))
		return nil, errors.New("internal failure")
	}
	return tcs, nil
}

func (r *Regression) GetAllGrpc(ctx context.Context, cid, appID string, offset *int, limit *int) ([]models.GrpcTestCase, error) {
	off, lim := 0, 25
	if offset != nil {
		off = *offset
	}
	if limit != nil {
		lim = *limit
	}

	tcs, err := r.tdb.GetAllGrpc(ctx, cid, appID, false, off, lim)

	if err != nil {
		sanitizedAppID := pkg.SanitiseInput(appID)
		r.log.Error("failed to get testcases from the DB", zap.String("cid", cid), zap.String("appID", sanitizedAppID), zap.Error(err))
		return nil, errors.New("internal failure")
	}
	return tcs, nil
}

func (r *Regression) UpdateTC(ctx context.Context, t []models.TestCase) error {
	for _, v := range t {
		err := r.tdb.UpdateTC(ctx, v)
		if err != nil {
			r.log.Error("failed to insert testcase into DB", zap.String("appID", v.AppID), zap.Error(err))
			return errors.New("internal failure")
		}
	}
	r.tele.EditTc(r.client, ctx)
	return nil
}

func (r *Regression) putTC(ctx context.Context, cid string, t models.TestCase) (string, error) {
	t.CID = cid

	var err error
	if r.EnableDeDup {
		// check if already exists
		dup, err := r.isDup(ctx, &t)
		if err != nil {
			r.log.Error("failed to run deduplication on the testcase", zap.String("cid", cid), zap.String("appID", t.AppID), zap.Error(err))
			return "", errors.New("internal failure")
		}
		if dup {
			r.log.Info("found duplicate testcase", zap.String("cid", cid), zap.String("appID", t.AppID), zap.String("uri", t.URI))
			return "", nil
		}
	}
	err = r.tdb.Upsert(ctx, t)
	if err != nil {
		r.log.Error("failed to insert testcase into DB", zap.String("cid", cid), zap.String("appID", t.AppID), zap.Error(err))
		return "", errors.New("internal failure")
	}

	return t.ID, nil
}

func (r *Regression) putTCGrpc(ctx context.Context, cid string, t models.GrpcTestCase) (string, error) {
	t.CID = cid

	var err error
	if r.EnableDeDup {
		// check if already exists
		dup, err := r.isDupGrpc(ctx, &t)
		if err != nil {
			r.log.Error("failed to run deduplication on the testcase", zap.String("cid", cid), zap.String("appID", t.AppID), zap.Error(err))
			return "", errors.New("internal failure")
		}
		if dup {
			r.log.Info("found duplicate testcase", zap.String("cid", cid), zap.String("appID", t.AppID), zap.String("uri", t.Method))
			return "", nil
		}
	}
	err = r.tdb.UpsertGrpc(ctx, t)
	if err != nil {
		r.log.Error("failed to insert testcase into DB", zap.String("cid", cid), zap.String("appID", t.AppID), zap.Error(err))
		return "", errors.New("internal failure")
	}

	return t.ID, nil
}

func (r *Regression) Put(ctx context.Context, cid string, tcs []models.TestCase) ([]string, error) {
	var ids []string
	if len(tcs) == 0 {
		return ids, errors.New("no testcase to update")
	}
	for _, t := range tcs {
		id, err := r.putTC(ctx, cid, t)
		if err != nil {
			msg := "failed saving testcase"
			r.log.Error(msg, zap.Error(err), zap.String("cid", cid), zap.String("id", t.ID), zap.String("app", t.AppID))
			return ids, errors.New(msg)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (r *Regression) PutGrpc(ctx context.Context, cid string, tcs []models.GrpcTestCase) ([]string, error) {
	var ids []string
	if len(tcs) == 0 {
		return ids, errors.New("no testcase to update")
	}
	for _, t := range tcs {
		id, err := r.putTCGrpc(ctx, cid, t)
		if err != nil {
			msg := "failed saving testcase"
			r.log.Error(msg, zap.Error(err), zap.String("cid", cid), zap.String("id", t.ID), zap.String("app", t.AppID))
			return ids, errors.New(msg)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (r *Regression) WriteTC(ctx context.Context, test []models.Mock, testCasePath, mockPath string) ([]string, error) {
	if testCasePath == "" || !pkg.IsValidPath(testCasePath) || !pkg.IsValidPath(mockPath) {
		return nil, fmt.Errorf("path directory not found. got testcase path: %s and mock path: %s", pkg.SanitiseInput(testCasePath), pkg.SanitiseInput(mockPath))
	}
	err := r.store.Write(ctx, testCasePath, test[0])
	if err != nil {
		r.log.Error(err.Error())
	}

	if len(test) > 1 {
		err = r.store.WriteAll(ctx, mockPath, test[0].Name, test[1:])
		if err != nil {
			r.log.Error(err.Error())
		}
	}
	return []string{test[0].Name}, nil
}

func (r *Regression) test(ctx context.Context, cid, runId, id, app string, resp models.HttpResp, testCasePath, mockPath string) (bool, *run.Result, *models.TestCase, error) {
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
	bodyType := run.BodyTypePlain
	if json.Valid([]byte(resp.Body)) {
		bodyType = run.BodyTypeJSON
	}
	pass := true
	hRes := &[]run.HeaderResult{}

	res := &run.Result{
		StatusCode: run.IntResult{
			Normal:   false,
			Expected: tc.HttpResp.StatusCode,
			Actual:   resp.StatusCode,
		},
		BodyResult: run.BodyResult{
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

	if !pkg.Contains(tc.Noise, "body") && bodyType == run.BodyTypeJSON {
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

			expected, actual := pkg.RemoveNoise(tc.HttpResp.Body, resp.Body, bodyNoise, r.log)

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

func (r *Regression) testGrpc(ctx context.Context, cid, runId, id, app string, resp string) (bool, *run.ResultGrpc, *models.GrpcTestCase, error) {
	var (
		tc  models.GrpcTestCase
		err error
	)
	tc, err = r.tdb.GetGrpc(ctx, cid, id)
	if err != nil {
		r.log.Error("failed to get testcase from DB", zap.String("id", id), zap.String("cid", cid), zap.String("appID", app), zap.Error(err))
		return false, nil, nil, err
	}
	bodyType := run.BodyTypePlain
	if json.Valid([]byte(resp)) {
		bodyType = run.BodyTypeJSON
	}
	pass := true

	res := &run.ResultGrpc{
		BodyResult: run.BodyResult{
			Normal:   false,
			Type:     bodyType,
			Expected: tc.Resp,
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

	if !pkg.Contains(tc.Noise, "body") && bodyType == run.BodyTypeJSON {
		pass, err = pkg.Match(tc.Resp, resp, bodyNoise, r.log)
		if err != nil {
			return false, res, &tc, err
		}
	} else {
		if !pkg.Contains(tc.Noise, "body") && tc.Resp != resp {
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
			"%+v\n\n"+"DIFF: \n", tc.ID, tc.GrpcReq, tc.Resp, resp)

		if !res.BodyResult.Normal {

			expected, actual := pkg.RemoveNoise(tc.Resp, resp, bodyNoise, r.log)

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
	ok, res, tc, err := r.test(ctx, cid, runID, id, app, resp, testCasePath, mockPath)
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
		err2 := r.saveResult(ctx, t)
		if err2 != nil {
			r.log.Error("failed test result to db", zap.Error(err2), zap.String("cid", cid), zap.String("app", app))
		}
	}()

	if err != nil {
		r.log.Error("failed to run the testcase", zap.Error(err), zap.String("cid", cid), zap.String("app", app))
		t.Status = run.TestStatusFailed
	}
	if ok {
		t.Status = run.TestStatusPassed
		return ok, nil
	}
	t.Status = run.TestStatusFailed
	return false, nil
}

func (r *Regression) TestGrpc(ctx context.Context, cid, app, runID, id, resp string) (bool, error) {
	var t *run.TestGrpc
	started := time.Now().UTC()
	ok, res, tc, err := r.testGrpc(ctx, cid, runID, id, app, resp)
	if tc != nil {
		t = &run.TestGrpc{
			ID:         uuid.New().String(),
			Started:    started.Unix(),
			RunID:      runID,
			TestCaseID: id,
			Method:     tc.Method,
			Req:        tc.GrpcReq,
			Dep:        tc.Deps,
			Resp:       resp,
			Result:     *res,
			Noise:      tc.Noise,
		}
	}
	t.Completed = time.Now().UTC().Unix()
	defer func() {
		err2 := r.saveResultGrpc(ctx, t)
		if err2 != nil {
			r.log.Error("failed test result to db", zap.Error(err2), zap.String("cid", cid), zap.String("app", app))
		}
	}()

	if err != nil {
		r.log.Error("failed to run the testcase", zap.Error(err), zap.String("cid", cid), zap.String("app", app))
		t.Status = run.TestStatusFailed
	}
	if ok {
		t.Status = run.TestStatusPassed
		return ok, nil
	}
	t.Status = run.TestStatusFailed
	return false, nil
}

func (r *Regression) saveResult(ctx context.Context, t *run.Test) error {
	err := r.rdb.PutTest(ctx, *t)
	if err != nil {
		return err
	}
	if t.Status == run.TestStatusFailed {
		err = r.rdb.Increment(ctx, false, true, t.RunID)
	} else {
		err = r.rdb.Increment(ctx, true, false, t.RunID)
	}

	if err != nil {
		return err
	}
	return nil
}

func (r *Regression) saveResultGrpc(ctx context.Context, t *run.TestGrpc) error {
	err := r.rdb.PutTestGrpc(ctx, *t)
	if err != nil {
		return err
	}
	if t.Status == run.TestStatusFailed {
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
	tcs, err := r.store.Read(ctx, path, id, false)
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
	if err != nil {
		r.log.Error("failed to get testcase from DB", zap.String("id", id), zap.String("cid", cid), zap.String("appID", app), zap.Error(err))
		return err
	}

	a, b := map[string][]string{}, map[string][]string{}

	// add headers
	for k, v := range tc.HttpResp.Header {
		a["header."+k] = []string{strings.Join(v, "")}
	}

	for k, v := range h {
		b["header."+k] = []string{strings.Join(v, "")}
	}

	err = addBody(tc.HttpResp.Body, a)
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

func (r *Regression) DeNoiseGrpc(ctx context.Context, cid, id, app, body string) error {
	tc, err := r.tdb.GetGrpc(ctx, cid, id)
	if err != nil {
		r.log.Error("failed to get testcase from DB", zap.String("id", id), zap.String("cid", cid), zap.String("appID", app), zap.Error(err))
		return err
	}

	a, b := map[string][]string{}, map[string][]string{}

	err = addBody(tc.Resp, a)
	if err != nil {
		r.log.Error("failed to parse response body", zap.String("id", id), zap.String("cid", cid), zap.String("appID", app), zap.Error(err))
		return err
	}

	err = addBody(body, b)
	if err != nil {
		r.log.Error("failed to parse response body", zap.String("id", id), zap.String("cid", cid), zap.String("appID", app), zap.Error(err))
		return err
	}
	r.log.Debug("denoise between", zap.Any("stored object", a), zap.Any("coming object", b))
	var noise []string
	for k, v := range a {
		v2, ok := b[k]
		fmt.Println("v2 , ok ", v2, ok)
		if !ok {
			noise = append(noise, k)
			continue
		}
		if !reflect.DeepEqual(v, v2) {
			noise = append(noise, k)
		}
	}
	r.log.Debug("Noise Array : ", zap.Any("", noise))
	tc.Noise = noise
	err = r.tdb.UpsertGrpc(ctx, tc)
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

func (r *Regression) fillCache(ctx context.Context, t *models.TestCase) (string, error) {

	index := fmt.Sprintf("%s-%s-%s", t.CID, t.AppID, t.URI)
	_, ok1 := r.noisyFields[index]
	_, ok2 := r.fieldCounts[index]
	if ok1 && ok2 {
		return index, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// check again after the lock
	_, ok1 = r.noisyFields[index]
	_, ok2 = r.fieldCounts[index]

	if !ok1 || !ok2 {
		var anchors []map[string][]string
		fieldCounts, noisyFields := map[string]map[string]int{}, map[string]bool{}
		tcs, err := r.tdb.GetKeys(ctx, t.CID, t.AppID, t.URI)
		if err != nil {
			return "", err
		}
		for _, v := range tcs {
			//var appAnchors map[string][]string
			//for _, a := range v.Anchors {
			//	appAnchors[a] = v.AllKeys[a]
			//}
			anchors = append(anchors, v.Anchors)
			for k, v1 := range v.AllKeys {
				if fieldCounts[k] == nil {
					fieldCounts[k] = map[string]int{}
				}
				for _, v2 := range v1 {
					fieldCounts[k][v2] = fieldCounts[k][v2] + 1
				}
				if !isAnchor(fieldCounts[k]) {
					noisyFields[k] = true
				}
			}
		}
		r.fieldCounts[index], r.noisyFields[index], r.anchors[index] = fieldCounts, noisyFields, anchors
	}
	return index, nil
}

func (r *Regression) fillCacheGrpc(ctx context.Context, t *models.GrpcTestCase) (string, error) {

	index := fmt.Sprintf("%s-%s-%s", t.CID, t.AppID, t.Method)
	_, ok1 := r.noisyFields[index]
	_, ok2 := r.fieldCounts[index]
	if ok1 && ok2 {
		return index, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// check again after the lock
	_, ok1 = r.noisyFields[index]
	_, ok2 = r.fieldCounts[index]

	if !ok1 || !ok2 {
		var anchors []map[string][]string
		fieldCounts, noisyFields := map[string]map[string]int{}, map[string]bool{}
		tcs, err := r.tdb.GetKeys(ctx, t.CID, t.AppID, t.Method)
		if err != nil {
			return "", err
		}
		for _, v := range tcs {
			//var appAnchors map[string][]string
			//for _, a := range v.Anchors {
			//	appAnchors[a] = v.AllKeys[a]
			//}
			anchors = append(anchors, v.Anchors)
			for k, v1 := range v.AllKeys {
				if fieldCounts[k] == nil {
					fieldCounts[k] = map[string]int{}
				}
				for _, v2 := range v1 {
					fieldCounts[k][v2] = fieldCounts[k][v2] + 1
				}
				if !isAnchor(fieldCounts[k]) {
					noisyFields[k] = true
				}
			}
		}
		r.fieldCounts[index], r.noisyFields[index], r.anchors[index] = fieldCounts, noisyFields, anchors
	}
	return index, nil
}

func (r *Regression) isDup(ctx context.Context, t *models.TestCase) (bool, error) {

	reqKeys := map[string][]string{}
	filterKeys := map[string][]string{}

	index, err := r.fillCache(ctx, t)
	if err != nil {
		return false, err
	}

	// add headers
	for k, v := range t.HttpReq.Header {
		reqKeys["header."+k] = []string{strings.Join(v, "")}
	}

	// add url params
	for k, v := range t.HttpReq.URLParams {
		reqKeys["url_params."+k] = []string{v}
	}

	// add body if it is a valid json
	if json.Valid([]byte(t.HttpReq.Body)) {
		var result interface{}

		err = json.Unmarshal([]byte(t.HttpReq.Body), &result)
		if err != nil {
			return false, err
		}
		body := flatten(result)
		for k, v := range body {
			nk := "body"
			if k != "" {
				nk = nk + "." + k
			}
			reqKeys[nk] = v
		}
	}

	isAnchorChange := true
	for k, v := range reqKeys {
		if !r.noisyFields[index][k] {
			// update field count
			for _, s := range v {
				if _, ok := r.fieldCounts[index][k]; !ok {
					r.fieldCounts[index][k] = map[string]int{}
				}
				r.fieldCounts[index][k][s] = r.fieldCounts[index][k][s] + 1
			}
			if !isAnchor(r.fieldCounts[index][k]) {
				r.noisyFields[index][k] = true
				isAnchorChange = true
				continue
			}
			filterKeys[k] = v
		}
	}

	if len(filterKeys) == 0 {
		return true, nil
	}
	if isAnchorChange {
		err = r.tdb.DeleteByAnchor(ctx, t.CID, t.AppID, t.URI, filterKeys)
		if err != nil {
			return false, err
		}
	}

	// check if testcase based on anchor keys already exists
	dup, err := r.exists(ctx, filterKeys, index)
	if err != nil {
		return false, err
	}

	t.AllKeys = reqKeys
	//var keys []string
	//for k := range filterKeys {
	//	keys = append(keys, k)
	//}
	t.Anchors = filterKeys
	r.anchors[index] = append(r.anchors[index], filterKeys)

	return dup, nil
}

func (r *Regression) isDupGrpc(ctx context.Context, t *models.GrpcTestCase) (bool, error) {

	reqKeys := map[string][]string{}
	filterKeys := map[string][]string{}

	index, err := r.fillCacheGrpc(ctx, t)
	if err != nil {
		return false, err
	}

	isAnchorChange := true
	for k, v := range reqKeys {
		if !r.noisyFields[index][k] {
			// update field count
			for _, s := range v {
				if _, ok := r.fieldCounts[index][k]; !ok {
					r.fieldCounts[index][k] = map[string]int{}
				}
				r.fieldCounts[index][k][s] = r.fieldCounts[index][k][s] + 1
			}
			if !isAnchor(r.fieldCounts[index][k]) {
				r.noisyFields[index][k] = true
				isAnchorChange = true
				continue
			}
			filterKeys[k] = v
		}
	}

	if len(filterKeys) == 0 {
		return true, nil
	}
	if isAnchorChange {
		err = r.tdb.DeleteByAnchor(ctx, t.CID, t.AppID, t.Method, filterKeys)
		if err != nil {
			return false, err
		}
	}

	// check if testcase based on anchor keys already exists
	dup, err := r.exists(ctx, filterKeys, index)
	if err != nil {
		return false, err
	}

	t.AllKeys = reqKeys
	//var keys []string
	//for k := range filterKeys {
	//	keys = append(keys, k)
	//}
	t.Anchors = filterKeys
	r.anchors[index] = append(r.anchors[index], filterKeys)

	return dup, nil
}

func (r *Regression) exists(_ context.Context, anchors map[string][]string, index string) (bool, error) {
	for _, v := range anchors {
		sort.Strings(v)
	}
	for _, v := range r.anchors[index] {
		if reflect.DeepEqual(v, anchors) {
			return true, nil
		}
	}
	return false, nil

}

func isAnchor(m map[string]int) bool {
	totalCount := 0
	for _, v := range m {
		totalCount = totalCount + v
	}
	// if total values for that field is less than 20 then,
	// the sample size is too small to know if its high variance.
	if totalCount < 20 {
		return true
	}
	// if the unique values are less than 40% of the total value count them,
	// the field is low variant.
	if float64(totalCount)*0.40 > float64(len(m)) {
		return true
	}
	return false
}
