package telemetry

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
)

type telemetryDB struct {
	db  *mongo.Collection
	log *zap.Logger
}

func (adb telemetryDB) Count() (int64, error) {
	if adb.db == nil {
		return 0, nil
	}
	return adb.db.CountDocuments(context.TODO(), bson.M{})
}

func (adb telemetryDB) Insert(id string) (*mongo.InsertOneResult, error) {
	if adb.db == nil {
		return nil, nil
	}
	return adb.db.InsertOne(context.TODO(), bson.D{{"InstallationID", id}})
}

func (adb telemetryDB) Find() *mongo.SingleResult {
	if adb.db == nil {
		return nil
	}
	return adb.db.FindOne(context.TODO(), bson.M{})
}

func NewTelemetryDB(db *mongo.Database, isTestMode bool, enableTelemetry bool, logger *zap.Logger) telemetryDB {
	adb := telemetryDB{
		log: logger,
	}
	if !isTestMode && enableTelemetry {
		adb.db = db.Collection("analytics")
	}
	return adb
}
