package platform

import (
	"context"
)

type TestCaseDB interface {
	WriteTestcase(tc KindSpecifier, ctx context.Context, filters KindSpecifier) error
	WriteMock(tc KindSpecifier, ctx context.Context) error

	ReadTestcase(path string, lastSeenId KindSpecifier, options KindSpecifier) ([]KindSpecifier, error)
	ReadTcsMocks(tc KindSpecifier, path string) ([]KindSpecifier, error)
	ReadConfigMocks(path string) ([]KindSpecifier, error)
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
