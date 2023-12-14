package platform

import (
	"context"
)

type TestCaseDB interface {
	WriteTestcase(tc MockDescriptor, ctx context.Context, filters MockDescriptor) error
	WriteMock(tc MockDescriptor, ctx context.Context) error

	ReadTestcase(path string, lastSeenId MockDescriptor, options MockDescriptor) ([]MockDescriptor, error)
	ReadTcsMocks(tc MockDescriptor, path string) ([]MockDescriptor, error)
	ReadConfigMocks(path string) ([]MockDescriptor, error)
}

type TestReportDB interface {
	Lock()
	Unlock()
	SetResult(runId string, test MockDescriptor)
	GetResults(runId string) ([]MockDescriptor, error)
	Read(ctx context.Context, path, name string) (MockDescriptor, error)
	Write(ctx context.Context, path string, doc MockDescriptor) error
}

type MockDescriptor interface {
	GetKind() string
}
