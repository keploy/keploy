// Package telemetry provides functionality for telemetry data collection.
package telemetry

import (
	"bytes"
	"net/http"
	"runtime"
	"sync"
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
	dataMap := &sync.Map{}
	dataMap.Store("Passed-Tests", success)
	dataMap.Store("Failed-Tests", failure)
	dataMap.Store("Test-Set", testSet)
	dataMap.Store("Run-Status", runStatus)
	go tel.SendTelemetry("TestSetRun", dataMap)
}

func (tel *Telemetry) TestRun(success int, failure int, testSets int, runStatus string) {
	dataMap := &sync.Map{}
	dataMap.Store("Passed-Tests", success)
	dataMap.Store("Failed-Tests", failure)
	dataMap.Store("Test-Sets", testSets)
	dataMap.Store("Run-Status", runStatus)
	go tel.SendTelemetry("TestRun", dataMap)
}

// MockTestRun is Telemetry event for the Mocking feature test run
func (tel *Telemetry) MockTestRun(utilizedMocks int) {
	dataMap := &sync.Map{}
	dataMap.Store("Utilized-Mocks", utilizedMocks)
	go tel.SendTelemetry("MockTestRun", dataMap)
}

// RecordedTestSuite is Telemetry event for the tests and mocks that are recorded
func (tel *Telemetry) RecordedTestSuite(testSet string, testsTotal int, mockTotal map[string]int) {
	dataMap := &sync.Map{}
	dataMap.Store("test-set", testSet)
	dataMap.Store("tests", testsTotal)

	mockMap := &sync.Map{}
	for k, v := range mockTotal {
		mockMap.Store(k, v)
	}
	dataMap.Store("mocks", mockMap)

	go tel.SendTelemetry("RecordedTestSuite", dataMap)
}

func (tel *Telemetry) RecordedTestAndMocks() {
	dataMap := &sync.Map{}
	mapcheck := make(map[string]int)
	dataMap.Store("mocks", mapcheck) // Storing 0 instead of an empty map
	go tel.SendTelemetry("RecordedTestAndMocks", dataMap)
}

func (tel *Telemetry) GenerateUT() {
	dataMap := &sync.Map{}
	go tel.SendTelemetry("GenerateUT", dataMap)
}

// RecordedMocks is Telemetry event for the mocks that are recorded in the mocking feature
func (tel *Telemetry) RecordedMocks(mockTotal map[string]int) {
	mockMap := &sync.Map{}
	for k, v := range mockTotal {
		mockMap.Store(k, v)
	}
	dataMap := &sync.Map{}
	dataMap.Store("mocks", mockMap)
	go tel.SendTelemetry("RecordedMocks", dataMap)
}

func (tel *Telemetry) RecordedTestCaseMock(mockType string) {
	dataMap := &sync.Map{}
	dataMap.Store("mock", mockType)
	go tel.SendTelemetry("RecordedTestCaseMock", dataMap)
}

func (tel *Telemetry) SendTelemetry(eventType string, output ...*sync.Map) {
	if tel.Enabled {
		event := models.TeleEvent{
			EventType: eventType,
			CreatedAt: time.Now().Unix(),
		}
		if len(output) > 0 {
			event.Meta = output[0]
		} else {
			event.Meta = &sync.Map{}
		}

		hasGlobalMap := false
        tel.GlobalMap.Range(func(key, value interface{}) bool {
            hasGlobalMap = true
            return false // Stop iteration after finding the first element
        })

        if hasGlobalMap {
			// event.Meta["global-map"] = syncMapToMap(tel.GlobalMap)
			// If you want to nest the global map, you can do this (but the telemetry
			// endpoint needs to support nested sync.Maps):
			// event.Meta.Store("global-map", tel.GlobalMap)
			// Otherwise, merge the global map into the event's meta map
			tel.GlobalMap.Range(func(key, value interface{}) bool {
				event.Meta.Store(key, value)
				return true
			})
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
