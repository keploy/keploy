package telemetry

import (
	// "context"

	// "go.mongodb.org/mongo-driver/bson"
	"context"


	"go.mongodb.org/mongo-driver/mongo"
	// "go.uber.org/zap"
)

type DB interface {
	Count() (int64, error)
	Insert(id string) (*mongo.InsertOneResult, error)
	Find() string
}

type FS interface {
	Get(bool) (string, error)
	Set(string) error
}

type Service interface {
	Ping(bool)
	Normalize(context.Context)
	EditTc(context.Context)
	Testrun(int, int,context.Context)
	MockTestRun(int, int, context.Context)
	RecordedTest(context.Context, int, []string)
	RecordedMock(context.Context, string)
	DeleteTc(context.Context)
	GetApps(int,context.Context)
}
