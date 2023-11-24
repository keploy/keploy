package platform

import (
	"context"
)

type TestCaseDB interface {
	WriteTestcase(tc Interface, ctx context.Context, filters Interface) error
	WriteMock(tc Interface, ctx context.Context) error

	ReadTestcase(path string, lastSeenId string, options Interface) ([]Interface, error)
	ReadTcsMocks(tc Interface, path string) ([]Interface, error)
	ReadConfigMocks(path string) ([]Interface, error)
}

type TestReportDB interface {
	Lock()
	Unlock()
	SetResult(runId string, test Interface)
	GetResults(runId string) ([]Interface, error)
	Read(ctx context.Context, path, name string) (Interface, error)
	Write(ctx context.Context, path string, doc Interface) error
}

type Interface interface {
	GetKind() string
}
