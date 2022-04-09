package telemetry

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.uber.org/zap"
)

type DB interface {
	Count() (int64, error)
	Insert(id string) (*mongo.InsertOneResult, error)
	Find() *mongo.SingleResult
}

type analyticsDB struct {
	db  *mongo.Collection
	log *zap.Logger
}

func (adb analyticsDB) Count() (int64, error) {
	if adb.db == nil {
		return 0, nil
	}
	return adb.db.CountDocuments(context.TODO(), bson.M{})
}

func (adb analyticsDB) Insert(id string) (*mongo.InsertOneResult, error) {
	if adb.db == nil {
		return nil, nil
	}
	return adb.db.InsertOne(context.TODO(), bson.D{{"InstallationID", id}})
}

func (adb analyticsDB) Find() *mongo.SingleResult {
	if adb.db == nil {
		return nil
	}
	return adb.db.FindOne(context.TODO(), bson.M{})
}

func NewAnalyticsDB(db *mongo.Database, isTestMode bool, enableTelemetry bool, logger *zap.Logger) analyticsDB {
	adb := analyticsDB{
		log: logger,
	}
	if !isTestMode && enableTelemetry {
		adb.db = db.Collection("analytics")
	}
	return adb
}
