package utgen

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

type Service interface {
	Start(ctx context.Context) error
	runCoverage() error
	GenerateTests(ctx context.Context) (*models.UnitTestsDetails, error)
	InitialUnitTestAnalysis(ctx context.Context) error
	analyzeTestHeadersIndentation(ctx context.Context) (int, error)
	analyzeRelevantLineNumberToInsertAfter(ctx context.Context) (int, error)
	ValidateTest(generatedTest models.UnitTest) error
}
