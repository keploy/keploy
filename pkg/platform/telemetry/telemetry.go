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

var (
	AnalyticDB      *mongo.Collection
	EnableTelemetry bool
)

type AnalyticsConfig struct {
	AnalyticDB      DB
	EnableTelemetry bool
	logger          *zap.Logger
}

func NewAnalyticsConfig(db *mongo.Database, isTestMode bool, enableTelemetry bool, logger *zap.Logger) AnalyticsConfig {
	adb := AnalyticsConfig{
		EnableTelemetry: enableTelemetry,
		logger:          logger,
	}
	if !isTestMode && enableTelemetry {
		adb.AnalyticDB = NewAnalyticsDB(db, isTestMode, enableTelemetry, logger)
	}
	return adb
}

func (ac *AnalyticsConfig) PingTelemetry() {
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

func (ac *AnalyticsConfig) NormalizeTelemetry() {
	if ac.AnalyticDB == nil || !ac.EnableTelemetry {
		return
	}
	go func() {
		ac.SendTelemetry("NormaliseTC")
	}()
}

func (ac *AnalyticsConfig) DeleteTcTelemetry() {
	if ac.AnalyticDB == nil || !ac.EnableTelemetry {
		return
	}
	go func() {
		ac.SendTelemetry("DeleteTC")
	}()
}

func (ac *AnalyticsConfig) EditTcTelemetry() {
	if ac.AnalyticDB == nil || !ac.EnableTelemetry {
		return
	}
	go func() {
		ac.SendTelemetry("EditTC")
	}()
}

func (ac *AnalyticsConfig) TestrunTelemetry(success int, failure int) {
	if ac.AnalyticDB == nil || !ac.EnableTelemetry {
		return
	}
	go func() {
		ac.SendTelemetry("TestRun", map[string]interface{}{"Passed-Tests": success, "Failed-Tests": failure})
	}()
}

func (ac *AnalyticsConfig) GetAppsTelemetry(apps int) {
	if ac.AnalyticDB == nil || !ac.EnableTelemetry {
		return
	}
	go func() {
		ac.SendTelemetry("GetApps", map[string]interface{}{"Apps": apps})
	}()
}

func (ac *AnalyticsConfig) SendTelemetry(eventType string, output ...map[string]interface{}) {
	if ac.AnalyticDB != nil && ac.EnableTelemetry {
		event := models.Event{
			EventType: eventType,
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
