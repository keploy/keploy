package main

import (
	"context"
	"log"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/mcp"
	"go.keploy.io/server/v2/pkg/service/embed"
	"go.uber.org/zap"
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatal("Failed to initialize logger:", err)
	}
	defer logger.Sync()

	cfg := &config.Config{
		Embed: config.Embed{
			SourcePath:    "./",
			Model:         "",
			LLMBaseURL:    "",
			LLMApiVersion: "",
			DatabaseURL:   "postgresql://postgres:postgres@localhost:5432/keploy_embeddings",
			APIKey:        "", // enter keploy-openai api key here although not needed now.
			ModelName:     "sentence-transformers/all-MiniLM-L6-v2",
		},
	}

	mockTelemetry := &mockTelemetry{}

	mockAuth := &mockAuth{}

	embedService, err := embed.NewEmbedService(cfg, mockTelemetry, mockAuth, logger)
	if err != nil {
		logger.Fatal("Failed to create embed service", zap.Error(err))
	}

	server := mcp.NewServer(embedService, cfg, logger)

	ctx := context.Background()
	if err := server.Start(ctx); err != nil {
		logger.Fatal("Failed to start MCP server", zap.Error(err))
	}
}

type mockTelemetry struct{}

func (m *mockTelemetry) GenerateEmbedding() {
}

type mockAuth struct{}

func (m *mockAuth) GetToken(ctx context.Context) (string, error) {
	return "", nil
}

func (m *mockAuth) Login(ctx context.Context) bool {
	return true
}
