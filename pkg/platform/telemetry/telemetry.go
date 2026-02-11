// Package telemetry provides functionality for telemetry data collection.
package telemetry

import (
	"bytes"
	"net/http"
	"runtime"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

var teleURL = "https://telemetry.keploy.io/analytics"

type Telemetry struct {
	Enabled        bool
	OffMode        bool
	logger         *zap.Logger
	InstallationID string
	KeployVersion  string
	GlobalMap      sync.Map
	client         *http.Client
}

type Options struct {
	Enabled        bool
	Version        string
	GlobalMap      sync.Map
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
		for {
			tel.SendTelemetry("Ping")
			time.Sleep(5 * time.Minute)
		}
	}()
}

func (tel *Telemetry) TestSetRun(success int, failure int, testSet string, runStatus string) {
	dataMap := map[string]interface{}{
		"Passed-Tests": success,
		"Failed-Tests": failure,
		"Test-Set":     testSet,
		"Run-Status":   runStatus,
	}
	go tel.SendTelemetry("TestSetRun", dataMap)
}

func (tel *Telemetry) TestRun(success int, failure int, testSets int, runStatus string, metadata map[string]interface{}) {
	dataMap := map[string]interface{}{
		"Passed-Tests": success,
		"Failed-Tests": failure,
		"Test-Sets":    testSets,
		"Run-Status":   runStatus,
	}
	for k, v := range metadata {
		dataMap[k] = v
	}
	go tel.SendTelemetry("TestRun", dataMap)
}

// MockTestRun is Telemetry event for the Mocking feature test run
func (tel *Telemetry) MockTestRun(utilizedMocks int) {
	dataMap := map[string]interface{}{
		"Utilized-Mocks": utilizedMocks,
	}
	go tel.SendTelemetry("MockTestRun", dataMap)
}

// RecordedTestSuite is Telemetry event for the tests and mocks that are recorded
func (tel *Telemetry) RecordedTestSuite(testSet string, testsTotal int, mockTotal map[string]int, metadata map[string]interface{}) {
	mockMap := make(map[string]interface{}, len(mockTotal))
	for k, v := range mockTotal {
		mockMap[k] = v
	}
	dataMap := map[string]interface{}{
		"test-set": testSet,
		"tests":    testsTotal,
		"mocks":    mockMap,
	}
	for k, v := range metadata {
		dataMap[k] = v
	}
	go tel.SendTelemetry("RecordedTestSuite", dataMap)
}

func (tel *Telemetry) RecordedTestAndMocks() {
	dataMap := map[string]interface{}{
		"mocks": make(map[string]int),
	}
	go tel.SendTelemetry("RecordedTestAndMocks", dataMap)
}

func (tel *Telemetry) GenerateUT() {
	go tel.SendTelemetry("GenerateUT")
}

// RecordedMocks is Telemetry event for the mocks that are recorded in the mocking feature
func (tel *Telemetry) RecordedMocks(mockTotal map[string]int) {
	mockMap := make(map[string]interface{}, len(mockTotal))
	for k, v := range mockTotal {
		mockMap[k] = v
	}
	dataMap := map[string]interface{}{
		"mocks": mockMap,
	}
	go tel.SendTelemetry("RecordedMocks", dataMap)
}

func (tel *Telemetry) RecordedTestCaseMock(mockType string) {
	dataMap := map[string]interface{}{
		"mock": mockType,
	}
	go tel.SendTelemetry("RecordedTestCaseMock", dataMap)
}

func (tel *Telemetry) SendTelemetry(eventType string, output ...map[string]interface{}) {
	if tel.Enabled {
		event := models.TeleEvent{
			EventType: eventType,
			CreatedAt: time.Now().Unix(),
		}
		if len(output) > 0 {
			event.Meta = output[0]
		} else {
			event.Meta = map[string]interface{}{}
		}

		// Merge global map entries into event meta
		tel.GlobalMap.Range(func(key, value interface{}) bool {
			if k, ok := key.(string); ok {
				event.Meta[k] = value
			}
			return true
		})

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
