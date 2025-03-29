package vector

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

const (
	DefaultMockCollection   = "mocks"
	DefaultChunkSize        = 1000
	DefaultChunkOverlap     = 200
	DefaultTopK             = 5
	DefaultMinScore         = 0.7
	DefaultRefreshThreshold = 24 * time.Hour
)

// RAGServiceImpl implements the RAGService interface
type RAGServiceImpl struct {
	VectorDB         VectorDBService
	EmbeddingService EmbeddingService
	Logger           *zap.Logger
	MockCollection   string
	ChunkSize        int
	ChunkOverlap     int
	TopK             int
	MinScore         float32
	Initialized      bool
	mu               sync.RWMutex
	lastRefreshTime  time.Time
}

// NewRAGService creates a new RAG service
func NewRAGService(logger *zap.Logger, vectorDB VectorDBService, embeddingService EmbeddingService) *RAGServiceImpl {
	return &RAGServiceImpl{
		VectorDB:         vectorDB,
		EmbeddingService: embeddingService,
		Logger:           logger,
		MockCollection:   DefaultMockCollection,
		ChunkSize:        DefaultChunkSize,
		ChunkOverlap:     DefaultChunkOverlap,
		TopK:             DefaultTopK,
		MinScore:         DefaultMinScore,
		Initialized:      false,
	}
}

// Initialize implements the RAGService interface
func (s *RAGServiceImpl) Initialize(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Initialize underlying services
	if err := s.VectorDB.Initialize(ctx); err != nil {
		return fmt.Errorf("failed to initialize vector database: %w", err)
	}

	if err := s.EmbeddingService.Initialize(ctx); err != nil {
		return fmt.Errorf("failed to initialize embedding service: %w", err)
	}

	// Create the mock collection if it doesn't exist
	collections, err := s.VectorDB.ListCollections(ctx)
	if err != nil {
		return fmt.Errorf("failed to list collections: %w", err)
	}

	collectionExists := false
	for _, collection := range collections {
		if collection == s.MockCollection {
			collectionExists = true
			break
		}
	}

	if !collectionExists {
		// For OpenAI text-embedding-3-small, the dimension is 1536
		err = s.VectorDB.CreateCollection(ctx, s.MockCollection, DefaultOpenAIDimension)
		if err != nil {
			return fmt.Errorf("failed to create collection: %w", err)
		}
	}

	s.Initialized = true
	s.lastRefreshTime = time.Now()
	s.Logger.Info("Initialized RAG service", zap.String("collection", s.MockCollection))
	return nil
}

// Close implements the RAGService interface
func (s *RAGServiceImpl) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.Initialized {
		return nil
	}

	if err := s.VectorDB.Close(ctx); err != nil {
		return fmt.Errorf("failed to close vector database: %w", err)
	}

	if err := s.EmbeddingService.Close(ctx); err != nil {
		return fmt.Errorf("failed to close embedding service: %w", err)
	}

	s.Initialized = false
	return nil
}

