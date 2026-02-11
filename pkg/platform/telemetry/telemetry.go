package telemetry

import (
	"bytes"
	"context"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
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
	GlobalMap      *sync.Map
	client         *http.Client
	inflight       sync.WaitGroup
	closed         atomic.Bool
}

type Options struct {
	Enabled        bool
	Version        string
	GlobalMap      *sync.Map
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

func (tel *Telemetry) Ping(ctx context.Context) {
	if !tel.Enabled {
		return
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		tel.SendTelemetry("Ping")
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tel.SendTelemetry("Ping")
			}
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
	tel.SendTelemetry("TestSetRun", dataMap)
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
	tel.SendTelemetry("TestRun", dataMap)
}

func (tel *Telemetry) MockTestRun(utilizedMocks int) {
	dataMap := map[string]interface{}{
		"Utilized-Mocks": utilizedMocks,
	}
	tel.SendTelemetry("MockTestRun", dataMap)
}

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
	tel.SendTelemetry("RecordedTestSuite", dataMap)
}

func (tel *Telemetry) RecordedTestAndMocks() {
	dataMap := map[string]interface{}{
		"mocks": make(map[string]int),
	}
	tel.SendTelemetry("RecordedTestAndMocks", dataMap)
}

func (tel *Telemetry) GenerateUT() {
	tel.SendTelemetry("GenerateUT")
}

func (tel *Telemetry) RecordedMocks(mockTotal map[string]int) {
	mockMap := make(map[string]interface{}, len(mockTotal))
	for k, v := range mockTotal {
		mockMap[k] = v
	}
	dataMap := map[string]interface{}{
		"mocks": mockMap,
	}
	tel.SendTelemetry("RecordedMocks", dataMap)
}

func (tel *Telemetry) RecordedTestCaseMock(mockType string) {
	dataMap := map[string]interface{}{
		"mock": mockType,
	}
	tel.SendTelemetry("RecordedTestCaseMock", dataMap)
}

func (tel *Telemetry) SendTelemetry(eventType string, output ...map[string]interface{}) {
	if !tel.Enabled || tel.closed.Load() {
		return
	}

	event := models.TeleEvent{
		EventType: eventType,
		CreatedAt: time.Now().Unix(),
	}
	if len(output) > 0 {
		event.Meta = output[0]
	} else {
		event.Meta = map[string]interface{}{}
	}

	if tel.GlobalMap != nil {
		tel.GlobalMap.Range(func(key, value interface{}) bool {
			if k, ok := key.(string); ok {
				event.Meta[k] = value
			}
			return true
		})
	}

	event.InstallationID = tel.InstallationID
	event.OS = runtime.GOOS
	event.KeployVersion = tel.KeployVersion
	event.Arch = runtime.GOARCH

	func() {
		defer func() { recover() }() //nolint:errcheck
		event.IsCI, event.CIProvider = detectCI()
		event.GitRepo = detectGitRepo()
	}()

	bin, err := marshalEvent(event)
	if err != nil {
		return
	}

	tel.inflight.Add(1)
	go func() {
		defer tel.inflight.Done()

		req, err := http.NewRequest(http.MethodPost, teleURL, bytes.NewBuffer(bin))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		resp, err := tel.client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		_, _ = unmarshalResp(resp)
	}()
}

func (tel *Telemetry) Shutdown() {
	tel.closed.Store(true)
	if tel.logger != nil {
		tel.logger.Info("Cleaning up running operations...")
	}
	done := make(chan struct{})
	go func() {
		tel.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}
