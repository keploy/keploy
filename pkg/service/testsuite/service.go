package testsuite

import (
	"context"
)

type Service interface {
	Execute(ctx context.Context) (*ExecutionReport, error)
}
