package telemetry

import (
	"bytes"
	"context"
	"net/http"
	"runtime"
	"time"

	// "go.mongodb.org/mongo-driver/bson"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

type Telemetry struct {
	db             DB
	Enabled        bool
	OffMode        bool
	logger         *zap.Logger
	InstallationID string
	store          FS
	testExport     bool
	KeployVersion  string
}

func NewTelemetry(col DB, enabled, offMode, testExport bool, store FS, logger *zap.Logger, KeployVersion string) *Telemetry {

	tele := Telemetry{
		Enabled:       enabled,
		OffMode:       offMode,
		logger:        logger,
		db:            col,
		store:         store,
		testExport:    testExport,
		KeployVersion: KeployVersion,
	}
	return &tele

}

func (ac *Telemetry) Ping(isTestMode bool) {
	check := false
	if !ac.Enabled {
		return
	}
	if isTestMode {
		check = true
	}

	go func() {
		for {
			var count int64
			var err error
			var id string

			if ac.Enabled && !isTestMode {
				if ac.testExport {
					// Checking if id is present in hidden keploy-config folder
					id, _ = ac.store.Get(true)
					count = int64(len(id))
				} else {
					count, err = ac.db.Count()
				}
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
				if ac.testExport {
					// Checking if id is present in old keploy-config folder
					id, _ = ac.store.Get(false)
					count = int64(len(id))
				}
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
				if ac.testExport {
					ac.store.Set(id)
				} else {
					ac.db.Insert(id)
				}
			} else {
				ac.SendTelemetry("Ping", http.Client{}, context.TODO())
			}
			time.Sleep(5 * time.Minute)
		}
	}()

}

func (ac *Telemetry) Normalize(client http.Client, ctx context.Context) {
	ac.SendTelemetry("NormaliseTC", client, ctx)
}

func (ac *Telemetry) DeleteTc(client http.Client, ctx context.Context) {
	ac.SendTelemetry("DeleteTC", client, ctx)
}

func (ac *Telemetry) EditTc(client http.Client, ctx context.Context) {
	ac.SendTelemetry("EditTC", client, ctx)
}

func (ac *Telemetry) Testrun(success int, failure int, client http.Client, ctx context.Context) {
	ac.SendTelemetry("TestRun", client, ctx, map[string]interface{}{"Passed-Tests": success, "Failed-Tests": failure})
}

// Telemetry event for the Mocking feature test run
func (ac *Telemetry) MockTestRun(success int, failure int, client http.Client, ctx context.Context) {
	ac.SendTelemetry("MockTestRun", client, ctx, map[string]interface{}{"Passed-Mocks": success, "Failed-Mocks": failure})
}

// Telemetry event for the tests and mocks that are recorded
func (ac *Telemetry) RecordedTest(client http.Client, ctx context.Context, mockCount int, mockType []string) {
	ac.SendTelemetry("RecordedTestAndMocks", client, ctx, map[string]interface{}{"mockCount": mockCount, "mockType": mockType})
}

// Telemetry event for the mocks that are recorded in the mocking feature
func (ac *Telemetry) RecordedMock(client http.Client, ctx context.Context, mockType string) {
	ac.SendTelemetry("RecordedMock", client, ctx, map[string]interface{}{"mockType": mockType})
}

func (ac *Telemetry) GetApps(apps int, client http.Client, ctx context.Context) {
	ac.SendTelemetry("GetApps", client, ctx, map[string]interface{}{"Apps": apps})
}

func (ac *Telemetry) SendTelemetry(eventType string, client http.Client, ctx context.Context, output ...map[string]interface{}) {

	if ac.Enabled {
		event := models.TeleEvent{
			EventType: eventType,
			CreatedAt: time.Now().Unix(),
		}
		if len(output) != 0 {
			event.Meta = output[0]
		}
		if ac.InstallationID == "" {
			id := ""
			if ac.testExport {
				id, _ = ac.store.Get(true)
			} else {
				id = ac.db.Find()
			}
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
			req = req.WithContext(ctx)
			resp, err := client.Do(req)
			if err != nil {
				ac.logger.Debug("failed to send request for analytics", zap.Error(err))
				return
			}

			unmarshalResp(resp, ac.logger)
			return
		}
		go func() {
			resp, err := client.Do(req)
			if err != nil {
				ac.logger.Debug("failed to send request for analytics", zap.Error(err))
				return
			}
			unmarshalResp(resp, ac.logger)
		}()
	}
}
