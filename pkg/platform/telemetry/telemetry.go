package telemetry

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"go.keploy.io/server/pkg/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
)

type TelemetryConfig struct {
	AnalyticDB      DB
	EnableTelemetry bool
	logger          *zap.Logger
}

func NewTelemetryConfig(db *mongo.Database, isTestMode bool, enableTelemetry bool, logger *zap.Logger) TelemetryConfig {
	adb := TelemetryConfig{
		EnableTelemetry: enableTelemetry,
		logger:          logger,
	}
	if !isTestMode && enableTelemetry {
		adb.AnalyticDB = NewTelemetryDB(db, isTestMode, enableTelemetry, logger)
	}
	return adb
}

func (ac *TelemetryConfig) PingTelemetry() {
	if ac.AnalyticDB == nil || !ac.EnableTelemetry {
		return
	}
	go func() {
		for {
			count, err := ac.AnalyticDB.Count()
			if err != nil {
				ac.logger.Fatal("failed to countDocuments in analytics collection", zap.Error(err))
			}
			event := models.Event{
				EventType: "Ping",
				CreatedAt: time.Now().Unix(),
			}
			if count == 0 {
				bin, err := marshalEvent(event, ac.logger)
				if err != nil {
					break
				}
				resp, err := http.Post("http://localhost:3030/analytics", "application/json", bytes.NewBuffer(bin))
				if err != nil {
					ac.logger.Fatal("failed to send request for analytics", zap.Error(err))
					break
				}
				id, err := unmarshalResp(resp, ac.logger)
				if err != nil {
					break
				}
				fmt.Printf("create installation id: %v\n", id)
				ac.AnalyticDB.Insert(id)
			} else {
				ac.SendTelemetry("Ping")
			}
			time.Sleep(5 * time.Minute)
		}
	}()

}

func (ac *TelemetryConfig) NormalizeTelemetry() {
	if ac.AnalyticDB == nil || !ac.EnableTelemetry {
		return
	}
	go func() {
		ac.SendTelemetry("NormaliseTC")
	}()
}

func (ac *TelemetryConfig) DeleteTcTelemetry() {
	if ac.AnalyticDB == nil || !ac.EnableTelemetry {
		return
	}
	go func() {
		ac.SendTelemetry("DeleteTC")
	}()
}

func (ac *TelemetryConfig) EditTcTelemetry() {
	if ac.AnalyticDB == nil || !ac.EnableTelemetry {
		return
	}
	go func() {
		ac.SendTelemetry("EditTC")
	}()
}

func (ac *TelemetryConfig) TestrunTelemetry(success int, failure int) {
	if ac.AnalyticDB == nil || !ac.EnableTelemetry {
		return
	}
	go func() {
		ac.SendTelemetry("TestRun", map[string]interface{}{"Passed-Tests": success, "Failed-Tests": failure})
	}()
}

func (ac *TelemetryConfig) GetAppsTelemetry(apps int) {
	if ac.AnalyticDB == nil || !ac.EnableTelemetry {
		return
	}
	go func() {
		ac.SendTelemetry("GetApps", map[string]interface{}{"Apps": apps})
	}()
}

func (ac *TelemetryConfig) SendTelemetry(eventType string, output ...map[string]interface{}) {
	if ac.AnalyticDB != nil && ac.EnableTelemetry {
		event := models.Event{
			EventType: eventType,
			CreatedAt: time.Now().Unix(),
		}
		// only 1 or no meta is passed in output array parameter
		if len(output) == 1 {
			event.Meta = output[0]
		}
		sr := ac.AnalyticDB.Find()
		if sr == nil {
			ac.logger.Error("")
			return
		}
		doc := bson.D{}
		err := sr.Decode(&doc)
		if err != nil {
			ac.logger.Error("failed to decode transactionID", zap.Error(err))
			return
		}
		m := doc.Map()
		tid, ok := m["InstallationID"].(string)
		if !ok {
			ac.logger.Error("InstallationID not present")
			return
		}
		event.InstallationID = tid
		bin, err := marshalEvent(event, ac.logger)
		if err != nil {
			return
		}
		resp, err := http.Post("http://localhost:3030/analytics", "application/json", bytes.NewBuffer(bin))
		if err != nil {
			ac.logger.Fatal("failed to send request for analytics", zap.Error(err))
			return
		}
		id, err := unmarshalResp(resp, ac.logger)
		if err != nil {
			return
		}
		fmt.Printf("stored installation id: %v and eventType: %v\n", id, eventType)
	}
}
