package testsuite

import (
	"context"

	"golang.org/x/time/rate"
)

type Service interface {
	Execute(ctx context.Context, limiter *rate.Limiter) (*ExecutionReport, error)
}
