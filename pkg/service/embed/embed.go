package embed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	}

	e.logger.Info("Starting embedding generation",
		zap.String("sourcePath", e.cfg.Embed.SourcePath),
		zap.String("outputPath", e.cfg.Embed.OutputPath))

	// 1. Reading the source file
	code, err := e.readSourceFile(e.cfg.Embed.SourcePath)
	if err != nil {
		return fmt.Errorf("failed to read source file: %w", err)
	}

	// 2. Determine file extension
	fileExt := e.getFileExtension(e.cfg.Embed.SourcePath)

	// 3. Processing the code using chunker
	chunks, err := e.ProcessCode(code, fileExt, e.getTokenLimit())
	if err != nil {
		return fmt.Errorf("failed to process code: %w", err)
	}

	// 4. Generate embeddings
	err = e.GenerateEmbeddings(chunks)
	if err != nil {
		return fmt.Errorf("failed to generate embeddings: %w", err)
	}

	// 5. Save embeddings to output file -- need to be in milvus db
	err = e.saveEmbeddings(chunks, e.cfg.Embed.OutputPath)
	if err != nil {
		return fmt.Errorf("failed to save embeddings: %w", err)
	}

	e.logger.Info("Embedding generation completed successfully")
	return nil
}

func (e *EmbedService) ProcessCode(code string, fileExtension string, tokenLimit int) (map[int]string, error) {
	e.logger.Info("Processing code for chunking",
		zap.String("fileExtension", fileExtension),
		zap.Int("tokenLimit", tokenLimit))

	// Create a code chunker for the specific file extension
	chunker := NewCodeChunker(fileExtension, "cl100k_base")

	// Chunk the code
	chunks, err := chunker.Chunk(code, tokenLimit)
	if err != nil {
		e.logger.Error("Failed to chunk code", zap.Error(err))
		return nil, fmt.Errorf("failed to chunk code: %w", err)
	}

	e.logger.Info("Code chunking completed",
		zap.Int("numChunks", len(chunks)))

	return chunks, nil
}

func (e *EmbedService) GenerateEmbeddings(chunks map[int]string) error {
	e.logger.Info("Generating embeddings for chunks",
		zap.Int("numChunks", len(chunks)))

	// TODO: Implement AI service integration for embedding generation
	// This would involve:
	// 1. Iterate through chunks
	// 2. Call AI service to generate embeddings for each chunk
	// 3. Store embeddings with chunk metadata

	for chunkID, chunkContent := range chunks {
		e.logger.Debug("Processing chunk for embedding",
			zap.Int("chunkID", chunkID),
			zap.Int("contentLength", len(chunkContent)))

		// TODO: Call AI service here
		// embedding, err := e.callAIService(chunkContent)
		// if err != nil {
		//     return fmt.Errorf("failed to generate embedding for chunk %d: %w", chunkID, err)
		// }

		// TODO: Store embedding
		// err = e.storeEmbedding(chunkID, embedding, chunkContent)
		// if err != nil {
		//     return fmt.Errorf("failed to store embedding for chunk %d: %w", chunkID, err)
		// }
	}

	e.logger.Info("Embedding generation completed successfully")
	return nil
}

func (e *EmbedService) readSourceFile(sourcePath string) (string, error) {
	content, err := os.ReadFile(sourcePath)
	if err != nil {
		return "", fmt.Errorf("failed to read file %s: %w", sourcePath, err)
	}
	return string(content), nil
}

func (e *EmbedService) getFileExtension(filePath string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filePath), "."))

	switch ext {
	case "py", "python":
		return "py"
	case "js", "javascript", "ts", "typescript":
		return "js"
	case "go":
		return "go"
	default:
		e.logger.Warn("Unsupported file extension, defaulting to 'go'", zap.String("extension", ext))
		return "go"
	}
}

func (e *EmbedService) getTokenLimit() int {
	return 4000
}

func (e *EmbedService) saveEmbeddings(chunks map[int]string, outputPath string) error {
	// TODO: Implement saving embeddings to file
	// This could be JSON, CSV, or any other format depending on requirements

	type EmbeddingOutput struct {
		ChunkID   int       `json:"chunk_id"`
		Content   string    `json:"content"`
		Embedding []float64 `json:"embedding,omitempty"` // Will be populated when AI integration is done
	}

	var output []EmbeddingOutput
	for chunkID, content := range chunks {
		output = append(output, EmbeddingOutput{
			ChunkID: chunkID,
			Content: content,
		})
	}

	// For now, just save the chunks structure
	// TODO: Include actual embeddings when AI service is integrated
	data, err := json.Marshal(output)
	if err != nil {
		return fmt.Errorf("failed to marshal embeddings: %w", err)
	}

	err = os.WriteFile(outputPath, data, 0644)
	if err != nil {
		return fmt.Errorf("failed to write embeddings to file: %w", err)
	}

	e.logger.Info("Embeddings saved to file", zap.String("outputPath", outputPath))
	return nil
}
