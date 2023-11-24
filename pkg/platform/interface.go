package platform

import (
	"context"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type TestCaseDB interface {
	WriteTestcase(tc Mock, ctx context.Context, filters Mock) error
	WriteMock(tc Mock, ctx context.Context) error

	ReadTestcase(path string, lastSeenId *primitive.ObjectID, options Mock) ([]Mock, error)
	ReadTcsMocks(tc Mock, path string) ([]Mock, error)
	ReadConfigMocks(path string) ([]Mock, error)
}

type TestReportDB interface {
	Lock()
	Unlock()
	SetResult(runId string, test Mock)
	GetResults(runId string) ([]Mock, error)
	Read(ctx context.Context, path, name string) (Mock, error)
	Write(ctx context.Context, path string, doc Mock) error
}

type Mock interface {
	GetKind() string
}
