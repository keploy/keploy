package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"time"

	// "github.com/keploy/go-sdk/integrations/kmongo"
	"go.keploy.io/server/pkg/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
)

var (
	AnalyticDB      *mongo.Collection
	EnableTelemetry bool
)

func PingTelemetry(db *mongo.Database, logger *zap.Logger) {
	AnalyticDB = db.Collection("analytics")
	go func() {
		for {
			count, err := AnalyticDB.CountDocuments(context.TODO(), bson.M{})
			if err != nil {
				logger.Fatal("failed to countDocuments in analytics collection", zap.Error(err))
			}
			event := models.Event{
				EventType: "Ping",
			}
			if count == 0 {
				bin, err := marshalEvent(event, logger)
				if err != nil {
					break
				}
				resp, err := http.Post("http://localhost:3030/analytics", "application/json", bytes.NewBuffer(bin))
				if err != nil {
					logger.Fatal("failed to send request for analytics", zap.Error(err))
					break
				}
				id, err := unmarshalResp(resp, logger)
				if err != nil {
					break
				}
				fmt.Printf("create installation id: %v\n", id)
				AnalyticDB.InsertOne(context.TODO(), bson.D{{"InstallationID", id}})
			} else {
				SendTelemetry("Ping", logger)
			}
			time.Sleep(5 * time.Minute)
		}
	}()

}

func SendTelemetry(eventType string, log *zap.Logger, output ...map[string]interface{}) {
	if AnalyticDB != nil && EnableTelemetry {
		event := models.Event{
			EventType: eventType,
		}
		if len(output) == 1 {
			event.Meta = output[0]
		}
		sr := AnalyticDB.FindOne(context.TODO(), bson.M{})
		doc := bson.D{}
		err := sr.Decode(&doc)
		if err != nil {
			log.Error("failed to decode transactionID", zap.Error(err))
			return
		}
		m := doc.Map()
		tid, ok := m["InstallationID"].(string)
		if !ok {
			log.Error("InstallationID not present")
			return
		}
		event.InstallationID = tid
		bin, err := marshalEvent(event, log)
		if err != nil {
			return
		}
		resp, err := http.Post("http://localhost:3030/analytics", "application/json", bytes.NewBuffer(bin))
		if err != nil {
			log.Fatal("failed to send request for analytics", zap.Error(err))
			return
		}
		id, err := unmarshalResp(resp, log)
		if err != nil {
			return
		}
		fmt.Printf("stored installation id: %v and eventType: %v\n", id, eventType)
	}
}

func marshalEvent(event models.Event, log *zap.Logger) (bin []byte, err error) {
	bin, err = json.Marshal(event)
	if err != nil {
		log.Fatal("failed to marshal event struct into json", zap.Error(err))
	}
	return
}

func unmarshalResp(resp *http.Response, log *zap.Logger) (id string, err error) {
	defer func(Body io.ReadCloser) {
		err = Body.Close()
		if err != nil {
			log.Error("failed to close connecton reader", zap.String("url", "http://localhost:3030/analytics"), zap.Error(err))
			return
		}
	}(resp.Body)
	var res map[string]string
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Error("failed to read response from telemetry server", zap.String("url", "http://localhost:3030/analytics"), zap.Error(err))
		return
	}
	err = json.Unmarshal(body, &res)
	if err != nil {
		log.Error("failed to read testcases from telemetry server", zap.Error(err))
		return
	}
	id, ok := res["InstallationID"]
	if !ok {
		log.Error("InstallationID not present")
		err = errors.New("InstallationID not present")
		return
	}
	return
}
