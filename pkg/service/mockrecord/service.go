package mockrecord

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/record"
)

// Service defines the interface for recording outgoing mocks.
type Service interface {
	Record(ctx context.Context, opts models.RecordOptions) (*models.RecordResult, error)
}

// RecordRunner defines the record flow entrypoint for mocks-only capture.
type RecordRunner interface {
	StartWithOptions(ctx context.Context, reRecordCfg models.ReRecordCfg, opts record.StartOptions) (*record.StartResult, error)
}
