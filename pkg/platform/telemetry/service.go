package telemetry

import (
	// "context"

	// "go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	// "go.uber.org/zap"
)

type DB interface {
	Count() (int64, error)
	Insert(id string) (*mongo.InsertOneResult, error)
	Find() string
}

type Service interface {
	Ping()
	Normalize()
	EditTc()
	Testrun(int, int)
	DeleteTc()
	GetApps(int)
}
