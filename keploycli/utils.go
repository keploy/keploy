package keploycli

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/keploy/go-sdk/pkg/keploy"
	"go.keploy.io/server/config"
	"go.keploy.io/server/graph"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/service"
	"go.uber.org/zap"
)

type testInput struct {
	ctx        context.Context
	conf       *config.Config
	testExport bool
	appId      string
	tcsPath    string
	mockPath   string
	host       string
	kServices  *service.KServices
	logger     *zap.Logger
}

// fetch is a generic method to fetch the tcs either from mongo or local yamls
func fetch(t testInput) ([]models.TestCase, error) {
	var tcs []models.TestCase = []models.TestCase{}

	pageSize := 25
	for i := 0; ; i += pageSize {
		res, err := t.kServices.TestcaseSrv.GetAll(t.ctx, graph.DEFAULT_COMPANY, t.appId, &i, &pageSize, t.tcsPath, t.mockPath)
		if err != nil {
			return nil, err
		}
		tcs = append(tcs, res...)
		// since, GetAll returns all yaml tcs and paginnated bson tcs
		if t.testExport || (!t.testExport && len(res) < pageSize) {
			break
		}
	}

	for i, j := range tcs {
		if strings.Contains(strings.Join(j.HttpReq.Header["Content-Type"], ", "), "multipart/form-data") {
			bin, err := base64.StdEncoding.DecodeString(j.HttpReq.Body)
			if err != nil {
				t.logger.Error("failed to decode the base64 encoded request body", zap.Error(err))
				return nil, err
			}
			tcs[i].HttpReq.Body = string(bin)
		}
	}
	return tcs, nil

}

// test
func testRun(t testInput) {

	tcs, err := fetch(t)
	if err != nil {
		t.logger.Error("failed to fetch testcases", zap.Error(err))
		return
	}
	total := len(tcs)

	// start a http test run
	id := uuid.New().String()
	now := time.Now().Unix()
	err = t.kServices.RegressionSrv.PutTest(t.ctx, models.TestRun{
		ID:      id,
		Created: now,
		Updated: now,
		Status:  models.TestRunStatusRunning,
		CID:     graph.DEFAULT_COMPANY,
		App:     t.appId,
		User:    graph.DEFAULT_USER,
		Total:   total,
	}, t.testExport, id, t.tcsPath, t.mockPath, t.conf.ReportPath, total)
	if err != nil {
		t.logger.Error("failed to start test run", zap.Error(err))
		return
	}

	t.logger.Info("starting test execution", zap.String("id", id), zap.Int("total tests", total))
	passed := true
	passedMutex := sync.Mutex{}
	// call the service for each test case
	var wg sync.WaitGroup
	maxGoroutines := 10
	guard := make(chan struct{}, maxGoroutines)
	for i, tc := range tcs {
		t.logger.Info(fmt.Sprintf("testing %d of %d", i+1, total), zap.String("testcase id", tc.ID))
		guard <- struct{}{}
		wg.Add(1)
		tcCopy := tc
		go func() {
			ok := check(t, id, t.host, tcCopy, t.logger)
			if !ok {
				passedMutex.Lock()
				passed = false
				passedMutex.Unlock()
			}
			t.logger.Info("result", zap.String("testcase id", tcCopy.ID), zap.Bool("passed", ok))
			<-guard
			wg.Done()
		}()
	}
	wg.Wait()

	// end the http test run
	now = time.Now().Unix()
	stat := models.TestRunStatusFailed
	if passed {
		stat = models.TestRunStatusPassed
	}
	t.kServices.RegressionSrv.PutTest(t.ctx, models.TestRun{
		ID:      id,
		Updated: now,
		Status:  stat,
	}, t.testExport, id, t.tcsPath, t.mockPath, t.conf.ReportPath, total)
	if err != nil {
		t.logger.Error("failed to end test run", zap.Error(err))
		return
	}
	t.logger.Info("test run completed", zap.String("run id", id), zap.Bool("passed overall", passed))

}

func check(t testInput, runId, host string, tc models.TestCase, logger *zap.Logger) bool {
	resp, err := simulate(tc, host, http.Client{}, logger)
	if err != nil {
		logger.Error("failed to simulate request on local server", zap.Error(err))
		return false
	}

	pass, err := t.kServices.RegressionSrv.Test(t.ctx, graph.DEFAULT_COMPANY, t.appId, runId, tc.ID, t.tcsPath, t.mockPath, *resp)
	if err != nil {
		logger.Error("failed to run the recorded test", zap.String("testcase with id", tc.ID), zap.Error(err))
		return false
	}
	return pass
}

func simulate(tc models.TestCase, host string, client http.Client, logger *zap.Logger) (*models.HttpResp, error) {
	ctx := context.WithValue(context.Background(), keploy.KTime, tc.Captured)
	req, err := http.NewRequestWithContext(ctx, string(tc.HttpReq.Method), "http://"+host+tc.HttpReq.URL, bytes.NewBufferString(tc.HttpReq.Body))
	if err != nil {
		logger.Error("failed to create a http request", zap.String("for tcs with id: ", tc.ID), zap.Error(err))
		return nil, err
	}
	req.Header = tc.HttpReq.Header
	req.Header.Set("KEPLOY_TEST_ID", tc.ID)
	req.Header.Set("KEPLOY_CLIENT", "keploy-server")
	req.ProtoMajor = tc.HttpReq.ProtoMajor
	req.ProtoMinor = tc.HttpReq.ProtoMinor
	req.Close = true

	httpresp, err := client.Do(req)
	if err != nil {
		logger.Error("failed sending testcase request to app", zap.String("for tcs with id: ", tc.ID), zap.Error(err))
		return nil, err
	}

	respBody, err := ioutil.ReadAll(httpresp.Body)
	if err != nil {
		logger.Error("failed reading simulated response from app", zap.String("for tcs with id: ", tc.ID), zap.Error(err))
		return nil, err
	}
	return &models.HttpResp{
		StatusCode:    httpresp.StatusCode,
		Header:        httpresp.Header,
		Body:          string(respBody),
		StatusMessage: httpresp.Status,
		ProtoMajor:    httpresp.ProtoMajor,
		ProtoMinor:    httpresp.ProtoMinor,
	}, nil
}
