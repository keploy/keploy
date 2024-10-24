// Package telemetry provides functionality for telemetry data collection.
package telemetry

import (
	"bytes"
	"fmt"
	"net/http"
	"runtime"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

var teleURL = "https://telemetry.keploy.io/analytics"

type Telemetry struct {
	Enabled        bool
	OffMode        bool
	logger         *zap.Logger
	InstallationID string
	KeployVersion  string
	GlobalMap      map[string]interface{}
	client         *http.Client
}

type Options struct {
	Enabled        bool
	Version        string
	GlobalMap      map[string]interface{}
	InstallationID string
}

func NewTelemetry(logger *zap.Logger, opt Options) *Telemetry {
	return &Telemetry{
		Enabled:        opt.Enabled,
		logger:         logger,
		KeployVersion:  opt.Version,
		GlobalMap:      opt.GlobalMap,
		InstallationID: opt.InstallationID,
		client:         &http.Client{Timeout: 10 * time.Second},
	}
}

func (tel *Telemetry) Ping() {
	if !tel.Enabled {
		return
	}
	go func() {
		time.Sleep(10 * time.Second)
		for {
			tel.SendTelemetry("Ping")
			time.Sleep(5 * time.Second)
		}
	}()
}

func (tel *Telemetry) TestSetRun(success int, failure int, testSet string, runStatus string) {
	go tel.SendTelemetry("TestSetRun", map[string]interface{}{"Passed-Tests": success, "Failed-Tests": failure, "Test-Set": testSet, "Run-Status": runStatus})
}

func (tel *Telemetry) TestRun(success int, failure int, testSets int, runStatus string) {
	go tel.SendTelemetry("TestRun", map[string]interface{}{"Passed-Tests": success, "Failed-Tests": failure, "Test-Sets": testSets, "Run-Status": runStatus})
}

// MockTestRun is Telemetry event for the Mocking feature test run
func (tel *Telemetry) MockTestRun(utilizedMocks int) {
	go tel.SendTelemetry("MockTestRun", map[string]interface{}{"Utilized-Mocks": utilizedMocks})
}

// RecordedTestSuite is Telemetry event for the tests and mocks that are recorded
func (tel *Telemetry) RecordedTestSuite(testSet string, testsTotal int, mockTotal map[string]int) {
	go tel.SendTelemetry("RecordedTestSuite", map[string]interface{}{"test-set": testSet, "tests": testsTotal, "mocks": mockTotal})
}

func (tel *Telemetry) RecordedTestAndMocks() {
	go tel.SendTelemetry("RecordedTestAndMocks", map[string]interface{}{"mocks": make(map[string]int)})
}

func (tel *Telemetry) GenerateUT() {
	go tel.SendTelemetry("GenerateUT")
}

// RecordedMocks is Telemetry event for the mocks that are recorded in the mocking feature
func (tel *Telemetry) RecordedMocks(mockTotal map[string]int) {
	go tel.SendTelemetry("RecordedMocks", map[string]interface{}{"mocks": mockTotal})
}

func (tel *Telemetry) RecordedTestCaseMock(mockType string) {
	go tel.SendTelemetry("RecordedTestCaseMock", map[string]interface{}{"mock": mockType})
}

func (tel *Telemetry) SendTelemetry(eventType string, output ...map[string]interface{}) {
	if tel.Enabled {
		event := models.TeleEvent{
			EventType: eventType,
			CreatedAt: time.Now().Unix(),
		}
		event.Meta = make(map[string]interface{})
		if len(output) != 0 {
			event.Meta = output[0]
		}

		if tel.GlobalMap != nil {
			event.Meta["global-map"] = tel.GlobalMap
		}

		event.InstallationID = tel.InstallationID
		event.OS = runtime.GOOS
		event.KeployVersion = tel.KeployVersion
		event.Arch = runtime.GOARCH
		bin, err := marshalEvent(event, tel.logger)
		if err != nil {
			tel.logger.Debug("failed to marshal event", zap.Error(err))
			return
		}

		req, err := http.NewRequest(http.MethodPost, teleURL, bytes.NewBuffer(bin))
		if err != nil {
			tel.logger.Debug("failed to create request for analytics", zap.Error(err))
			return
		}

		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		fmt.Println("pingign")
		resp, err := tel.client.Do(req)
		if err != nil {
			tel.logger.Debug("failed to send request for analytics", zap.Error(err))
			return
		}
		_, err = unmarshalResp(resp, tel.logger)
		if err != nil {
			tel.logger.Debug("failed to unmarshal response", zap.Error(err))
			return
		}
	}
}
