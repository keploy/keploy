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
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
	sitter "github.com/smacker/go-tree-sitter"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service"
	"go.uber.org/zap"
)

type EmbedService struct {
	cfg            *config.Config
	logger         *zap.Logger
	auth           service.Auth
	tel            Telemetry
	pgConn         *pgx.Conn
	parser         *CodeParser
	allChunks      map[string]map[int]string
	mu             sync.Mutex
	previousHashes map[string]string
	currentHashes  map[string]string
	hashesMutex    sync.Mutex
}

type ChunkJob struct {
	FilePath string
	ChunkID  int
	Content  string
}

// EmbeddingResult holds the data passed from embedding workers to the DB writer.
type EmbeddingResult struct {
	FilePath  string
	ChunkID   int
	Content   string
	Embedding []float32
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

	// Initialize the shared code parser once.
	parser, err := NewCodeParser()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize shared code parser: %w", err)
	}

	return &EmbedService{
		cfg:            cfg,
		logger:         logger,
		auth:           auth,
		tel:            tel,
		pgConn:         conn,
		parser:         parser,
		allChunks:      make(map[string]map[int]string),
		previousHashes: make(map[string]string),
		currentHashes:  make(map[string]string),
	}, nil
}

func (e *EmbedService) Start(ctx context.Context) error {
	e.tel.GenerateEmbedding()

	select {
	case <-ctx.Done():
		return fmt.Errorf("process cancelled by user")
	default:
	}

	sourcePath := e.cfg.Embed.SourcePath
	if sourcePath == "" {
		sourcePath = "."
	}

	absSourcePath, err := filepath.Abs(sourcePath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path for source: %w", err)
	}
	sourcePath = absSourcePath

	// Verify the source path exists
	fileInfo, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("failed to stat source path: %w", err)
	}

	e.logger.Info("Starting embedding generation", zap.String("sourcePath", sourcePath), zap.Bool("isDirectory", fileInfo.IsDir()))

	if fileInfo.IsDir() {
		// Process directory using streaming approach
		err = e.processDirectoryStreaming(ctx, sourcePath)
	} else {
		// Process single file
		err = e.processSingleFile(ctx, sourcePath)
	}

	if err != nil {
		return err
	}

	return nil
}

func (e *EmbedService) ProcessCodeWithAST(rootNode *sitter.Node, code string, fileExtension string, tokenLimit int, filePath string) (map[int]string, error) {
	// Create a code chunker for the specific file extension
	chunker := NewCodeChunker(fileExtension, "cl100k_base")

	// Chunk the code
	chunks, err := chunker.Chunk(e.parser, rootNode, code, tokenLimit)
	if err != nil {
		e.logger.Error("Failed to chunk code", zap.Error(err))
		return nil, fmt.Errorf("failed to chunk code: %w", err)
	}

	logFields := []zap.Field{zap.Int("numChunks", len(chunks))}
	if filePath != "" {
		logFields = append(logFields, zap.String("filePath", filePath))
	}
	e.logger.Info("Code chunking completed", logFields...)

	// Clean chunks by removing \n and \t characters
	cleanedChunks := e.cleanChunks(chunks)

	return cleanedChunks, nil
}

func (e *EmbedService) ProcessCode(code string, fileExtension string, tokenLimit int) (map[int]string, error) {
	rootNode, err := e.parser.ParseCode(code, fileExtension)
	if err != nil {
		e.logger.Warn("Failed to parse code in ProcessCode", zap.Error(err))
		return nil, fmt.Errorf("failed to parse code: %w", err)
	}

	return e.ProcessCodeWithAST(rootNode, code, fileExtension, tokenLimit, "")
}

func (e *EmbedService) cleanChunks(chunks map[int]string) map[int]string {
	cleanedChunks := make(map[int]string)

	for chunkID, content := range chunks {
		// Normalize all Unicode whitespace (including newlines and tabs) into single spaces.
		// This is a more robust and efficient approach than multiple replacements.
		cleanedContent := strings.Join(strings.Fields(content), " ")

		cleanedChunks[chunkID] = cleanedContent

		e.logger.Debug("Cleaned chunk",
			zap.Int("chunkID", chunkID),
			zap.Int("originalLength", len(content)),
			zap.Int("cleanedLength", len(cleanedContent)))
	}

	return cleanedChunks
}

