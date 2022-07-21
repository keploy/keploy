package telemetry

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"go.keploy.io/server/pkg/models"
	// "go.mongodb.org/mongo-driver/bson"
	"go.uber.org/zap"
)

type Telemetry struct {
	db             DB
	Enabled        bool
	OffMode        bool
	logger         *zap.Logger
	InstallationID string
}

func NewTelemetry(col DB, enabled, offMode bool, logger *zap.Logger) *Telemetry {
	tele := Telemetry{
		Enabled: enabled,
		OffMode: offMode,
		logger:  logger,
		db:      col,
	}
	return &tele
}

func (ac *Telemetry) Ping(isTestMode bool) {
	check := false
	if !ac.Enabled || isTestMode {
		check = true
	}
	go func() {
		for {
			var count int64
			var err error
			if ac.Enabled && !isTestMode {
				count, err = ac.db.Count()
			}

			if err != nil {
				ac.logger.Fatal("failed to countDocuments in analytics collection", zap.Error(err))
			}
			event := models.TeleEvent{
				EventType: "Ping",
				CreatedAt: time.Now().Unix(),
				TeleCheck: check,
			}
			if count == 0 {
				bin, err := marshalEvent(event, ac.logger)
				if err != nil {
					break
				}
				resp, err := http.Post("https://telemetry.keploy.io/analytics", "application/json", bytes.NewBuffer(bin))
				if err != nil {
					ac.logger.Fatal("failed to send request for analytics", zap.Error(err))
					break
				}
				id, err := unmarshalResp(resp, ac.logger)
				if err != nil {
					break
				}
				ac.InstallationID = id
				ac.db.Insert(id)
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
			sr := ac.db.Find()
			ac.InstallationID = sr
		}
		event.InstallationID = ac.InstallationID

		bin, err := marshalEvent(event, ac.logger)
		if err != nil {
			ac.logger.Error("failed to marshal event", zap.Error(err))
			return
		}
		req, err := http.NewRequest(http.MethodPost, "https://telemetry.keploy.io/analytics", bytes.NewBuffer(bin))
		if err != nil {
			ac.logger.Fatal("failed to create request for analytics", zap.Error(err))
			return
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		if !ac.OffMode {
			req = req.WithContext(ctx)
			resp, err := client.Do(req)
			if err != nil {
				ac.logger.Fatal("failed to send request for analytics", zap.Error(err))
				return
			}
			unmarshalResp(resp, ac.logger)
			return
		}
		go func() {
			resp, err := client.Do(req)
			if err != nil {
				ac.logger.Fatal("failed to send request for analytics", zap.Error(err))
				return
			}
			unmarshalResp(resp, ac.logger)
		}()
	}
}
