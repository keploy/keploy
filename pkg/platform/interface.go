package platform

import (
	"context"

	"go.keploy.io/server/pkg/models"
)

type TestCaseDB interface {
	WriteTestcase(tc KindSpecifier, ctx context.Context, filters KindSpecifier) error
	WriteMock(tc KindSpecifier, ctx context.Context) error

	RewriteMocks(mocks []*models.Mock, testSet string) error

	ReadTestcases(testSet string, lastSeenId KindSpecifier, options KindSpecifier) ([]KindSpecifier, error)
	ReadTcsMocks(tc KindSpecifier, testSet string) ([]KindSpecifier, error)
	ReadConfigMocks(testSet string) ([]KindSpecifier, error)
	ReadTestSessionIndices() ([]string, error)
}

type TestReportDB interface {
	Lock()
	Unlock()
	SetResult(runId string, test KindSpecifier)
	GetResults(runId string) ([]KindSpecifier, error)
	Read(ctx context.Context, path, name string) (KindSpecifier, error)
	Write(ctx context.Context, path string, doc KindSpecifier) error
}

type KindSpecifier interface {
	GetKind() string
}