// IndexMock implements the RAGService interface
func (s *RAGServiceImpl) IndexMock(ctx context.Context, mock *models.Mock) error {
	s.mu.RLock()
	initialized := s.Initialized
	s.mu.RUnlock()

	if !initialized {
		return fmt.Errorf("RAG service not initialized")
	}

	// Extract content from the mock
	chunks, err := s.extractChunksFromMock(mock)
	if err != nil {
		return fmt.Errorf("failed to extract chunks from mock: %w", err)
	}

	if len(chunks) == 0 {
		s.Logger.Debug("No chunks extracted from mock", zap.String("mock_id", mock.Name))
		return nil
	}

	// Generate embeddings for each chunk
	texts := make([]string, len(chunks))
	for i, chunk := range chunks {
		texts[i] = chunk
	}

	vectors, err := s.EmbeddingService.GetEmbeddings(ctx, texts)
	if err != nil {
		return fmt.Errorf("failed to generate embeddings: %w", err)
	}

	// Create embeddings
	embeddings := make([]*models.Embedding, len(chunks))
	now := time.Now().Unix()

	for i, chunk := range chunks {
		id := uuid.New().String()

		metadata := models.Metadata{
			Source:    "mock",
			Kind:      mock.Kind,
			MockID:    mock.Name,
			Labels:    map[string]string{"kind": string(mock.Kind)},
			FileType:  "json",
			LineStart: 0,
			LineEnd:   0,
		}

		embeddings[i] = &models.Embedding{
			ID:        id,
			Vector:    vectors[i],
			Content:   chunk,
			Metadata:  metadata,
			CreatedAt: now,
			UpdatedAt: now,
		}
	}

	// Store embeddings in the vector database
	err = s.VectorDB.UpsertEmbeddings(ctx, s.MockCollection, embeddings)
	if err != nil {
		return fmt.Errorf("failed to store embeddings: %w", err)
	}

	s.Logger.Info("Indexed mock", 
		zap.String("mock_id", mock.Name), 
		zap.String("kind", string(mock.Kind)), 
		zap.Int("chunks", len(chunks)))
	return nil
}

// IndexMocks implements the RAGService interface
func (s *RAGServiceImpl) IndexMocks(ctx context.Context, mocks []*models.Mock) error {
	s.mu.RLock()
	initialized := s.Initialized
	s.mu.RUnlock()

	if !initialized {
		return fmt.Errorf("RAG service not initialized")
	}

	for _, mock := range mocks {
		err := s.IndexMock(ctx, mock)
		if err != nil {
			s.Logger.Warn("Failed to index mock", 
				zap.String("mock_id", mock.Name), 
				zap.Error(err))
		}
	}

	return nil
}

// RetrieveContext implements the RAGService interface
func (s *RAGServiceImpl) RetrieveContext(ctx context.Context, query string, options map[string]interface{}) (*models.VectorQueryResult, error) {
	s.mu.RLock()
	initialized := s.Initialized
	s.mu.RUnlock()

	if !initialized {
		return nil, fmt.Errorf("RAG service not initialized")
	}

	if time.Since(s.lastRefreshTime) > DefaultRefreshThreshold {
		s.Logger.Info("Refreshing RAG index due to age threshold", 
			zap.Duration("age", time.Since(s.lastRefreshTime)),
			zap.Duration("threshold", DefaultRefreshThreshold))
		if err := s.RefreshIndex(ctx); err != nil {
			s.Logger.Warn("Failed to refresh index", zap.Error(err))
		}
	}

	// Process options
	topK := s.TopK
	if val, ok := options["top_k"]; ok {
		if k, ok := val.(int); ok {
			topK = k
		}
	}

	minScore := s.MinScore
	if val, ok := options["min_score"]; ok {
		if score, ok := val.(float32); ok {
			minScore = score
		}
	}

	includeMetadata := true
	if val, ok := options["include_metadata"]; ok {
		if include, ok := val.(bool); ok {
			includeMetadata = include
		}
	}

	filters := make(map[string]string)
	if val, ok := options["filters"]; ok {
		if filterMap, ok := val.(map[string]string); ok {
			filters = filterMap
		}
	}

	// Generate embedding for the query
	queryVector, err := s.EmbeddingService.GetEmbedding(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to generate query embedding: %w", err)
	}

	// Query the vector database
	vectorQuery := &models.VectorQuery{
		QueryVector:     queryVector,
		TopK:            topK,
		Filters:         filters,
		IncludeMetadata: includeMetadata,
		IncludeVectors:  false,
		MinScore:        minScore,
	}

	result, err := s.VectorDB.QueryEmbeddings(ctx, s.MockCollection, vectorQuery)
	if err != nil {
		return nil, fmt.Errorf("failed to query embeddings: %w", err)
	}

	s.Logger.Debug("Retrieved context", 
		zap.String("query", query), 
		zap.Int("results", result.TotalResults),
		zap.Int64("query_time_ms", result.QueryTime))
	return result, nil
}

