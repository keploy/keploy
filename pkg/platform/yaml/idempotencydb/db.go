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

// type IRRResponse struct {
// 	StatusCode int                 `json:"statuscode" bson:"statuscode"`
// 	Header     map[string][]string `json:"header" bson:"header"`
// 	Body       string              `json:"body" bson:"body"`
// }

/* IRR stands for Idempontency Request Replayer */

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

var irrReport = []IRRTestCase{}

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

	tc.Name = fmt.Sprintf("test-%d", lastIndex)

	httpResponses := []models.HTTPResp{}
	httpResponses = append(httpResponses, tc.HTTPResp)

	irrTestCase := &IRRTestCase{
		TestCase:     tc,
		Replay:       replay,
		IRRResponses: []models.HTTPResp{},
	}

	/* ------------------Replay TestCase Request------------------- */
	client := &http.Client{}

	req, err := http.NewRequestWithContext(ctx, string(tc.HTTPReq.Method), tc.HTTPReq.URL, strings.NewReader(tc.HTTPReq.Body))
	if err != nil {
		irr.Logger.Error("IRR: failed to create HTTP request", zap.Error(err))
		return
	}

	for key, value := range tc.HTTPReq.Header {
		req.Header.Add(key, value)
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

	irrReport = append(irrReport, *irrTestCase)

	SaveIRRReport(&irrReport, idemReporFiletPath, irr.Logger)

	tc.Noise = detectedNoise.NoiseFields
}

func (irr *IRRReportYaml) CheckReplayHeader(tc *models.TestCase) bool {
	if _, ok := tc.HTTPReq.Header["Idempotency-Replay"]; ok {
		return true
	}
	return false
}

//--------------------------------------------------------------------------------------------------------------------------------------

// A list of dynamic header to be configured.
// allow the user to modify/update or add new of these tokens/headers in the config file.
var DynamicHeaders = []string{
	"Date",
	"X-Request-ID",
	"X-Forwarded-For",
	"User-Agent",
}

// A list of session related tokens to be configured.
// allow the user to modify/update or add new of these tokens/headers in the config file.
var SessionTokensHeaders = []string{
	// request headers
	"Authorization",
	"Cookie",
}

type IRRConfig struct {
	DynamicHeaders       map[string]string `json:"dynamicHeaders" bson:"dynamicHeaders"`
	SessionTokensHeaders map[string]string `json:"sessionTokens" bson:"sessionTokens"`
}

var irrconfig = IRRConfig{
	DynamicHeaders:       make(map[string]string),
	SessionTokensHeaders: make(map[string]string),
}

func (irr *IRRReportYaml) StoreDynamicHeaders(ctx context.Context, tc *models.TestCase, testSetID string) {
	configPath := filepath.Join(irr.IdemReportPath, testSetID, "irrconfig.yaml")

	for _, header := range DynamicHeaders {
		if _, ok := tc.HTTPReq.Header[header]; ok {
			irr.Logger.Warn("IRR: dynamic header detected, make sure to update/ignore it through test-set config file before testing.", zap.String("header", header))
			irrconfig.DynamicHeaders[header] = tc.HTTPReq.Header[header]
		}
	}

	for _, token := range SessionTokensHeaders {
		if _, ok := tc.HTTPReq.Header[token]; ok {
			irr.Logger.Warn("IRR: session token detected, make sure to update/ignore it through test-set config file before testing.", zap.String("token", token))
			irrconfig.SessionTokensHeaders[token] = tc.HTTPReq.Header[token]
		}
	}

	SaveConfig(irrconfig, configPath, irr.Logger)
}

/*
- Implement a way to store dynamic headers along side every test-set as a config file.
- Make it easy for the user to update/ignore certain dynamic headers which may lead to flaky testing.
- When recording give a warning about give the user the option to ignore/update these dynamic headers in the config file or leave them as it is.
- Also this can work for session related tokens as well, but will put more work on the user to update these tokens.
*/

/*
- Implement a way to store session related tokens/headers along side every test-set.
- To avoid failed tests when these session related tokens/headers expires,
	-- we can have a way to update these tokens/headers by running the test-case responsible of these tokens/headers.
	-- this will make it easier for the user to avoid making changes in his api development enviroment only for testing purposes.
*/
