package orchestrator

import "context"

type Service interface {
	ReRecord(ctx context.Context) error
	Normalize(ctx context.Context) error
}
