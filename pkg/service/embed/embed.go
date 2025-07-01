package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service"
	"go.uber.org/zap"
)

type EmbedService struct {
	cfg    *config.Config
	logger *zap.Logger
	auth   service.Auth
	tel    Telemetry
	pgConn *pgx.Conn
}

type ChunkJob struct {
	FilePath string
	ChunkID  int
	Content  string
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
	// Initialize PostgreSQL connection
	ctx := context.Background()
	databaseURL := cfg.Embed.DatabaseURL
	if databaseURL == "" {
		databaseURL = os.Getenv("KEPLOY_EMBED_DATABASE_URL")
	}
	if databaseURL == "" {
		databaseURL = os.Getenv("DATABASE_URL")
	}
	if databaseURL == "" {
		return nil, fmt.Errorf("database URL not configured. Set KEPLOY_EMBED_DATABASE_URL environment variable or configure in config file")
	}

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// Enable vector extension
	_, err = conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
	if err != nil {
		logger.Warn("Failed to create vector extension", zap.Error(err))
	}

	// Register pgvector types
	err = pgxvector.RegisterTypes(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("failed to register pgvector types: %w", err)
	}

	return &EmbedService{
		cfg:    cfg,
		logger: logger,
		auth:   auth,
		tel:    tel,
		pgConn: conn,
	}, nil
}

func (e *EmbedService) Start(ctx context.Context) error {
	e.tel.GenerateEmbedding()

	select {
	case <-ctx.Done():
		return fmt.Errorf("process cancelled by user")
	default:
	}

	e.logger.Info("Starting embedding generation", zap.String("sourcePath", e.cfg.Embed.SourcePath))

	// Check if source path is a file or directory
	fileInfo, err := os.Stat(e.cfg.Embed.SourcePath)
	if err != nil {
		return fmt.Errorf("failed to stat source path: %w", err)
	}

	if fileInfo.IsDir() {
		// Process directory using streaming approach
		return e.processDirectoryStreaming(ctx, e.cfg.Embed.SourcePath)
	} else {
		// Process single file
		return e.processSingleFile(ctx, e.cfg.Embed.SourcePath)
	}
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

	// Clean chunks by removing \n and \t characters
	cleanedChunks := e.cleanChunks(chunks)

	return cleanedChunks, nil
}

func (e *EmbedService) cleanChunks(chunks map[int]string) map[int]string {
	cleanedChunks := make(map[int]string)

	for chunkID, content := range chunks {
		// Remove \n and \t characters
		cleanedContent := strings.ReplaceAll(content, "\n", " ")
		cleanedContent = strings.ReplaceAll(cleanedContent, "\t", " ")

		// Replace multiple spaces with single space
		cleanedContent = strings.Join(strings.Fields(cleanedContent), " ")

		cleanedChunks[chunkID] = cleanedContent

		e.logger.Debug("Cleaned chunk",
			zap.Int("chunkID", chunkID),
			zap.Int("originalLength", len(content)),
			zap.Int("cleanedLength", len(cleanedContent)))
	}

	return cleanedChunks
}

func (e *EmbedService) GenerateEmbeddings(chunks map[int]string, filePath string) error {
	e.logger.Info("Generating and storing embeddings for chunks",
		zap.Int("numChunks", len(chunks)),
		zap.String("filePath", filePath))

	ctx := context.Background()

	if err := e.initializeDatabase(ctx); err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}

	for chunkID, chunkContent := range chunks {
		e.logger.Debug("Processing chunk for embedding",
			zap.Int("chunkID", chunkID),
			zap.String("filePath", filePath),
			zap.Int("contentLength", len(chunkContent)))

		// Call AI service to generate embeddings
		embedding, err := e.callAIService(chunkContent)
		if err != nil {
			e.logger.Error("Failed to generate embedding",
				zap.Int("chunkID", chunkID),
				zap.String("filePath", filePath),
				zap.Error(err))
			continue
		}

		// Store embedding in PostgreSQL with pgvector
		err = e.storeEmbedding(ctx, filePath, chunkID, chunkContent, embedding)
		if err != nil {
			e.logger.Error("Failed to store embedding",
				zap.Int("chunkID", chunkID),
				zap.String("filePath", filePath),
				zap.Error(err))
			continue
		}

		e.logger.Debug("Successfully stored embedding",
			zap.Int("chunkID", chunkID),
			zap.String("filePath", filePath))
	}

	e.logger.Info("Embedding generation and storage completed successfully")
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