func (e *EmbedService) GenerateEmbeddings(ctx context.Context, chunks map[int]string, filePath string) error {
	// Check for context cancellation before starting
	select {
	case <-ctx.Done():
		return fmt.Errorf("process cancelled by user")
	default:
	}

	e.logger.Info("Generating and storing embeddings for chunks",
		zap.Int("numChunks", len(chunks)),
		zap.String("filePath", filePath))

	if err := e.initializeDatabase(ctx); err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}

	if len(chunks) == 0 {
		return nil
	}

	var results []EmbeddingResult
	// To keep order, since maps are unordered.
	sortedChunkIDs := make([]int, 0, len(chunks))
	for chunkID := range chunks {
		sortedChunkIDs = append(sortedChunkIDs, chunkID)
	}
	sort.Ints(sortedChunkIDs)

	const batchSize = 32 // Process chunks in batches to manage memory

	for i := 0; i < len(sortedChunkIDs); i += batchSize {
		// Check for context cancellation in each batch iteration
		select {
		case <-ctx.Done():
			return fmt.Errorf("process cancelled by user")
		default:
		}

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

		embeddings, err := e.callAIService(ctx, batchContents)
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
			results = append(results, EmbeddingResult{
				FilePath:  filePath,
				ChunkID:   validChunkIDs[j],
				Content:   batchContents[j],
				Embedding: embedding,
			})
		}
	}

	if err := e.storeEmbeddingsBatch(ctx, results); err != nil {
		e.logger.Error("Failed to store embeddings batch for file", zap.String("filePath", filePath), zap.Error(err))
		return err
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
	return 256
}

