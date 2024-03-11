package connection

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/hooks/structs"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform"
	"go.keploy.io/server/utils"
)

var Emoji = "\U0001F430" + " Keploy:"

// Factory is a routine-safe container that holds a trackers with unique ID, and able to create new tracker.
type Factory struct {
	connections         map[structs.ConnID]*Tracker
	inactivityThreshold time.Duration
	mutex               *sync.RWMutex
	logger              *zap.Logger
}

// NewFactory creates a new instance of the factory.
func NewFactory(inactivityThreshold time.Duration, logger *zap.Logger) *Factory {
	return &Factory{
		connections:         make(map[structs.ConnID]*Tracker),
		mutex:               &sync.RWMutex{},
		inactivityThreshold: inactivityThreshold,
		logger:              logger,
	}
}

// ProcessActiveTrackers iterates over all connection the trackers and checks if they are complete. If so, it captures the ingress call and
// deletes the tracker. If the tracker is inactive for a long time, it deletes it.
func (factory *Factory) ProcessActiveTrackers(db platform.TestCaseDB, ctx context.Context, filters *models.TestFilter, autoNoiseCfg *models.AutoNoiseConfig) {
	factory.mutex.Lock()
	defer factory.mutex.Unlock()
	var trackersToDelete []structs.ConnID
	for connID, tracker := range factory.connections {
		ok, requestBuf, responseBuf, reqTimestampTest, resTimestampTest := tracker.IsComplete()
		if ok {

			if len(requestBuf) == 0 || len(responseBuf) == 0 {
				factory.logger.Warn("failed processing a request due to invalid request or response", zap.Any("Request Size", len(requestBuf)), zap.Any("Response Size", len(responseBuf)))
				continue
			}

			parsedHttpReq, err := pkg.ParseHTTPRequest(requestBuf)
			if err != nil {
				factory.logger.Error("failed to parse the http request from byte array", zap.Error(err), zap.Any("requestBuf", requestBuf))
				continue
			}
			parsedHttpRes, err := pkg.ParseHTTPResponse(responseBuf, parsedHttpReq)
			if err != nil {
				factory.logger.Error("failed to parse the http response from byte array", zap.Error(err))
				continue
			}

			switch models.GetMode() {
			case models.MODE_RECORD:
				// capture the ingress call for record cmd
				factory.logger.Debug("capturing ingress call from tracker in record mode")
				capture(db, parsedHttpReq, parsedHttpRes, factory.logger, ctx, reqTimestampTest, resTimestampTest, filters, autoNoiseCfg)
			case models.MODE_TEST:
				factory.logger.Debug("skipping tracker in test mode")
			default:
				factory.logger.Warn("Keploy mode is not set to record or test. Tracker is being skipped.",
					zap.Any("current mode", models.GetMode()))
			}

		} else if tracker.IsInactive(factory.inactivityThreshold) {
			trackersToDelete = append(trackersToDelete, connID)
		}
	}

	// Delete all the processed trackers.
	for _, key := range trackersToDelete {
		delete(factory.connections, key)
	}
}

// GetOrCreate returns a tracker that related to the given connection and transaction ids. If there is no such tracker
// we create a new one.
func (factory *Factory) GetOrCreate(connectionID structs.ConnID) *Tracker {
	factory.mutex.Lock()
	defer factory.mutex.Unlock()
	tracker, ok := factory.connections[connectionID]
	if !ok {
		factory.connections[connectionID] = NewTracker(connectionID, factory.logger)
		return factory.connections[connectionID]
	}
	return tracker
}

