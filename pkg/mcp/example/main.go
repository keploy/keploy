package main

import (
	"context"
	"log"
	"os"

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
			Model:         "gpt-3.5-turbo",
			LLMBaseURL:    "https://api.openai.com/v1",
			LLMApiVersion: "2023-05-15",
			DatabaseURL:   "postgres://localhost:5432/keploy",
			APIKey:        os.Getenv("OPENAI_API_KEY"),
			ModelName:     "sentence-transformers/all-MiniLM-L6-v2",
		},
	}

	var embedService embed.Service

	server := mcp.NewServer(embedService, cfg, logger)

	ctx := context.Background()
	if err := server.Start(ctx); err != nil {
		logger.Fatal("Failed to start MCP server", zap.Error(err))
	}
}
