package telemetry

import (
	"bytes"
	"context"
	"net/http"
	"runtime"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

var teleUrl = "https://telemetry.keploy.io/analytics"

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
		for {
			tel.SendTelemetry(ctx, "Ping")
			time.Sleep(5 * time.Minute)
		}
	}()
}

func (tel *Telemetry) Testrun(ctx context.Context, success int, failure int) {
	tel.SendTelemetry(ctx, "TestRun", map[string]interface{}{"Passed-Tests": success, "Failed-Tests": failure})
}

// Telemetry event for the Mocking feature test run
func (tel *Telemetry) MockTestRun(ctx context.Context, utilizedMocks int) {
	tel.SendTelemetry(ctx, "MockTestRun", map[string]interface{}{"Utilized-Mocks": utilizedMocks})
}

// Telemetry event for the tests and mocks that are recorded
func (tel *Telemetry) RecordedTestSuite(ctx context.Context, testSet string, testsTotal int, mockTotal map[string]int) {
	tel.SendTelemetry(ctx, "RecordedTestSuite", map[string]interface{}{"test-set": testSet, "tests": testsTotal, "mocks": mockTotal})
}

func (tel *Telemetry) RecordedTestAndMocks(ctx context.Context) {
	tel.SendTelemetry(ctx, "RecordedTestAndMocks", map[string]interface{}{"mocks": make(map[string]int)})
}

// Telemetry event for the mocks that are recorded in the mocking feature
func (tel *Telemetry) RecordedMocks(ctx context.Context, mockTotal map[string]int) {
	go tel.SendTelemetry(ctx, "RecordedMocks", map[string]interface{}{"mocks": mockTotal})
}

func (tel *Telemetry) RecordedTestCaseMock(ctx context.Context, mockType string) {
	go tel.SendTelemetry(ctx, "RecordedTestCaseMock", map[string]interface{}{"mock": mockType})
}

func (tel *Telemetry) SendTelemetry(ctx context.Context, eventType string, output ...map[string]interface{}) {
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

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, teleUrl, bytes.NewBuffer(bin))
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
		unmarshalResp(resp, tel.logger)
	}
}