func (e *EmbedService) shouldSkipDirectory(dirPath string) bool {
	dirName := filepath.Base(dirPath)

	// List of directories to skip
	skipDirs := map[string]bool{
		".keploy":       true,
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

func (e *EmbedService) processDirectoryStreaming(ctx context.Context, dirPath string) error {
	if err := e.initializeDatabase(ctx); err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}

	// Resolve the absolute path to ensure consistent comparison
	absDirPath, err := filepath.Abs(dirPath)
	if err != nil {
		return fmt.Errorf("failed to resolve absolute path for %s: %w", dirPath, err)
	}

	hashesFilePath := filepath.Join(absDirPath, ".keploy", "embedding_hashes.json")
	var filesToProcess []string

	if e.cfg.Embed.Incremental {
		var err error
		e.previousHashes, err = loadHashes(hashesFilePath, e.logger)
		if err != nil {
			e.logger.Warn("failed to load previous hashes, proceeding with full re-indexing", zap.Error(err))
		}
	}

	// Keep track of visited directories to prevent infinite loops
	visitedDirs := make(map[string]bool)

	walkErr := filepath.Walk(absDirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			e.logger.Warn("Error accessing path, skipping", zap.String("path", path), zap.Error(err))
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if info.IsDir() {
			// Check for recursive linking or infinite traversal
			absPath, absErr := filepath.Abs(path)
			if absErr != nil {
				e.logger.Warn("Failed to resolve absolute path for directory, skipping", zap.String("path", path), zap.Error(absErr))
				return filepath.SkipDir
			}

			if visitedDirs[absPath] {
				e.logger.Warn("Detected potential recursive directory linking, skipping", zap.String("path", path))
				return filepath.SkipDir
			}
			visitedDirs[absPath] = true

			if e.shouldSkipDirectory(path) {
				e.logger.Debug("Skipping directory", zap.String("dir", path))
				return filepath.SkipDir
			}
			return nil
		}
		if e.isCodeFile(path) {
			newHash, err := calculateFileHash(path)
			if err != nil {
				e.logger.Warn("failed to calculate hash for file, skipping", zap.String("path", path), zap.Error(err))
				return nil
			}

			e.hashesMutex.Lock()
			e.currentHashes[path] = newHash
			e.hashesMutex.Unlock()

			// Only add files that have changed or when not in incremental mode
			prevHash := e.previousHashes[path]
			if !e.cfg.Embed.Incremental || prevHash != newHash {
				filesToProcess = append(filesToProcess, path)
				e.logger.Debug("File will be processed", zap.String("path", path), zap.Bool("changed", prevHash != newHash))
			} else {
				e.logger.Debug("Skipping unchanged file", zap.String("path", path))
			}
		}
		return nil
	})

	if walkErr != nil {
		return fmt.Errorf("error walking directory: %w", walkErr)
	}

	if e.cfg.Embed.Incremental {
		var deletedFiles []string
		for path := range e.previousHashes {
			if _, exists := e.currentHashes[path]; !exists {
				deletedFiles = append(deletedFiles, path)
			}
		}
		if len(deletedFiles) > 0 {
			if err := e.deleteEmbeddingsForFiles(ctx, deletedFiles); err != nil {
				e.logger.Error("failed to delete embeddings for removed files, state will not be updated", zap.Error(err))
				return err
			}
		}
	}

	if len(filesToProcess) == 0 {
		if e.cfg.Embed.Incremental {
			if err := saveHashes(hashesFilePath, e.currentHashes); err != nil {
				e.logger.Error("failed to save hashes", zap.Error(err))
			}
		}
		return nil
	}

	e.logger.Info("Indexing codebase started", zap.Int("filesToProcess", len(filesToProcess)))

	// Channels for parallel processing
	chunkChan := make(chan ChunkJob, 1000) // Buffer for chunks
	embeddingJobsChan := make(chan []ChunkJob, 4)
	dbWriteChan := make(chan EmbeddingResult, 100)

	var totalChunks int32
	var chunkingWg sync.WaitGroup
	var embeddingWg sync.WaitGroup
	var dbWg sync.WaitGroup

	// Start embedding workers
	numEmbeddingWorkers := 4
	for i := 0; i < numEmbeddingWorkers; i++ {
		embeddingWg.Add(1)
		go func() {
			defer embeddingWg.Done()
			for batch := range embeddingJobsChan {
				if ctx.Err() != nil {
					return
				}
				results, err := e.processChunkBatchForEmbeddings(ctx, batch)
				if err != nil {
					e.logger.Error("Failed to process chunk batch for embeddings", zap.Error(err))
					// Decrement totalChunks for failed chunks
					atomic.AddInt32(&totalChunks, -int32(len(batch)))
					continue
				}
				for _, res := range results {
					if ctx.Err() != nil {
						return
					}
					dbWriteChan <- res
				}
			}
		}()
	}

	// Start DB writer
	dbWg.Add(1)
	go func() {
		defer dbWg.Done()
		const dbBatchSize = 128
		var dbBatch []EmbeddingResult
		for res := range dbWriteChan {
			if ctx.Err() != nil {
				return
			}
			dbBatch = append(dbBatch, res)
			if len(dbBatch) >= dbBatchSize {
				if err := e.storeEmbeddingsBatch(ctx, dbBatch); err != nil {
					e.logger.Error("Failed to store embeddings batch", zap.Error(err))
				}
				dbBatch = nil
			}
		}
		if len(dbBatch) > 0 {
			if err := e.storeEmbeddingsBatch(ctx, dbBatch); err != nil {
				e.logger.Error("Failed to store remaining embeddings batch", zap.Error(err))
			}
		}
	}()

	// Start chunk processor
	chunkingWg.Add(1)
	go func() {
		defer chunkingWg.Done()
		const batchSize = 32
		var batch []ChunkJob

		for chunk := range chunkChan {
			if ctx.Err() != nil {
				return
			}

			batch = append(batch, chunk)
			if len(batch) >= batchSize {
				embeddingJobsChan <- batch
				batch = nil
			}
		}

		// Send remaining chunks
		if len(batch) > 0 {
			embeddingJobsChan <- batch
		}
		close(embeddingJobsChan)
	}()

	// Process files and generate chunks (this happens in parallel with embedding)
	for _, path := range filesToProcess {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Check if file has changed before processing
		if e.cfg.Embed.Incremental {
			newHash, err := calculateFileHash(path)
			if err != nil {
				e.logger.Warn("failed to calculate hash for file, skipping", zap.String("path", path), zap.Error(err))
				continue
			}

			prevHash := e.previousHashes[path]
			if prevHash == newHash {
				e.logger.Debug("Skipping unchanged file", zap.String("path", path))
				continue
			}

			// Update current hash
			e.hashesMutex.Lock()
			e.currentHashes[path] = newHash
			e.hashesMutex.Unlock()
		}

		chunks, err := e.processSingleFileForChunks(path)
		if err != nil {
			e.logger.Warn("failed to process and chunk file, skipping", zap.String("file", path), zap.Error(err))
			continue
		}

		// Add chunks to the collection and send to embedding pipeline
		for chunkID, content := range chunks {
			chunk := ChunkJob{FilePath: path, ChunkID: chunkID, Content: content}
			chunkChan <- chunk

			// Update total chunks count
			atomic.AddInt32(&totalChunks, 1)
		}
	}

	// Close chunk channel and wait for processing to complete
	close(chunkChan)
	chunkingWg.Wait()
	embeddingWg.Wait()
	close(dbWriteChan)
	dbWg.Wait()

	if err := saveHashes(hashesFilePath, e.currentHashes); err != nil {
		e.logger.Error("failed to save hashes after successful run", zap.Error(err))
	}

	// Log summary
	e.logger.Info("Codebase indexing completed successfully", zap.Int("processedFiles", len(filesToProcess)), zap.Int32("generatedEmbeddings", totalChunks))

	return nil
}

