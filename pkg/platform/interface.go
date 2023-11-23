package platform

import (
	"context"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type TestCaseDB interface {
	WriteTestcase(tc interface{}, ctx context.Context, filters interface{}) error
	WriteMock(tc interface{}, ctx context.Context) error

	ReadTestcase(path string, lastSeenId *primitive.ObjectID, options interface{}) ([]interface{}, error)
	ReadTcsMocks(tc interface{}, path string) ([]interface{}, error)
	ReadConfigMocks(path string) ([]interface{}, error)
}

type TestReportDB interface {
	Lock()
	Unlock()
	SetResult(runId string, test interface{})
	GetResults(runId string) ([]interface{}, error)
	Read(ctx context.Context, path, name string) (interface{}, error)
	Write(ctx context.Context, path string, doc interface{}) error
}