func (e *EmbedService) shouldSkipDirectory(dirPath string) bool {
	dirName := filepath.Base(dirPath)

	// List of directories to skip
	skipDirs := map[string]bool{
		"node_modules":  true,
		".git":          true,
		".svn":          true,
		".hg":           true,
		"vendor":        true,
		"target":        true,
		"build":         true,
		"dist":          true,
		"out":           true,
		".vscode":       true,
		".idea":         true,
		".gradle":       true,
		".mvn":          true,
		"__pycache__":   true,
		".pytest_cache": true,
		".tox":          true,
		"coverage":      true,
		"test-results":  true,
		"bin":           true,
		"obj":           true,
		".next":         true,
		".nuxt":         true,
		".output":       true,
		"tmp":           true,
		"temp":          true,
	}

	return skipDirs[dirName]
}

func (e *EmbedService) processDirectory(ctx context.Context, dirPath string) error {
	e.logger.Info("Processing directory for embeddings", zap.String("directory", dirPath))

	var allChunks = make(map[string]map[int]string) // filePath -> chunks

	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			e.logger.Warn("Error accessing path", zap.String("path", path), zap.Error(err))
			return nil
		}

		// Check for context cancellation
		select {
		case <-ctx.Done():
			return fmt.Errorf("process cancelled by user")
		default:
		}

		// Skip directories that should be ignored
		if info.IsDir() {
			if e.shouldSkipDirectory(path) {
				e.logger.Debug("Skipping directory", zap.String("dir", path))
				return filepath.SkipDir // This skips the entire directory
			}
			return nil
		}

		// Skip non-code files
		if !e.isCodeFile(path) {
			return nil
		}

		e.logger.Debug("Processing file", zap.String("file", path))

		// Process each file
		chunks, err := e.processSingleFileForChunks(path)
		if err != nil {
			e.logger.Warn("Failed to process file", zap.String("file", path), zap.Error(err))
			return nil
		}

		if len(chunks) > 0 {
			allChunks[path] = chunks
		}

		return nil
	})

	if err != nil {
		return fmt.Errorf("failed to walk directory: %w", err)
	}

	// Generate embeddings for all chunks
	return e.generateEmbeddingsForAllFiles(allChunks)
}

func (e *EmbedService) processDirectoryStreaming(ctx context.Context, dirPath string) error {
	e.logger.Info("Processing directory for embeddings (streaming)", zap.String("directory", dirPath))

	chunkChan := make(chan ChunkJob, 100)
	errChan := make(chan error, 1)

	if err := e.initializeDatabase(ctx); err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}

	go func() {
		defer close(chunkChan)
		err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				e.logger.Warn("Error accessing path", zap.String("path", path), zap.Error(err))
				return nil
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			if info.IsDir() {
				if e.shouldSkipDirectory(path) {
					e.logger.Debug("Skipping directory", zap.String("dir", path))
					return filepath.SkipDir
				}
				return nil
			}

			// Skip non-code files
			if !e.isCodeFile(path) {
				return nil
			}

			e.logger.Debug("Processing file", zap.String("file", path))

			chunks, err := e.processSingleFileForChunks(path)
			if err != nil {
				e.logger.Warn("Failed to process file", zap.String("file", path), zap.Error(err))
				return nil
			}

			for chunkID, content := range chunks {
				select {
				case chunkChan <- ChunkJob{FilePath: path, ChunkID: chunkID, Content: content}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
		errChan <- err
	}()

	// Consumer goroutine to process chunks as they arrive
	chunkCount := 0
	for chunk := range chunkChan {
		chunkCount++
		e.logger.Debug("Processing chunk for embedding (streaming)",
			zap.Int("chunkID", chunk.ChunkID),
			zap.String("filePath", chunk.FilePath),
			zap.Int("totalProcessed", chunkCount))

		embedding, err := e.callAIService(chunk.Content)
		if err != nil {
			e.logger.Error("Failed to generate embedding",
				zap.String("file", chunk.FilePath),
				zap.Int("chunkID", chunk.ChunkID),
				zap.Error(err))
			continue
		}

		err = e.storeEmbedding(ctx, chunk.FilePath, chunk.ChunkID, chunk.Content, embedding)
		if err != nil {
			e.logger.Error("Failed to store embedding",
				zap.String("file", chunk.FilePath),
				zap.Int("chunkID", chunk.ChunkID),
				zap.Error(err))
		} else {
			e.logger.Debug("Successfully stored embedding (streaming)",
				zap.Int("chunkID", chunk.ChunkID),
				zap.String("filePath", chunk.FilePath))
		}
	}

	e.logger.Info("Embedding generation completed (streaming)",
		zap.Int("totalChunks", chunkCount))

	return <-errChan
}

func (e *EmbedService) processSingleFile(_ context.Context, filePath string) error {
	chunks, err := e.processSingleFileForChunks(filePath)
	if err != nil {
		return err
	}

	return e.GenerateEmbeddings(chunks, filePath)
}

func (e *EmbedService) processSingleFileForChunks(filePath string) (map[int]string, error) {
	// Read the file
	code, err := e.readSourceFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read source file: %w", err)
	}

	// Determine file extension
	fileExt := e.getFileExtension(filePath)

	// Process the code using chunker
	chunks, err := e.ProcessCode(code, fileExt, e.getTokenLimit())
	if err != nil {
		return nil, fmt.Errorf("failed to process code: %w", err)
	}

	return chunks, nil
}

