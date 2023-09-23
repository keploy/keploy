package telemetry

import (
	"context"
)

type FS interface {
	Get(bool) (string, error)
	Set(string, string) error
}

type Service interface {
	Ping(bool)
	Testrun(int, int, context.Context)
	MockTestRun(int, int, context.Context)
	RecordedTest(context.Context, int, []string)
	RecordedMock(context.Context, string)
}
