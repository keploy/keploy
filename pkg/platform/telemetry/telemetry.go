// Package telemetry collects anonymous usage metrics.
package telemetry

import (
	"bytes"
	"context"
	"io"
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
	mu             sync.Mutex // guards closed + inflight.Add to prevent Add/Wait race
	inflight       sync.WaitGroup
	inflightN      atomic.Int64
	closed         atomic.Bool
}

type Options struct {
	Enabled        bool
	Version        string
	GlobalMap      *sync.Map
	InstallationID string
}

func NewTelemetry(logger *zap.Logger, opt Options) *Telemetry {
	gm := opt.GlobalMap
	if gm == nil {
		gm = &sync.Map{}
	}
	return &Telemetry{
		Enabled:        opt.Enabled,
		logger:         logger,
		KeployVersion:  opt.Version,
		GlobalMap:      gm,
		InstallationID: opt.InstallationID,
		client:         &http.Client{Timeout: 2 * time.Second}, // matches Shutdown drain timeout
	}
}

func (tel *Telemetry) Ping(ctx context.Context) {
	if !tel.Enabled {
		return
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		select {
		case <-ctx.Done():
			return
		default:
			if tel.closed.Load() {
				return
			}
			tel.SendTelemetry("Ping")
		}
		for {
			if tel.closed.Load() {
				return
			}
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
	tel.sendTracked("TestSetRun", dataMap)
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
	tel.sendTracked("TestRun", dataMap)
}

func (tel *Telemetry) MockTestRun(utilizedMocks int) {
	dataMap := map[string]interface{}{
		"Utilized-Mocks": utilizedMocks,
	}
	tel.sendTracked("MockTestRun", dataMap)
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
	tel.sendTracked("RecordedTestSuite", dataMap)
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
	tel.sendEvent(eventType, false, output...)
}

func (tel *Telemetry) sendTracked(eventType string, output ...map[string]interface{}) {
	tel.sendEvent(eventType, true, output...)
}

func (tel *Telemetry) sendEvent(eventType string, tracked bool, output ...map[string]interface{}) {
	if !tel.Enabled {
		return
	}

	if tracked {
		tel.mu.Lock()
		if tel.closed.Load() {
			tel.mu.Unlock()
			return
		}
		tel.inflight.Add(1)
		tel.inflightN.Add(1)
		tel.mu.Unlock()
	} else if tel.closed.Load() {
		return
	}

	event := models.TeleEvent{
		EventType: eventType,
		CreatedAt: time.Now().Unix(),
	}
	if len(output) > 0 && output[0] != nil {
		event.Meta = output[0]
	} else {
		event.Meta = map[string]interface{}{}
	}

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

	go func() {
		if tracked {
			defer func() {
				tel.inflightN.Add(-1)
				tel.inflight.Done()
			}()
		}

		func() {
			defer func() { _ = recover() }()
			event.IsCI, event.CIProvider = detectCI()
			event.GitRepo = detectGitRepo()
		}()

		bin, err := marshalEvent(event)
		if err != nil {
			return
		}

		req, err := http.NewRequest(http.MethodPost, teleURL, bytes.NewBuffer(bin))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		resp, err := tel.client.Do(req)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
}

func (tel *Telemetry) Shutdown() {
	if !tel.Enabled {
		return
	}
	tel.mu.Lock()
	if !tel.closed.CompareAndSwap(false, true) {
		tel.mu.Unlock()
		return
	}
	tel.mu.Unlock()
	if tel.inflightN.Load() == 0 {
		return
	}
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
