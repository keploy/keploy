package mockrecord

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
)

// Service defines the interface for recording outgoing mocks.
type Service interface {
	Record(ctx context.Context, opts models.RecordOptions) (*models.RecordResult, error)
}

// RecordService defines the record service interactions required for recording mocks.
type RecordService interface {
	RecordMocks(ctx context.Context, opts models.RecordOptions) (*models.RecordResult, error)
}
