package idempotencydb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

var IdempotentMethodMap = map[string]bool{
	"GET":     true,
	"HEAD":    true,
	"OPTIONS": true,
	"TRACE":   true,
	"PUT":     true,
	"DELETE":  true,
	"POST":    false,
	"PATCH":   false,
}

var IdempotencyTestCases = []IdempotencyTestCase{}

type IRResponse struct {
	StatusCode int                 `json:"statuscode" bson:"statuscode"`
	Header     map[string][]string `json:"header" bson:"header"`
	Body       string              `json:"body" bson:"body"`
}

type IdempotencyTestCase struct {
	TestCase   *models.TestCase `json:"testcase" bson:"testcase"`
	Replay     int              `json:"replay" bson:"replay"`
	IRResponse []IRResponse     `json:"irresponse" bson:"irresponse"`
}

type IdempotencyReportYaml struct {
	IdemReportPath string
	IdemReportName string
	Logger         *zap.Logger
}

func New(logger *zap.Logger, idemReportPath string, idemReportName string) *IdempotencyReportYaml {
	return &IdempotencyReportYaml{
		IdemReportPath: idemReportPath,
		IdemReportName: idemReportName,
		Logger:         logger,
	}
}

func (ir *IdempotencyReportYaml) StoreReplayResult() {
}

func (ir *IdempotencyReportYaml) ReplayTestCase(ctx context.Context, tc *models.TestCase, testSetID string, replay int) {
	tcsPath := filepath.Join(ir.IdemReportPath, testSetID, "tests")
	lastIndex, err := yaml.FindLastIndex(tcsPath, ir.Logger)
	if err != nil {
		ir.Logger.Error("error in finding last index", zap.Error(err))
		return
	}

	idemReporFiletPath := filepath.Join(ir.IdemReportPath, testSetID, ir.IdemReportName)

	if lastIndex == 1 {
		_, err := os.Create(idemReporFiletPath)
		if err != nil {
			ir.Logger.Error("error in creating idempotency report file", zap.Error(err))
			return
		}
	}

	f, err := os.OpenFile(idemReporFiletPath, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		ir.Logger.Error("error in opening idempotency report file", zap.Error(err))
		return
	}
	defer f.Close()

	tc.Name = fmt.Sprintf("test-%d", lastIndex)

	irTest := &IdempotencyTestCase{
		TestCase:   tc,
		Replay:     replay,
		IRResponse: []IRResponse{},
	}

	// IdempotencyTestCases = append(IdempotencyTestCases, *irTest)

	// data, err := yamlLib.Marshal(&IdempotencyTestCases)
	// if err != nil {
	// 	ir.Logger.Error("error in marshalling the testcase", zap.Error(err))
	// 	return
	// }

	// f.Write(data)

	//------------------Replay TestCase Request-------------------
	client := &http.Client{}

	req, err := http.NewRequestWithContext(ctx, string(tc.HTTPReq.Method), tc.HTTPReq.URL, strings.NewReader(tc.HTTPReq.Body))
	if err != nil {
		ir.Logger.Error("failed to create HTTP request", zap.Error(err))
		return
	}

	for key, values := range tc.HTTPReq.Header {
		for _, value := range values {
			req.Header.Add(key, string(value))
		}
	}

	// Add special header to indicate with value of the test number.
	req.Header.Add("Idempotency-Replay", fmt.Sprintf("%d", lastIndex))

	for replay > 0 {
		resp, err := client.Do(req)
		if err != nil {
			ir.Logger.Error("failed to execute HTTP request", zap.Error(err))
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			ir.Logger.Error("failed to read response body", zap.Error(err))
			return
		}

		irResponse := IRResponse{
			StatusCode: resp.StatusCode,
			Header:     resp.Header,
			Body:       string(respBody),
		}

		irTest.IRResponse = append(irTest.IRResponse, irResponse)

		// data, err = yamlLib.Marshal(&IdempotencyTestCases)
		// if err != nil {
		// 	ir.Logger.Error("error in marshalling the updated test case", zap.Error(err))
		// 	return
		// }

		// f, err = os.OpenFile(idemReporFiletPath, os.O_WRONLY|os.O_TRUNC, 0644)
		// if err != nil {
		// 	ir.Logger.Error("error reopening idempotency report file", zap.Error(err))
		// 	return
		// }

		// _, err = f.Write(data)
		// if err != nil {
		// 	ir.Logger.Error("error writing updated test case", zap.Error(err))
		// }
		replay--
	}

	IdempotencyTestCases = append(IdempotencyTestCases, *irTest)

	data, err := yamlLib.Marshal(&IdempotencyTestCases)
	if err != nil {
		ir.Logger.Error("error in marshalling the updated test case", zap.Error(err))
		return
	}

	_, err = f.Write(data)
	if err != nil {
		ir.Logger.Error("error writing updated test case", zap.Error(err))
	}
}

func (ir *IdempotencyReportYaml) CheckReplayHeader(tc *models.TestCase) bool {
	if _, ok := tc.HTTPReq.Header["Idempotency-Replay"]; ok {
		return true
	}
	return false
}