// Also extend the supported file types
func (e *EmbedService) isCodeFile(filePath string) bool {
	ext := strings.ToLower(filepath.Ext(filePath))

	codeExtensions := map[string]bool{
		".go": true,
		".py": true,
		".js": true,
	}

	return codeExtensions[ext]
}

func (e *EmbedService) generateEmbeddingsForAllFiles(allChunks map[string]map[int]string) error {
	totalChunks := 0
	for _, chunks := range allChunks {
		totalChunks += len(chunks)
	}

	e.logger.Info("Generating embeddings for all files",
		zap.Int("totalFiles", len(allChunks)),
		zap.Int("totalChunks", totalChunks))

	for filePath, chunks := range allChunks {
		e.logger.Debug("Processing file chunks",
			zap.String("file", filePath),
			zap.Int("numChunks", len(chunks)))

		err := e.GenerateEmbeddings(chunks, filePath)
		if err != nil {
			e.logger.Error("Failed to generate embeddings for file",
				zap.String("file", filePath),
				zap.Error(err))
			continue
		}
	}

	e.logger.Info("Embedding generation completed for all files")
	return nil
}

func (e *EmbedService) initializeDatabase(ctx context.Context) error {
	// Create embeddings table
	createTableQuery := `
        CREATE TABLE IF NOT EXISTS code_embeddings (
            id BIGSERIAL PRIMARY KEY,
            file_path TEXT NOT NULL,
            chunk_id INTEGER NOT NULL,
            content TEXT NOT NULL,
            embedding VECTOR(1536),
            created_at TIMESTAMP DEFAULT NOW(),
            UNIQUE(file_path, chunk_id)
        )
    `

	_, err := e.pgConn.Exec(ctx, createTableQuery)
	if err != nil {
		return fmt.Errorf("failed to create embeddings table: %w", err)
	}

	// Create index for vector similarity search
	indexQuery := `
        CREATE INDEX IF NOT EXISTS code_embeddings_embedding_idx 
        ON code_embeddings USING hnsw (embedding vector_cosine_ops)
    `

	_, err = e.pgConn.Exec(ctx, indexQuery)
	if err != nil {
		e.logger.Warn("Failed to create vector index", zap.Error(err))
	}

	return nil
}

func (e *EmbedService) storeEmbedding(ctx context.Context, filePath string, chunkID int, content string, embedding []float32) error {
	query := `
        INSERT INTO code_embeddings (file_path, chunk_id, content, embedding)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (file_path, chunk_id)
        DO UPDATE SET 
            content = EXCLUDED.content,
            embedding = EXCLUDED.embedding,
            created_at = NOW()
    `

	vector := pgvector.NewVector(embedding)
	_, err := e.pgConn.Exec(ctx, query, filePath, chunkID, content, vector)
	if err != nil {
		return fmt.Errorf("failed to insert embedding: %w", err)
	}

	return nil
}