// UpdateContext implements the RAGService interface
func (s *RAGServiceImpl) UpdateContext(ctx context.Context, mock *models.Mock) error {
	s.mu.RLock()
	initialized := s.Initialized
	s.mu.RUnlock()

	if !initialized {
		return fmt.Errorf("RAG service not initialized")
	}

	// First delete existing context for this mock
	err := s.DeleteContext(ctx, mock.Name)
	if err != nil {
		return fmt.Errorf("failed to delete existing context: %w", err)
	}

	// Then index the mock
	err = s.IndexMock(ctx, mock)
	if err != nil {
		return fmt.Errorf("failed to index mock: %w", err)
	}

	return nil
}

// DeleteContext implements the RAGService interface
func (s *RAGServiceImpl) DeleteContext(ctx context.Context, mockID string) error {
	s.mu.RLock()
	initialized := s.Initialized
	s.mu.RUnlock()

	if !initialized {
		return fmt.Errorf("RAG service not initialized")
	}

	// Query to find all embeddings for this mock
	filters := map[string]string{
		"mock_id": mockID,
	}

	vectorQuery := &models.VectorQuery{
		TopK:            1000, // Get all embeddings for this mock
		Filters:         filters,
		IncludeMetadata: false,
		IncludeVectors:  false,
	}

	result, err := s.VectorDB.QueryEmbeddings(ctx, s.MockCollection, vectorQuery)
	if err != nil {
		return fmt.Errorf("failed to query embeddings: %w", err)
	}

	if result.TotalResults == 0 {
		return nil
	}

	// Extract IDs
	ids := make([]string, len(result.Results))
	for i, r := range result.Results {
		ids[i] = r.ID
	}

	// Delete embeddings
	err = s.VectorDB.DeleteEmbeddings(ctx, s.MockCollection, ids)
	if err != nil {
		return fmt.Errorf("failed to delete embeddings: %w", err)
	}

	s.Logger.Info("Deleted context", zap.String("mock_id", mockID), zap.Int("embeddings", len(ids)))
	return nil
}

// RefreshIndex implements the RAGService interface
func (s *RAGServiceImpl) RefreshIndex(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.Initialized {
		return fmt.Errorf("RAG service not initialized")
	}

	s.Logger.Info("Refreshing RAG index")
	s.lastRefreshTime = time.Now()
	return nil
}

// extractChunksFromMock extracts content chunks from a mock for indexing
func (s *RAGServiceImpl) extractChunksFromMock(mock *models.Mock) ([]string, error) {
	// First attempt to use semantic chunking based on mock type
	chunks, err := s.semanticChunkingByMockType(mock)
	if err != nil || len(chunks) == 0 {
		// Fall back to basic chunking if semantic chunking fails
		return s.basicChunkingFromMock(mock)
	}
	
	return chunks, nil
}