// processFileForChunking reads a single file, parses it, chunks it, and sends the chunks to a channel.
func (e *EmbedService) processFileForChunking(path string, chunkChan chan<- ChunkJob, ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	e.logger.Debug("Processing file for symbols and chunks", zap.String("file", path))

	code, err := e.readSourceFile(path)
	if err != nil {
		e.logger.Warn("Failed to read file, skipping", zap.String("file", path), zap.Error(err))
		return
	}

	fileExt := e.getFileExtension(path)

	rootNode, err := e.parser.ParseCode(code, fileExt)
	if err != nil {
		e.logger.Warn("Failed to parse code, skipping", zap.String("file", path), zap.Error(err))
		return
	}

	chunks, err := e.ProcessCodeWithAST(rootNode, code, fileExt, e.getTokenLimit(), path)
	if err != nil {
		e.logger.Warn("Failed to process and chunk file", zap.String("file", path), zap.Error(err))
		return
	}

	if len(chunks) > 0 {
		e.mu.Lock()
		e.allChunks[path] = chunks
		e.mu.Unlock()
	}

	for chunkID, content := range chunks {
		select {
		case chunkChan <- ChunkJob{FilePath: path, ChunkID: chunkID, Content: content}:
		case <-ctx.Done():
			return
		}
	}
}

func (e *EmbedService) processChunkBatchForEmbeddings(ctx context.Context, batch []ChunkJob) ([]EmbeddingResult, error) {
	e.logger.Debug("Processing chunk batch for embedding", zap.Int("batchSize", len(batch)))

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
		return nil, nil
	}

	embeddings, err := e.callAIService(ctx, contents)
	if err != nil {
		return nil, fmt.Errorf("failed to generate embeddings for batch: %w", err)
	}

	if len(embeddings) != len(validJobs) {
		return nil, fmt.Errorf("mismatch in number of embeddings returned for batch (expected %d, got %d)", len(validJobs), len(embeddings))
	}

	results := make([]EmbeddingResult, len(validJobs))
	for i, embedding := range embeddings {
		job := validJobs[i]
		results[i] = EmbeddingResult{
			FilePath:  job.FilePath,
			ChunkID:   job.ChunkID,
			Content:   job.Content,
			Embedding: embedding,
		}
	}
	return results, nil
}

func (e *EmbedService) processSingleFile(ctx context.Context, filePath string) error {
	// Verify that filePath is actually a file, not a directory
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("failed to stat file %s: %w", filePath, err)
	}

	if fileInfo.IsDir() {
		return fmt.Errorf("expected a file but got a directory: %s", filePath)
	}

	dir := filepath.Dir(filePath)
	hashesFilePath := filepath.Join(dir, ".keploy", "embedding_hashes.json")

	e.logger.Info("Processing single file...", zap.String("filePath", filePath))

	if e.cfg.Embed.Incremental {
		previousHashes, err := loadHashes(hashesFilePath, e.logger)
		if err != nil {
			e.logger.Warn("failed to load previous hashes, proceeding with full processing", zap.Error(err))
		}
		newHash, err := calculateFileHash(filePath)
		if err != nil {
			return fmt.Errorf("failed to calculate hash for file %s: %w", filePath, err)
		}
		if previousHashes[filePath] == newHash {
			e.logger.Info("Skipping unchanged file", zap.String("path", filePath))
			return nil
		}
		e.currentHashes[filePath] = newHash
	}

	chunks, err := e.processSingleFileForChunks(filePath)
	if err != nil {
		return err
	}

	if len(chunks) > 0 {
		e.mu.Lock()
		e.allChunks[filePath] = chunks
		e.mu.Unlock()
	}

	e.logger.Info("Generated chunks from file", zap.String("filePath", filePath), zap.Int("chunkCount", len(chunks)))

	if len(chunks) == 0 {
		e.logger.Warn("No chunks generated for file", zap.String("filePath", filePath))
		if e.cfg.Embed.Incremental {
			if err := saveHashes(hashesFilePath, e.currentHashes); err != nil {
				e.logger.Error("failed to save hashes for single file", zap.Error(err))
			}
		}
		return nil
	}

	e.logger.Info("Starting embedding generation for file...", zap.String("filePath", filePath))

	// Process chunks in batches for embeddings
	const batchSize = 32
	var allResults []EmbeddingResult

	for i := 0; i < len(chunks); i += batchSize {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		end := i + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}

		batch := make([]ChunkJob, 0, end-i)
		for j := i; j < end; j++ {
			batch = append(batch, ChunkJob{FilePath: filePath, ChunkID: j, Content: chunks[j]})
		}

		results, err := e.processChunkBatchForEmbeddings(ctx, batch)
		if err != nil {
			e.logger.Error("Failed to process chunk batch for embeddings", zap.Error(err))
			continue
		}

		allResults = append(allResults, results...)
	}

	// Store all embeddings
	if err := e.storeEmbeddingsBatch(ctx, allResults); err != nil {
		e.logger.Error("Failed to store embeddings batch", zap.Error(err))
		return err
	}

	if e.cfg.Embed.Incremental {
		if err := saveHashes(hashesFilePath, e.currentHashes); err != nil {
			e.logger.Error("failed to save hashes for single file", zap.Error(err))
		}
	}

	// Log summary
	e.logger.Info("Codebase indexing completed successfully for file", zap.String("filePath", filePath), zap.Int("processedChunks", len(chunks)))

	return nil
}

