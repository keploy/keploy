package idempotencydb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

var IdempotentMethodsMap = map[string]bool{
	"GET":     true,
	"HEAD":    true,
	"OPTIONS": true,
	"TRACE":   true,
	"PUT":     true,
	"DELETE":  true,
	"POST":    false,
	"PATCH":   false,
}

var CommonNoiseFields = []string{
	"header.Date",
}

// type IRRResponse struct {
// 	StatusCode int                 `json:"statuscode" bson:"statuscode"`
// 	Header     map[string][]string `json:"header" bson:"header"`
// 	Body       string              `json:"body" bson:"body"`
// }

/* IRR stands for Idempontency Request Replayser */

type IRRTestCase struct {
	TestCase     *models.TestCase  `json:"testcase" bson:"testcase"`
	Replay       int               `json:"replay" bson:"replay"`
	IRRResponses []models.HTTPResp `json:"irrresponses" bson:"irrresponses"`
	IRRNoise     IRRDetectedNoise  `json:"irrnoise" bson:"irrnoise"`
}

type IRRDetectedNoise struct {
	NoiseFields map[string][]string `json:"noisefields" bson:"noisefields"`
}

type IRRReportYaml struct {
	IdemReportPath string
	IdemReportName string
	Logger         *zap.Logger
}

func New(logger *zap.Logger, idemReportPath string, idemReportName string) *IRRReportYaml {
	return &IRRReportYaml{
		IdemReportPath: idemReportPath,
		IdemReportName: idemReportName,
		Logger:         logger,
	}
}

var irrTestCases = []IRRTestCase{}

func (irr *IRRReportYaml) StoreReplayResult() {
}

func (irr *IRRReportYaml) ReplayTestCase(ctx context.Context, tc *models.TestCase, testSetID string, replay int) {
	tcsPath := filepath.Join(irr.IdemReportPath, testSetID, "tests")
	lastIndex, err := yaml.FindLastIndex(tcsPath, irr.Logger)
	if err != nil {
		irr.Logger.Error("IRR: error in finding last index", zap.Error(err))
		return
	}

	idemReporFiletPath := filepath.Join(irr.IdemReportPath, testSetID, irr.IdemReportName)

	if lastIndex == 1 {
		_, err := os.Create(idemReporFiletPath)
		if err != nil {
			irr.Logger.Error("IRR: error in creating idempotency report file", zap.Error(err))
			return
		}
	}

	f, err := os.OpenFile(idemReporFiletPath, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		irr.Logger.Error("IRR: error in opening idempotency report file", zap.Error(err))
		return
	}
	defer f.Close()

	tc.Name = fmt.Sprintf("test-%d", lastIndex)

	httpResponses := []models.HTTPResp{}
	httpResponses = append(httpResponses, tc.HTTPResp)

	irrTestCase := &IRRTestCase{
		TestCase:     tc,
		Replay:       replay,
		IRRResponses: []models.HTTPResp{},
	}

	//------------------Replay TestCase Request-------------------
	client := &http.Client{}

	req, err := http.NewRequestWithContext(ctx, string(tc.HTTPReq.Method), tc.HTTPReq.URL, strings.NewReader(tc.HTTPReq.Body))
	if err != nil {
		irr.Logger.Error("IRR: failed to create HTTP request", zap.Error(err))
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
			irr.Logger.Error("IRR: failed to execute HTTP request", zap.Error(err))
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			irr.Logger.Error("IRR: failed to read response body", zap.Error(err))
			return
		}

		headers := make(map[string]string)
		for k, v := range resp.Header {
			if len(v) > 0 {
				headers[k] = v[0]
			}
		}

		irResponse := models.HTTPResp{
			StatusCode:    resp.StatusCode,
			Header:        headers,
			Body:          string(respBody),
			StatusMessage: resp.Status,
			ProtoMajor:    resp.ProtoMajor,
			ProtoMinor:    resp.ProtoMinor,
			Timestamp:     time.Now(),
		}

		httpResponses = append(httpResponses, irResponse)
		irrTestCase.IRRResponses = append(irrTestCase.IRRResponses, irResponse)

		replay--
	}

	detectedNoise := CompareResponses(httpResponses, irr.Logger)
	irrTestCase.IRRNoise = detectedNoise

	irrTestCases = append(irrTestCases, *irrTestCase)

	data, err := yamlLib.Marshal(&irrTestCases)
	if err != nil {
		irr.Logger.Error("IRR: error in marshalling the updated test case", zap.Error(err))
		return
	}

	_, err = f.Write(data)
	if err != nil {
		irr.Logger.Error("IRR: error writing updated test case", zap.Error(err))
	}

	tc.Noise = detectedNoise.NoiseFields
}

func (irr *IRRReportYaml) CheckReplayHeader(tc *models.TestCase) bool {
	if _, ok := tc.HTTPReq.Header["Idempotency-Replay"]; ok {
		return true
	}
	return false
}
