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
	"sort"
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

	if len(chunks) == 0 {
		return nil
	}

	// To keep order, since maps are unordered.
	sortedChunkIDs := make([]int, 0, len(chunks))
	for chunkID := range chunks {
		sortedChunkIDs = append(sortedChunkIDs, chunkID)
	}
	sort.Ints(sortedChunkIDs)

	const batchSize = 32 // Process chunks in batches to manage memory

	for i := 0; i < len(sortedChunkIDs); i += batchSize {
		end := i + batchSize
		if end > len(sortedChunkIDs) {
			end = len(sortedChunkIDs)
		}
		batchIDs := sortedChunkIDs[i:end]

		batchContents := make([]string, 0, len(batchIDs))
		validChunkIDs := make([]int, 0, len(batchIDs))

		for _, chunkID := range batchIDs {
			chunkContent := chunks[chunkID]
			if strings.TrimSpace(chunkContent) == "" {
				e.logger.Warn("Skipping empty chunk", zap.Int("chunkID", chunkID), zap.String("filePath", filePath))
				continue
			}
			batchContents = append(batchContents, chunkContent)
			validChunkIDs = append(validChunkIDs, chunkID)
		}

		if len(batchContents) == 0 {
			continue
		}

		embeddings, err := e.callAIService(batchContents)
		if err != nil {
			e.logger.Error("Failed to generate embeddings for batch",
				zap.String("filePath", filePath),
				zap.Error(err))
			continue // Continue with the next batch
		}

		if len(embeddings) != len(batchContents) {
			e.logger.Error("Mismatch in number of embeddings returned for batch",
				zap.Int("expected", len(batchContents)),
				zap.Int("received", len(embeddings)),
				zap.String("filePath", filePath))
			continue // Continue with the next batch
		}

		for j, embedding := range embeddings {
			chunkID := validChunkIDs[j]
			chunkContent := batchContents[j]
			err = e.storeEmbedding(ctx, filePath, chunkID, chunkContent, embedding)
			if err != nil {
				e.logger.Error("Failed to store embedding",
					zap.Int("chunkID", chunkID),
					zap.String("filePath", filePath),
					zap.Error(err))
			} else {
				e.logger.Debug("Successfully stored embedding",
					zap.Int("chunkID", chunkID),
					zap.String("filePath", filePath))
			}
		}
	}

	e.logger.Info("Embedding generation and storage completed successfully for file", zap.String("filePath", filePath))
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
	// Using a token limit appropriate for the sentence-transformers model
	return 256
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
	batchSize := 32 // A reasonable batch size
	var chunkBatch []ChunkJob
	chunkCount := 0

	for chunk := range chunkChan {
		chunkBatch = append(chunkBatch, chunk)
		if len(chunkBatch) >= batchSize {
			e.processChunkBatch(ctx, chunkBatch)
			chunkCount += len(chunkBatch)
			chunkBatch = nil // Reset batch
		}
	}

	// Process any remaining chunks in the last batch
	if len(chunkBatch) > 0 {
		e.processChunkBatch(ctx, chunkBatch)
		chunkCount += len(chunkBatch)
	}

	e.logger.Info("Embedding generation completed (streaming)",
		zap.Int("totalChunks", chunkCount))

	return <-errChan
}