func (e *EmbedService) callAIService(content string) ([]float32, error) {
	token := os.Getenv("HF_TOKEN")
	if token == "" {
		if e.cfg.Embed.APIKey != "" {
			token = e.cfg.Embed.APIKey
		} else {
			return nil, fmt.Errorf("HF_TOKEN environment variable or config APIKey not set")
		}
	}

	modelID := "sentence-transformers/all-MiniLM-L6-v2"
	if e.cfg.Embed.ModelName != "" {
		modelID = e.cfg.Embed.ModelName
	}

	url := fmt.Sprintf("https://api-inference.huggingface.co/models/%s", modelID)

	type requestBody struct {
		Inputs  []string               `json:"inputs"`
		Options map[string]interface{} `json:"options,omitempty"`
	}

	if strings.TrimSpace(content) == "" {
		e.logger.Warn("Empty content provided for embedding generation")
		return nil, fmt.Errorf("content is empty")
	}

	maxContentLength := 512
	if len(content) > maxContentLength {
		e.logger.Warn("Content too long, truncating",
			zap.Int("originalLength", len(content)),
			zap.Int("maxLength", maxContentLength))
		content = content[:maxContentLength]
	}

	reqBody := requestBody{
		Inputs: []string{content},
		Options: map[string]interface{}{
			"wait_for_model": true,
			"use_cache":      false,
		},
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		e.logger.Error("Failed to marshal request body", zap.Error(err))
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payload))
	if err != nil {
		e.logger.Error("Failed to create HTTP request", zap.Error(err))
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	e.logger.Debug("Calling HuggingFace API for embedding generation",
		zap.String("model", modelID),
		zap.String("url", url),
		zap.Int("contentLength", len(content)))

	resp, err := client.Do(req)
	if err != nil {
		e.logger.Error("HTTP request failed", zap.Error(err))
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		e.logger.Error("Failed to read response body", zap.Error(err))
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	e.logger.Debug("HuggingFace API response",
		zap.Int("statusCode", resp.StatusCode),
		zap.String("response", string(body)))

	if resp.StatusCode != http.StatusOK {
		e.logger.Error("HuggingFace API error",
			zap.Int("statusCode", resp.StatusCode),
			zap.String("response", string(body)))

		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return nil, fmt.Errorf("unauthorized: check HF_TOKEN")
		case http.StatusTooManyRequests:
			return nil, fmt.Errorf("rate limited: too many requests")
		case http.StatusServiceUnavailable:
			return nil, fmt.Errorf("model loading: try again in a few moments")
		case http.StatusNotFound:
			return nil, fmt.Errorf("model not found: %s. Check if the model exists", modelID)
		default:
			return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
		}
	}

	var embeddings []float64
	if err := json.Unmarshal(body, &embeddings); err != nil {
		var nestedEmbeddings [][]float64
		if err2 := json.Unmarshal(body, &nestedEmbeddings); err2 != nil {
			e.logger.Error("Failed to decode JSON response",
				zap.Error(err),
				zap.Error(err2),
				zap.String("rawResponse", string(body)))
			return nil, fmt.Errorf("failed to decode embeddings: %w", err)
		}
		if len(nestedEmbeddings) == 0 || len(nestedEmbeddings[0]) == 0 {
			return nil, fmt.Errorf("received empty embedding vector")
		}
		embeddings = nestedEmbeddings[0]
	}

	if len(embeddings) == 0 {
		e.logger.Error("Empty embedding vector received")
		return nil, fmt.Errorf("received empty embedding vector")
	}

	// Convert float64 to float32 for pgvector compatibility
	result := make([]float32, len(embeddings))
	for i, val := range embeddings {
		if math.IsNaN(val) || math.IsInf(val, 0) {
			e.logger.Warn("Invalid float value in embedding",
				zap.Int("index", i),
				zap.Float64("value", val))
			val = 0.0
		}
		result[i] = float32(val)
	}

	e.logger.Debug("Successfully generated embedding",
		zap.Int("dimensions", len(result)),
		zap.String("model", modelID))

	return result, nil
}

func (e *EmbedService) SearchSimilarCode(ctx context.Context, queryEmbedding []float32, limit int) ([]SearchResult, error) {
	query := `
        SELECT file_path, chunk_id, content, embedding <-> $1 as distance
        FROM code_embeddings
        ORDER BY embedding <-> $1
        LIMIT $2
    `

	vector := pgvector.NewVector(queryEmbedding)
	rows, err := e.pgConn.Query(ctx, query, vector, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to search similar code: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var result SearchResult
		err := rows.Scan(&result.FilePath, &result.ChunkID, &result.Content, &result.Distance)
		if err != nil {
			return nil, fmt.Errorf("failed to scan result: %w", err)
		}
		results = append(results, result)
	}

	return results, nil
}

type SearchResult struct {
	FilePath string  `json:"file_path"`
	ChunkID  int     `json:"chunk_id"`
	Content  string  `json:"content"`
	Distance float64 `json:"distance"`
}

func (e *EmbedService) Close() error {
	if e.pgConn != nil {
		return e.pgConn.Close(context.Background())
	}
	return nil
}