// semanticChunkingByMockType extracts chunks from a mock based on its type
// This provides more meaningful chunks than simple text splitting
func (s *RAGServiceImpl) semanticChunkingByMockType(mock *models.Mock) ([]string, error) {
	var chunks []string
	
	// Extract different parts based on the mock type
	switch mock.Kind {
	case "HTTP":
		// For HTTP mocks, split request and response into separate chunks
		if mock.Spec.HTTPReq != nil {
			reqChunk, err := s.extractHTTPRequestChunk(mock.Spec.HTTPReq)
			if err == nil && reqChunk != "" {
				chunks = append(chunks, reqChunk)
			}
		}
		
		if mock.Spec.HTTPResp != nil {
			respChunk, err := s.extractHTTPResponseChunk(mock.Spec.HTTPResp)
			if err == nil && respChunk != "" {
				chunks = append(chunks, respChunk)
			}
		}
	
	case "Redis":
		// For Redis mocks, extract each request and response pair
		for i, req := range mock.Spec.RedisRequests {
			reqJSON, err := json.Marshal(req)
			if err == nil {
				chunks = append(chunks, string(reqJSON))
			}
			
			// Include corresponding response if available
			if i < len(mock.Spec.RedisResponses) {
				respJSON, err := json.Marshal(mock.Spec.RedisResponses[i])
				if err == nil {
					chunks = append(chunks, string(respJSON))
				}
			}
		}
	
	case "Mongo":
		// For MongoDB mocks, extract each request and response pair
		for i, req := range mock.Spec.MongoRequests {
			reqJSON, err := json.Marshal(req)
			if err == nil {
				chunks = append(chunks, string(reqJSON))
			}
			
			// Include corresponding response if available
			if i < len(mock.Spec.MongoResponses) {
				respJSON, err := json.Marshal(mock.Spec.MongoResponses[i])
				if err == nil {
					chunks = append(chunks, string(respJSON))
				}
			}
		}
		
	case "Postgres":
		// For PostgreSQL mocks, extract each request and response pair
		for i, req := range mock.Spec.PostgresRequests {
			reqJSON, err := json.Marshal(req)
			if err == nil {
				chunks = append(chunks, string(reqJSON))
			}
			
			// Include corresponding response if available
			if i < len(mock.Spec.PostgresResponses) {
				respJSON, err := json.Marshal(mock.Spec.PostgresResponses[i])
				if err == nil {
					chunks = append(chunks, string(respJSON))
				}
			}
		}
		
	case "gRPC":
		// For gRPC mocks, extract request and response
		if mock.Spec.GRPCReq != nil {
			reqJSON, err := json.Marshal(mock.Spec.GRPCReq)
			if err == nil {
				chunks = append(chunks, string(reqJSON))
			}
		}
		
		if mock.Spec.GRPCResp != nil {
			respJSON, err := json.Marshal(mock.Spec.GRPCResp)
			if err == nil {
				chunks = append(chunks, string(respJSON))
			}
		}
		
	case "MySQL":
		// For MySQL mocks, extract each request and response pair
		for i, req := range mock.Spec.MySQLRequests {
			reqJSON, err := json.Marshal(req)
			if err == nil {
				chunks = append(chunks, string(reqJSON))
			}
			
			// Include corresponding response if available
			if i < len(mock.Spec.MySQLResponses) {
				respJSON, err := json.Marshal(mock.Spec.MySQLResponses[i])
				if err == nil {
					chunks = append(chunks, string(respJSON))
				}
			}
		}
	}
	
	// Add mock metadata as a separate chunk
	metadataChunk := fmt.Sprintf("Name: %s, Kind: %s, Version: %s", 
		mock.Name, string(mock.Kind), string(mock.Version))
	chunks = append(chunks, metadataChunk)
	
	// If metadata map exists, add it as a chunk
	if len(mock.Spec.Metadata) > 0 {
		metaJSON, err := json.Marshal(mock.Spec.Metadata)
		if err == nil {
			chunks = append(chunks, string(metaJSON))
		}
	}
	
	return chunks, nil
}

// extractHTTPRequestChunk creates a structured representation of an HTTP request
func (s *RAGServiceImpl) extractHTTPRequestChunk(req *models.HTTPReq) (string, error) {
	if req == nil {
		return "", fmt.Errorf("nil HTTP request")
	}
	
	// Create a structured representation of the request
	var sb strings.Builder
	
	sb.WriteString(fmt.Sprintf("%s %s HTTP/1.1\n", req.Method, req.Path))
	sb.WriteString(fmt.Sprintf("URL: %s\n", req.URL))
	
	// Add headers
	if len(req.Header) > 0 {
		for key, values := range req.Header {
			for _, value := range values {
				sb.WriteString(fmt.Sprintf("%s: %s\n", key, value))
			}
		}
	}
	
	// Add query parameters if present
	if len(req.Query) > 0 {
		sb.WriteString("Query Parameters:\n")
		for key, values := range req.Query {
			for _, value := range values {
				sb.WriteString(fmt.Sprintf("%s=%s\n", key, value))
			}
		}
	}
	
	// Add body if present
	if len(req.Body) > 0 {
		sb.WriteString("\n")
		// For large bodies, summarize or truncate
		if len(req.Body) > 1000 {
			sb.Write(req.Body[:1000])
			sb.WriteString("... [truncated]")
		} else {
			sb.Write(req.Body)
		}
	}
	
	return sb.String(), nil
}