func (e *EmbedService) processSingleFileForChunks(filePath string) (map[int]string, error) {
	// Read the file
	code, err := e.readSourceFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read source file: %w", err)
	}

	// Determine file extension
	fileExt := e.getFileExtension(filePath)

	// Parse the code once to get the AST
	rootNode, err := e.parser.ParseCode(code, fileExt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse code: %w", err)
	}

	// Process the code using chunker
	chunks, err := e.ProcessCodeWithAST(rootNode, code, fileExt, e.getTokenLimit(), filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to process code: %w", err)
	}

	return chunks, nil
}

func (e *EmbedService) isCodeFile(filePath string) bool {
	ext := strings.ToLower(filepath.Ext(filePath))

	codeExtensions := map[string]bool{
		".go": true,
		".py": true,
		".js": true,
	}

	return codeExtensions[ext]
}

func (e *EmbedService) initializeDatabase(ctx context.Context) error {

	// dropCleanup := `
	// DROP INDEX IF EXISTS code_embeddings_embedding_idx;
	// DROP TABLE IF EXISTS code_embeddings;
	// `
	// if _, err := e.pgConn.Exec(ctx, dropCleanup); err != nil {
	// 	e.logger.Warn("Failed to drop existing index/table", zap.Error(err))
	// }

	// Create the vector extension if it doesn't exist.
	_, err := e.pgConn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector")
	if err != nil {
		e.logger.Warn("Failed to create vector extension, it might already exist.", zap.Error(err))
	}

	// Create the table if it doesn't exist.
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
	_, err = e.pgConn.Exec(ctx, createTableQuery)
	if err != nil {
		return fmt.Errorf("failed to create embeddings table: %w", err)
	}

	// Create the index if it doesn't exist.
	indexQuery := `
        CREATE INDEX IF NOT EXISTS code_embeddings_embedding_idx 
        ON code_embeddings USING hnsw (embedding vector_cosine_ops)
    `
	_, err = e.pgConn.Exec(ctx, indexQuery)
	if err != nil {
		e.logger.Warn("Failed to create vector index, it might already exist.", zap.Error(err))
	}

	return nil
}

func (e *EmbedService) storeEmbeddingsBatch(ctx context.Context, batch []EmbeddingResult) error {
	if len(batch) == 0 {
		return nil
	}

	e.logger.Debug("Storing embeddings batch (upsert)", zap.Int("size", len(batch)))

	for _, res := range batch {
		_, err := e.pgConn.Exec(ctx, `
			INSERT INTO code_embeddings (file_path, chunk_id, content, embedding)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (file_path, chunk_id)
			DO UPDATE SET content = EXCLUDED.content, embedding = EXCLUDED.embedding
		`, res.FilePath, res.ChunkID, res.Content, pgvector.NewVector(res.Embedding))
		if err != nil {
			return fmt.Errorf("failed to upsert embedding: %w", err)
		}
	}

	return nil
}