func capture(db platform.TestCaseDB, req *http.Request, resp *http.Response, logger *zap.Logger, ctx context.Context, reqTimeTest time.Time, resTimeTest time.Time, filters *models.TestFilter, autoNoiseCfg *models.AutoNoiseConfig) {
	reqBody, err := io.ReadAll(req.Body)
	if err != nil {
		logger.Error("failed to read the http request body", zap.Error(err))
		return
	}

	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error("failed to read the http response body", zap.Error(err))
		return
	}

	testCase := models.TestCase{
		Version: models.GetVersion(),
		Name:    pkg.ToYamlHttpHeader(req.Header)["Keploy-Test-Name"],
		Kind:    models.HTTP,
		Created: time.Now().Unix(),
		HttpReq: models.HttpReq{
			Method:     models.Method(req.Method),
			ProtoMajor: req.ProtoMajor,
			ProtoMinor: req.ProtoMinor,
			// URL:        req.URL.String(),
			// URL: fmt.Sprintf("%s://%s%s?%s", req.URL.Scheme, req.Host, req.URL.Path, req.URL.RawQuery),
			URL: fmt.Sprintf("http://%s%s", req.Host, req.URL.RequestURI()),
			//  URL: string(b),
			Header:    pkg.ToYamlHttpHeader(req.Header),
			Body:      string(reqBody),
			URLParams: pkg.UrlParams(req),
			Timestamp: reqTimeTest,
		},
		HttpResp: models.HttpResp{
			StatusCode:    resp.StatusCode,
			Header:        pkg.ToYamlHttpHeader(resp.Header),
			Body:          string(respBody),
			Timestamp:     resTimeTest,
			StatusMessage: http.StatusText(resp.StatusCode),
		},
		Noise:     map[string][]string{},
		AutoNoise: map[string][]string{},
		// Mocks: mocks,
	}

	// check if the incoming request is simulation or not
	if testCase.HttpReq.Header["Keploy-Simulation"] != "true" {
		autoNoise := map[string][]string{}

		// start the simulation if autoNoise is enabled
		if autoNoiseCfg.EnableAutoNoise {
			testCase.HttpReq.Header["Keploy-Simulation"] = "true"

			// check if the user application is running docker container using IDE
			dockerID := (autoNoiseCfg.AppCmd == "" && len(autoNoiseCfg.AppContainer) != 0)
			ok, _ := utils.IsDockerRelatedCmd(autoNoiseCfg.AppCmd)
			if ok || dockerID {
				var err error
				testCase.HttpReq.URL, err = pkg.ReplaceHostToIP(testCase.HttpReq.URL, autoNoiseCfg.UserIP)
				if err != nil {
					logger.Error("failed to replace host to docker container's IP", zap.Error(err))
				}
				logger.Debug("", zap.Any("replaced URL in case of docker env", testCase.HttpReq.URL))
			}

			testRes, err := pkg.SimulateHttp(testCase, "", logger, 10)
			if err != nil && resp == nil {
				logger.Info("result", zap.Any("testcase id", models.HighlightFailingString(testCase.Name)), zap.Any("passed", models.HighlightFailingString("false")))
			} else {
				// checking the difference in label values in request and response
				ok, diff := utils.TestHttp(testCase, testRes, models.GlobalNoise{}, true, logger, &sync.Mutex{}, false)
				if !ok {

					// checking for header and body noise
					headerNoise := []string{}
					bodyNoise := []string{}
					var (
						bodyExpected interface{}
						bodyActual   interface{}
					)

					for _, j := range diff.HeadersResult {
						if !j.Normal {
							headerNoise = append(headerNoise, j.Actual.Key)
						}
					}

					bodyRes := diff.BodyResult[0]

					if !bodyRes.Normal && bodyRes.Type == models.BodyTypeJSON {

						err1 := json.Unmarshal([]byte(bodyRes.Expected), &bodyExpected)
						err2 := json.Unmarshal([]byte(bodyRes.Actual), &bodyActual)

						if err1 != nil || err2 != nil {
							logger.Error("response are not json", zap.Error(err1), zap.Error(err2))
						}

						bodyNoise = findNoisyLabels(bodyExpected, bodyActual, logger)
					}
					autoNoise["header"] = headerNoise
					autoNoise["body"] = bodyNoise
				}
			}
		}

		testCase.AutoNoise = autoNoise
		err = db.WriteTestcase(&testCase, ctx, filters)
		if err != nil {
			logger.Error("failed to record the ingress requests", zap.Error(err))
			return
		}
	}
}