// extractHTTPResponseChunk creates a structured representation of an HTTP response
func (s *RAGServiceImpl) extractHTTPResponseChunk(resp *models.HTTPResp) (string, error) {
	if resp == nil {
		return "", fmt.Errorf("nil HTTP response")
	}
	
	// Create a structured representation of the response
	var sb strings.Builder
	
	sb.WriteString(fmt.Sprintf("HTTP/1.1 %d\n", resp.StatusCode))
	
	// Add headers
	if len(resp.Header) > 0 {
		for key, values := range resp.Header {
			for _, value := range values {
				sb.WriteString(fmt.Sprintf("%s: %s\n", key, value))
			}
		}
	}
	
	// Add body if present
	if len(resp.Body) > 0 {
		sb.WriteString("\n")
		// For large bodies, summarize or truncate
		if len(resp.Body) > 1000 {
			sb.Write(resp.Body[:1000])
			sb.WriteString("... [truncated]")
		} else {
			sb.Write(resp.Body)
		}
	}
	
	return sb.String(), nil
}

// basicChunkingFromMock falls back to basic JSON chunking if semantic chunking fails
func (s *RAGServiceImpl) basicChunkingFromMock(mock *models.Mock) ([]string, error) {
	// Convert the mock to JSON for text indexing
	mockJSON, err := json.Marshal(mock)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal mock: %w", err)
	}
	
	text := string(mockJSON)
	
	// Check if text is small enough to be used as a single chunk
	if len(text) <= s.ChunkSize {
		return []string{text}, nil
	}
	
	// For larger texts, use overlapping chunks
	return s.splitIntoChunks(text, s.ChunkSize, s.ChunkOverlap), nil
}

// splitIntoChunks splits text into chunks with a specified size and overlap
func (s *RAGServiceImpl) splitIntoChunks(text string, chunkSize, overlap int) []string {
	if len(text) <= chunkSize {
		return []string{text}
	}

	var chunks []string
	for i := 0; i < len(text); i += chunkSize - overlap {
		end := i + chunkSize
		if end > len(text) {
			end = len(text)
		}
		chunks = append(chunks, text[i:end])
		if end == len(text) {
			break
		}
	}
	return chunks
}

// semanticSplitJSONChunks attempts to split JSON by logical boundaries
func (s *RAGServiceImpl) semanticSplitJSONChunks(text string) ([]string, error) {
	var jsonObj map[string]interface{}
	if err := json.Unmarshal([]byte(text), &jsonObj); err != nil {
		return nil, err
	}
	
	var chunks []string
	
	// Process each top-level key separately
	for key, value := range jsonObj {
		// Marshal this key-value pair
		subMap := map[string]interface{}{key: value}
		subJSON, err := json.Marshal(subMap)
		if err != nil {
			continue
		}
		
		// If the resulting JSON is too large, process it recursively
		if len(subJSON) > s.ChunkSize {
			// For arrays, process each element separately
			if arr, ok := value.([]interface{}); ok {
				for _, item := range arr {
					itemJSON, err := json.Marshal(item)
					if err != nil {
						continue
					}
					
					if len(itemJSON) <= s.ChunkSize {
						chunks = append(chunks, string(itemJSON))
					} else {
						// Split large items further
						for _, chunk := range s.splitIntoChunks(string(itemJSON), s.ChunkSize, s.ChunkOverlap) {
							chunks = append(chunks, chunk)
						}
					}
				}
			} else {
				// For other large objects, use regular chunking
				for _, chunk := range s.splitIntoChunks(string(subJSON), s.ChunkSize, s.ChunkOverlap) {
					chunks = append(chunks, chunk)
				}
			}
		} else {
			chunks = append(chunks, string(subJSON))
		}
	}
	
	return chunks, nil
} 