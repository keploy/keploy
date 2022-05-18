package mgo

import (
	"context"

	"github.com/keploy/go-sdk/integrations/kmongo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
)

type telemetryDB struct {
	c   *kmongo.Collection
	log *zap.Logger
}

func (tele *telemetryDB) Count() (int64, error) {
	if tele.c == nil {
		return 0, nil
	}
	return tele.c.CountDocuments(context.TODO(), bson.M{})
}

func (tele *telemetryDB) Insert(id string) (*mongo.InsertOneResult, error) {
	if tele.c == nil {
		return nil, nil
	}
	return tele.c.InsertOne(context.TODO(), bson.D{{"InstallationID", id}})
}

func (tele *telemetryDB) Find() string {
	if tele.c == nil {
		return ""
	}

	sr := tele.c.FindOne(context.TODO(), bson.M{})
	if sr.Err() != nil {
		tele.log.Error("failed to find installationId", zap.Error(sr.Err()))
		return ""
	}
	doc := bson.D{}
	err := sr.Decode(&doc)
	if err != nil {
		tele.log.Error("failed to decode transactionID", zap.Error(err))
		return ""
	}
	m := doc.Map()
	tid, ok := m["InstallationID"].(string)
	if !ok {
		tele.log.Error("InstallationID not present")
		return ""
	}
	return tid
}

func NewTelemetryDB(db *mongo.Database, telemetryTable string, enabled bool, logger *zap.Logger) *telemetryDB {
	tele := telemetryDB{
		log: logger,
	}
	if enabled {
		tele.c = kmongo.NewCollection(db.Collection(telemetryTable))
	}
	return &tele
}
