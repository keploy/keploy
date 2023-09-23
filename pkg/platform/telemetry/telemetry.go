package telemetry

import (
	"bytes"
	"net/http"
	"runtime"
	"time"

	"go.keploy.io/server/pkg/models"
	sentry "github.com/getsentry/sentry-go"
	"go.uber.org/zap"
)

type Telemetry struct {
	Enabled        bool
	OffMode        bool
	logger         *zap.Logger
	InstallationID string
	store          FS
	KeployVersion  string
	GlobalMap      map[string]interface{}
	client         *http.Client
}

func NewTelemetry(enabled, offMode bool, store FS, logger *zap.Logger, KeployVersion string, GlobalMap map[string]interface{}) *Telemetry {

	tele := Telemetry{
		Enabled:       enabled,
		OffMode:       offMode,
		logger:        logger,
		store:         store,
		KeployVersion: KeployVersion,
		GlobalMap:     GlobalMap,
		client:        &http.Client{Timeout: 10 * time.Second},
	}
	return &tele
}

func (ac *Telemetry) Ping(isTestMode bool, ipAddress string) {
	check := false
	if !ac.Enabled {
		return
	}
	if isTestMode {
		check = true
	}

	go func() {
		defer sentry.Recover()
		for {
			var count int64
			var err error
			var id string

			if ac.Enabled && !isTestMode {
				id, _ = ac.store.Get(true)
				count = int64(len(id))
			}

			if err != nil {
				ac.logger.Debug("failed to countDocuments in analytics collection", zap.Error(err))
			}
			event := models.TeleEvent{
				EventType: "Ping",
				CreatedAt: time.Now().Unix(),
				TeleCheck: check,
			}

			if count == 0 {
				if count == 0 {
					bin, err := marshalEvent(event, ac.logger)
					if err != nil {
						break
					}
					resp, err := http.Post("https://telemetry.keploy.io/analytics", "application/json", bytes.NewBuffer(bin))
					if err != nil {
						ac.logger.Debug("failed to send request for analytics", zap.Error(err))
						break
					}
					installation_id, err := unmarshalResp(resp, ac.logger)
					if err != nil {
						break
					}
					id = installation_id
				}
				ac.InstallationID = id
				ac.store.Set(id, ipAddress)
			}
			time.Sleep(5 * time.Minute)
		}
	}()
}

func (ac *Telemetry) Testrun(success int, failure int) {
	ac.SendTelemetry("TestRun", map[string]interface{}{"Passed-Tests": success, "Failed-Tests": failure})
}

func (ac *Telemetry) UnitTestRun(success int, failure int) {
	ac.SendTelemetry("UnitTestRun", map[string]interface{}{"Passed-UnitTests": success, "Failed-UnitTests": failure})
}

// Telemetry event for the Mocking feature test run
func (ac *Telemetry) MockTestRun(success int, failure int) {
	ac.SendTelemetry("MockTestRun", map[string]interface{}{"Passed-Mocks": success, "Failed-Mocks": failure})
}

// Telemetry event for the tests and mocks that are recorded
func (ac *Telemetry) RecordedTest(testSet string, testsTotal int) {
	ac.SendTelemetry("RecordedTest", map[string]interface{}{"test-set": testSet, "tests": testsTotal})
}

// Telemetry event for the mocks that are recorded in the mocking feature
func (ac *Telemetry) RecordedMock(mockTotal map[string]int) {
	ac.SendTelemetry("RecordedMock", map[string]interface{}{"mockTotal": mockTotal})
}

func (ac *Telemetry) SendTelemetry(eventType string, output ...map[string]interface{}) {

	if ac.Enabled {
		event := models.TeleEvent{
			EventType: eventType,
			CreatedAt: time.Now().Unix(),
		}
		event.Meta = make(map[string]interface{})
		if len(output) != 0 {
			event.Meta = output[0]
		}

		if ac.GlobalMap != nil {
			event.Meta["global-map"] = ac.GlobalMap
		}

		if ac.InstallationID == "" {
			id := ""
			id, _ = ac.store.Get(true)
			ac.InstallationID = id
		}
		event.InstallationID = ac.InstallationID
		event.OS = runtime.GOOS
		event.KeployVersion = ac.KeployVersion
		bin, err := marshalEvent(event, ac.logger)
		if err != nil {
			ac.logger.Debug("failed to marshal event", zap.Error(err))
			return
		}

		req, err := http.NewRequest(http.MethodPost, "https://telemetry.keploy.io/analytics", bytes.NewBuffer(bin))
		if err != nil {
			ac.logger.Debug("failed to create request for analytics", zap.Error(err))
			return
		}

		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		if !ac.OffMode {
			resp, err := ac.client.Do(req)
			if err != nil {
				ac.logger.Debug("failed to send request for analytics", zap.Error(err))
				return
			}

			unmarshalResp(resp, ac.logger)
			return
		}
		go func() {
			defer sentry.Recover()
			resp, err := ac.client.Do(req)
			if err != nil {
				ac.logger.Debug("failed to send request for analytics", zap.Error(err))
				return
			}
			unmarshalResp(resp, ac.logger)
		}()
	}
}
