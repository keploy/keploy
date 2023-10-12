package telemetry

import (
	"bytes"
	"net/http"
	"runtime"
	"time"

	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/utils"
	"go.uber.org/zap"
)

var teleUrl = "https://telemetry.keploy.io/analytics"

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

func (tel *Telemetry) Ping(isTestMode bool) {
	if !tel.Enabled {
		return
	}

	go func() {
		defer utils.HandlePanic()
		for {
			var count int64
			var id string

			id, _ = tel.store.Get(true)
			count = int64(len(id))

			event := models.TeleEvent{
				EventType: "Ping",
				CreatedAt: time.Now().Unix(),
			}

			if count == 0 {
				//Check in the old keploy folder.
				id, _ = tel.store.Get(false)
				count = int64(len(id))
				if count == 0 {
					bin, err := marshalEvent(event, tel.logger)
					if err != nil {
						break
					}
					resp, err := http.Post(teleUrl, "application/json", bytes.NewBuffer(bin))
					if err != nil {
						tel.logger.Debug("failed to send request for analytics", zap.Error(err))
						break
					}
					installation_id, err := unmarshalResp(resp, tel.logger)
					if err != nil {
						break
					}
					id = installation_id
				}
				tel.store.Set(id)
			}
			tel.InstallationID = id
			tel.SendTelemetry("Ping")
			time.Sleep(5 * time.Minute)
		}
	}()
}

func (tel *Telemetry) Testrun(success int, failure int) {
	tel.SendTelemetry("TestRun", map[string]interface{}{"Passed-Tests": success, "Failed-Tests": failure})
}

// func (tel *Telemetry) UnitTestRun(cmd string, success int, failure int) {
// 	tel.SendTelemetry("UnitTestRun", map[string]interface{}{"Cmd": cmd, "Passed-UnitTests": success, "Failed-UnitTests": failure})
// }

// Telemetry event for the Mocking feature test run
func (tel *Telemetry) MockTestRun(utilizedMocks int) {
	tel.SendTelemetry("MockTestRun", map[string]interface{}{"Utilized-Mocks": utilizedMocks})
}

// Telemetry event for the tests and mocks that are recorded
func (tel *Telemetry) RecordedTestSuite(testSet string, testsTotal int, mockTotal map[string]int) {
	tel.SendTelemetry("RecordedTestSuite", map[string]interface{}{"test-set": testSet, "tests": testsTotal, "mocks": mockTotal})
}

func (tel *Telemetry) RecordedTestAndMocks() {
	tel.SendTelemetry("RecordedTestAndMocks", map[string]interface{}{"mocks": make(map[string]int)})
}

// Telemetry event for the mocks that are recorded in the mocking feature
func (tel *Telemetry) RecordedMocks(mockTotal map[string]int) {
	tel.SendTelemetry("RecordedMocks", map[string]interface{}{"mocks": mockTotal})
}

func (tel *Telemetry) RecordedMock(mockType string) {
	tel.SendTelemetry("RecordedMock", map[string]interface{}{"mock": mockType})
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

		if tel.InstallationID == "" {
			id := ""
			id, _ = tel.store.Get(true)
			if id == "" {
				id, _ = tel.store.Get(false)
			}
			tel.InstallationID = id
		}
		event.InstallationID = tel.InstallationID
		event.OS = runtime.GOOS
		tel.KeployVersion = "v2-alpha"
		event.KeployVersion = tel.KeployVersion
		event.Arch = runtime.GOARCH
		bin, err := marshalEvent(event, tel.logger)
		if err != nil {
			tel.logger.Debug("failed to marshal event", zap.Error(err))
			return
		}

		req, err := http.NewRequest(http.MethodPost, teleUrl, bytes.NewBuffer(bin))
		if err != nil {
			tel.logger.Debug("failed to create request for analytics", zap.Error(err))
			return
		}

		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		if !tel.OffMode {
			resp, err := tel.client.Do(req)
			if err != nil {
				tel.logger.Debug("failed to send request for analytics", zap.Error(err))
				return
			}
			// tel.logger.Debug("Sent the event to the telemetry server.")
			unmarshalResp(resp, tel.logger)
			return
		}
		go func() {
			defer utils.HandlePanic()
			resp, err := tel.client.Do(req)
			if err != nil {
				tel.logger.Debug("failed to send request for analytics", zap.Error(err))
				return
			}
			unmarshalResp(resp, tel.logger)
		}()
	}
}