func (e *EmbedService) processChunkBatch(ctx context.Context, batch []ChunkJob) {
	e.logger.Debug("Processing chunk batch for embedding (streaming)",
		zap.Int("batchSize", len(batch)))

	contents := make([]string, 0, len(batch))
	validJobs := make([]ChunkJob, 0, len(batch))

	for _, job := range batch {
		if strings.TrimSpace(job.Content) == "" {
			e.logger.Warn("Skipping empty chunk in batch", zap.Int("chunkID", job.ChunkID), zap.String("filePath", job.FilePath))
			continue
		}
		contents = append(contents, job.Content)
		validJobs = append(validJobs, job)
	}

	if len(contents) == 0 {
		return
	}

	embeddings, err := e.callAIService(contents)
	if err != nil {
		e.logger.Error("Failed to generate embeddings for batch", zap.Error(err))
		for _, job := range validJobs {
			e.logger.Error("Failed to generate embedding for chunk in batch",
				zap.String("file", job.FilePath),
				zap.Int("chunkID", job.ChunkID),
				zap.Error(err))
		}
		return
	}

	if len(embeddings) != len(validJobs) {
		e.logger.Error("Mismatch in number of embeddings returned for batch",
			zap.Int("expected", len(validJobs)),
			zap.Int("received", len(embeddings)))
		return
	}

	for i, embedding := range embeddings {
		job := validJobs[i]
		err = e.storeEmbedding(ctx, job.FilePath, job.ChunkID, job.Content, embedding)
		if err != nil {
			e.logger.Error("Failed to store embedding",
				zap.String("file", job.FilePath),
				zap.Int("chunkID", job.ChunkID),
				zap.Error(err))
		} else {
			e.logger.Debug("Successfully stored embedding (streaming)",
				zap.Int("chunkID", job.ChunkID),
				zap.String("filePath", job.FilePath))
		}
	}
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
	dropTableQuery := `DROP TABLE IF EXISTS code_embeddings;`
	_, err := e.pgConn.Exec(ctx, dropTableQuery)
	if err != nil {
		return fmt.Errorf("failed to drop existing embeddings table: %w", err)
	}

	createTableQuery := `
        CREATE TABLE IF NOT EXISTS code_embeddings (
            id BIGSERIAL PRIMARY KEY,
            file_path TEXT NOT NULL,
            chunk_id INTEGER NOT NULL,
            content TEXT NOT NULL,
            embedding VECTOR(384),
            created_at TIMESTAMP DEFAULT NOW(),
            UNIQUE(file_path, chunk_id)
        )
    `

	_, err = e.pgConn.Exec(ctx, createTableQuery)
	if err != nil {
		return fmt.Errorf("failed to create embeddings table: %w", err)
	}

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

func (e *EmbedService) callAIService(contents []string) ([][]float32, error) {
	modelID := "sentence-transformers/all-MiniLM-L6-v2"
	if e.cfg.Embed.ModelName != "" {
		modelID = e.cfg.Embed.ModelName
	}

	url := "https://4be83bff41e1.ngrok-free.app/generate_embeddings/"

	type requestBody struct {
		Sentences []string `json:"sentences"`
	}

	if len(contents) == 0 {
		return [][]float32{}, nil
	}

	// The chunker should now provide appropriately sized chunks, so we don't need to split them here.
	reqBody := requestBody{
		Sentences: contents,
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

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 60 * time.Second,
	}

	e.logger.Debug("Calling local embedding service",
		zap.String("model", modelID),
		zap.String("url", url),
		zap.Int("contentCount", len(contents)))

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

	e.logger.Debug("Local embedding service response",
		zap.Int("statusCode", resp.StatusCode))

	if resp.StatusCode != http.StatusOK {
		e.logger.Error("Local embedding service error",
			zap.Int("statusCode", resp.StatusCode),
			zap.String("response", string(body)))
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var response struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		e.logger.Error("Failed to decode JSON response",
			zap.Error(err),
			zap.String("rawResponse", string(body)))
		return nil, fmt.Errorf("failed to decode embeddings: %w", err)
	}

	if len(response.Embeddings) != len(contents) {
		return nil, fmt.Errorf("mismatch between number of sentences sent (%d) and embeddings received (%d)", len(contents), len(response.Embeddings))
	}

	finalEmbeddings := make([][]float32, len(contents))
	for i := 0; i < len(contents); i++ {
		finalEmbeddings[i] = convertToFloat32(response.Embeddings[i], e.logger, i)
	}

	e.logger.Debug("Successfully generated embeddings",
		zap.Int("count", len(finalEmbeddings)))

	return finalEmbeddings, nil
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

func averageEmbeddings(embeddings [][]float64) ([]float64, error) {
	if len(embeddings) == 0 {
		return nil, fmt.Errorf("cannot average empty list of embeddings")
	}
	if len(embeddings[0]) == 0 {
		return nil, fmt.Errorf("cannot average zero-dimension embeddings")
	}

	dim := len(embeddings[0])
	avgEmbedding := make([]float64, dim)

	for _, emb := range embeddings {
		if len(emb) != dim {
			return nil, fmt.Errorf("cannot average embeddings of different dimensions: %d vs %d", len(emb), dim)
		}
		for i := 0; i < dim; i++ {
			avgEmbedding[i] += emb[i]
		}
	}

	for i := 0; i < dim; i++ {
		avgEmbedding[i] /= float64(len(embeddings))
	}

	return avgEmbedding, nil
}

func convertToFloat32(embedding []float64, logger *zap.Logger, indexInBatch int) []float32 {
	result := make([]float32, len(embedding))
	for i, val := range embedding {
		if math.IsNaN(val) || math.IsInf(val, 0) {
			if logger != nil {
				logger.Warn("Invalid float value in embedding, replacing with 0.0",
					zap.Int("originalIndexInBatch", indexInBatch),
					zap.Int("valueIndex", i),
					zap.Float64("value", val))
			}
			val = 0.0
		}
		result[i] = float32(val)
	}
	return result
}
