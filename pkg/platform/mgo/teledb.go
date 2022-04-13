package mgo

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
)

type telemetryDB struct {
	c   *mongo.Collection
	log *zap.Logger
}

func (adb *telemetryDB) Count() (int64, error) {
	if adb.c == nil {
		return 0, nil
	}
	return adb.c.CountDocuments(context.TODO(), bson.M{})
}

func (adb *telemetryDB) Insert(id string) (*mongo.InsertOneResult, error) {
	if adb.c == nil {
		return nil, nil
	}
	return adb.c.InsertOne(context.TODO(), bson.D{{"InstallationID", id}})
}

func (adb *telemetryDB) Find() *mongo.SingleResult {
	if adb.c == nil {
		return nil
	}
	return adb.c.FindOne(context.TODO(), bson.M{})
}

func NewTelemetryDB(db *mongo.Database, telemetryTable string, enabled bool, logger *zap.Logger) *telemetryDB {
	adb := telemetryDB{
		log: logger,
	}
	if enabled {
		adb.c = db.Collection(telemetryTable)
	}
	return &adb
}