func (e *EmbedService) callAIService(ctx context.Context, contents []string) ([][]float32, error) {
	// Check for context cancellation before making the request
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("process cancelled by user")
	default:
	}

	e.logger.Info("Sending request to embedding service", zap.Int("chunk_count", len(contents)))
	modelID := "sentence-transformers/all-MiniLM-L6-v2"
	if e.cfg.Embed.ModelName != "" {
		modelID = e.cfg.Embed.ModelName
	}

	url := e.cfg.Embed.EmbeddingServiceURL
	if url == "" {
		url = os.Getenv("KEPLOY_EMBEDDING_SERVICE_URL")
	}
	if url == "" {
		return nil, fmt.Errorf("embedding service URL not configured. Set KEPLOY_EMBEDDING_SERVICE_URL environment variable or configure it in the config file")
	}

	type requestBody struct {
		Sentences []string `json:"sentences"`
	}

	if len(contents) == 0 {
		return [][]float32{}, nil
	}

	reqBody := requestBody{
		Sentences: contents,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		e.logger.Error("Failed to marshal request body", zap.Error(err))
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Use the passed context instead of creating a new one
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(payload))
	if err != nil {
		e.logger.Error("Failed to create HTTP request", zap.Error(err))
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 180 * time.Second,
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

func (e *EmbedService) GenerateEmbeddingsForQ(ctx context.Context, contents []string) ([][]float32, error) {
	if len(contents) == 0 {
		return [][]float32{}, nil
	}
	return e.callAIService(ctx, contents)
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

	e.logger.Info("found similar code snippets", zap.Any("results", results))
	return results, nil
}

func (e *EmbedService) Close() error {
	if e.pgConn != nil {
		return e.pgConn.Close(context.Background())
	}
	return nil
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

func (e *EmbedService) Converse(ctx context.Context, query string) error {
	// 1. Generate an embedding for the user's query
	e.logger.Info("Generating embedding for query", zap.String("query", query))
	queryEmbeddings, err := e.GenerateEmbeddingsForQ(ctx, []string{query})
	if err != nil {
		return fmt.Errorf("failed to generate embedding for query: %w", err)
	}
	if len(queryEmbeddings) == 0 {
		return fmt.Errorf("received no embedding for the query")
	}
	queryEmbedding := queryEmbeddings[0]

	// 2. Find similar code chunks from vector DB
	e.logger.Info("Searching for similar code chunks in the database")
	searchResults, err := e.SearchSimilarCode(ctx, queryEmbedding, 10)
	if err != nil {
		return fmt.Errorf("failed to search for similar code: %w", err)
	}

	// 3. Build context from vector search results
	var contextBuilder strings.Builder

	if len(searchResults) == 0 {
		e.logger.Warn("No relevant code snippets or symbols found for the query.")
		fmt.Println("I couldn't find any code snippets relevant to your question. Please try rephrasing or be more specific.")
		return nil
	}

	for _, res := range searchResults {
		contextBuilder.WriteString(fmt.Sprintf("--- Code Snippet from file: %s ---\n", res.FilePath))
		contextBuilder.WriteString(res.Content)
		contextBuilder.WriteString("\n---\n\n")
	}

	// 4. Construct the final prompt using the new prompt builder
	promptBuilder := NewPromptBuilder(query, contextBuilder.String(), e.logger)
	prompt, err := promptBuilder.BuildPrompt("ai_conversation")
	if err != nil {
		return fmt.Errorf("failed to build prompt: %w", err)
	}

	// 5. Call the LLM and stream the response
	e.logger.Info("Sending request to LLM for answer generation")

	aiClient, err := NewAIClient(e.cfg, e.logger, e.auth)
	if err != nil {
		return fmt.Errorf("failed to create AI client: %w", err)
	}

	response, err := aiClient.GenerateResponse(ctx, prompt)
	if err != nil {
		return fmt.Errorf("failed to get response from AI: %w", err)
	}

	fmt.Println("\nAI Assistant:")
	fmt.Println("----------------")
	fmt.Println(response)
	fmt.Println("----------------")

	return nil
}

func (e *EmbedService) deleteEmbeddingsForFiles(ctx context.Context, filePaths []string) error {
	if len(filePaths) == 0 {
		return nil
	}
	e.logger.Info("Deleting embeddings for removed files", zap.Strings("files", filePaths))
	_, err := e.pgConn.Exec(ctx, "DELETE FROM code_embeddings WHERE file_path = ANY($1)", filePaths)
	if err != nil {
		return fmt.Errorf("failed to delete embeddings for removed files: %w", err)
	}
	return nil
}
