package telemetry

import (
	// "context"

	// "go.mongodb.org/mongo-driver/bson"
	"context"
	"net/http"

	"go.mongodb.org/mongo-driver/mongo"
	// "go.uber.org/zap"
)

type DB interface {
	Count() (int64, error)
	Insert(id string) (*mongo.InsertOneResult, error)
	Find() string
}

type FS interface {
	Get() (string, error)
	Set(string) error
}

type Service interface {
	Ping(bool)
	Normalize(http.Client, context.Context)
	EditTc(http.Client, context.Context)
	Testrun(int, int, http.Client, context.Context)
	RecordedTests(int, int, http.Client, context.Context)
	RecordedMocks(int, http.Client, context.Context)
	DeleteTc(http.Client, context.Context)
	GetApps(int, http.Client, context.Context)
}
