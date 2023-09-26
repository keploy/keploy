package telemetry

import (
	"context"
)

type FS interface {
	Get(bool) (string, error)
	Set(string) error
}

type Service interface {
	Ping(bool)
	Testrun(int, int, context.Context)
	MockTestRun(int, int, context.Context)
	RecordedTest(string, int)
	RecordedMock(map[string]int)
}
