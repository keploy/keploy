package utgen

import (
	"context"
)

type Service interface {
	Start(ctx context.Context) error
}

type Telemetry interface {
	GenerateUT()
}
