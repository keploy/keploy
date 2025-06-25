package embed

import (
	"context"
	"fmt"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service"
	"go.uber.org/zap"
)

type EmbedService struct {
	cfg    *config.Config
	logger *zap.Logger
	auth   service.Auth
	tel    Telemetry
}

type Telemetry interface {
	GenerateEmbedding()
}

func NewEmbedService(
	cfg *config.Config,
	tel Telemetry,
	auth service.Auth,
	logger *zap.Logger,
) (*EmbedService, error) {
	return &EmbedService{
		cfg:    cfg,
		logger: logger,
		auth:   auth,
		tel:    tel,
	}, nil
}

func (e *EmbedService) Start(ctx context.Context) error {
	e.tel.GenerateEmbedding()

	// Check for context cancellation before proceeding
	select {
	case <-ctx.Done():
		return fmt.Errorf("process cancelled by user")
	default:
		// Continue if no cancellation
	}

	e.logger.Info("Starting embedding generation",
		zap.String("sourcePath", e.cfg.Embed.SourcePath),
		zap.String("outputPath", e.cfg.Embed.OutputPath))

	// TODO: Implement embedding generation logic here
	// This would include:
	// 1. Reading the source file
	// 2. Processing the code
	// 3. Calling AI service to generate embeddings
	// 4. Saving embeddings to output file

	e.logger.Info("Embedding generation completed successfully")
	return nil
}
