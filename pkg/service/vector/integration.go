package vector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

const (
	// Environment variables for configuration
	EnvVectorDBEnabled        = "KEPLOY_VECTOR_DB_ENABLED"
	EnvVectorDBType           = "KEPLOY_VECTOR_DB_TYPE"
	EnvEmbeddingModelType     = "KEPLOY_EMBEDDING_MODEL_TYPE"
	EnvVectorDBMinScore       = "KEPLOY_VECTOR_DB_MIN_SCORE"
	EnvVectorDBTopK           = "KEPLOY_VECTOR_DB_TOP_K"
	EnvVectorDBFallbackToFuzzy = "KEPLOY_VECTOR_DB_FALLBACK_TO_FUZZY"
	
	// Default configuration values
	DefaultVectorDBEnabled     = false
	DefaultVectorDBType        = VectorDBTypeChroma
	DefaultEmbeddingModelType  = EmbeddingModelTypeOpenAI
	DefaultVectorDBMinScore    = 0.6
	DefaultVectorDBTopK        = 3
	DefaultFallbackToFuzzy     = true
)

// IntegrationService provides integration between the RAG vector database and the existing fuzzy matching system
type IntegrationService struct {
	Logger            *zap.Logger
	Factory           *Factory
	Enabled           bool
	MinScore          float32
	TopK              int
	FallbackToFuzzy   bool
	mu                sync.RWMutex
	initialized       bool
	ragService        RAGService
}

// NewIntegrationService creates a new integration service
func NewIntegrationService(logger *zap.Logger) *IntegrationService {
	// Read configuration from environment variables
	enabledStr := os.Getenv(EnvVectorDBEnabled)
	enabled := DefaultVectorDBEnabled
	if enabledStr != "" {
		var err error
		enabled, err = strconv.ParseBool(enabledStr)
		if err != nil {
			logger.Warn("Invalid value for KEPLOY_VECTOR_DB_ENABLED, defaulting to false",
				zap.String("value", enabledStr),
				zap.Error(err))
		}
	}
	
	vectorDBType := os.Getenv(EnvVectorDBType)
	if vectorDBType == "" {
		vectorDBType = DefaultVectorDBType
	}
	
	embeddingModelType := os.Getenv(EnvEmbeddingModelType)
	if embeddingModelType == "" {
		embeddingModelType = DefaultEmbeddingModelType
	}
	
	minScoreStr := os.Getenv(EnvVectorDBMinScore)
	minScore := DefaultVectorDBMinScore
	if minScoreStr != "" {
		score, err := strconv.ParseFloat(minScoreStr, 32)
		if err != nil {
			logger.Warn("Invalid value for KEPLOY_VECTOR_DB_MIN_SCORE, using default",
				zap.String("value", minScoreStr),
				zap.Float32("default", DefaultVectorDBMinScore),
				zap.Error(err))
		} else {
			minScore = float32(score)
		}
	}
	
	topKStr := os.Getenv(EnvVectorDBTopK)
	topK := DefaultVectorDBTopK
	if topKStr != "" {
		k, err := strconv.Atoi(topKStr)
		if err != nil {
			logger.Warn("Invalid value for KEPLOY_VECTOR_DB_TOP_K, using default",
				zap.String("value", topKStr),
				zap.Int("default", DefaultVectorDBTopK),
				zap.Error(err))
		} else {
			topK = k
		}
	}
	
	fallbackToFuzzyStr := os.Getenv(EnvVectorDBFallbackToFuzzy)
	fallbackToFuzzy := DefaultFallbackToFuzzy
	if fallbackToFuzzyStr != "" {
		var err error
		fallbackToFuzzy, err = strconv.ParseBool(fallbackToFuzzyStr)
		if err != nil {
			logger.Warn("Invalid value for KEPLOY_VECTOR_DB_FALLBACK_TO_FUZZY, defaulting to true",
				zap.String("value", fallbackToFuzzyStr),
				zap.Error(err))
		}
	}
	
	s := &IntegrationService{
		Logger:          logger,
		Enabled:         enabled,
		MinScore:        minScore,
		TopK:            topK,
		FallbackToFuzzy: fallbackToFuzzy,
	}
	
	if enabled {
		s.Factory = NewFactory(logger, vectorDBType, embeddingModelType)
	}
	
	return s
}

// Initialize initializes the integration service
func (s *IntegrationService) Initialize(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	if s.initialized {
		return nil
	}
	
	if !s.Enabled {
		s.Logger.Info("Vector database integration is disabled")
		s.initialized = true
		return nil
	}
	
	// Initialize the RAG service
	ragService, err := s.Factory.GetRAGService(ctx)
	if err != nil {
		return fmt.Errorf("failed to initialize RAG service: %w", err)
	}
	
	s.ragService = ragService
	s.initialized = true
	
	s.Logger.Info("Vector database integration is enabled",
		zap.String("vector_db", s.Factory.vectorDBType),
		zap.String("embedding_model", s.Factory.embeddingModel),
		zap.Float32("min_score", s.MinScore),
		zap.Int("top_k", s.TopK),
		zap.Bool("fallback_to_fuzzy", s.FallbackToFuzzy))
		
	return nil
}

// Close closes the integration service
func (s *IntegrationService) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	if !s.initialized {
		return nil
	}
	
	s.initialized = false
	
	if s.Factory != nil {
		err := s.Factory.Close(ctx)
		if err != nil {
			return fmt.Errorf("failed to close factory: %w", err)
		}
	}
	
	return nil
}

// IndexMock indexes a mock in the vector database
func (s *IntegrationService) IndexMock(ctx context.Context, mock *models.Mock) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	
	if !s.Enabled || !s.initialized || s.ragService == nil {
		return nil
	}
	
	err := s.ragService.IndexMock(ctx, mock)
	if err != nil {
		return fmt.Errorf("failed to index mock: %w", err)
	}
	
	return nil
}

// FindMockByVectorSimilarity finds a mock by vector similarity
// If successful, returns the matching mock index and true
// If no match is found, returns -1 and false
func (s *IntegrationService) FindMockByVectorSimilarity(ctx context.Context, reqBuff []byte, mocks []*models.Mock, kind string) (int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	
	if !s.Enabled || !s.initialized || s.ragService == nil {
		return -1, false
	}
	
	// If no mocks available, return early
	if len(mocks) == 0 {
		return -1, false
	}
	
	// Use the hybrid search approach that combines vector similarity with fuzzy matching
	return s.findByHybridSearch(ctx, reqBuff, mocks, kind)
}

// findByHybridSearch combines vector similarity with fuzzy matching for more accurate results
func (s *IntegrationService) findByHybridSearch(ctx context.Context, reqBuff []byte, mocks []*models.Mock, kind string) (int, bool) {
	// Convert the request buffer to a string for the query
	query := string(reqBuff)
	
	// Set up options for the vector query
	options := map[string]interface{}{
		"top_k":     s.TopK * 2, // Get more results for re-ranking
		"min_score": s.MinScore * 0.8, // Lower threshold to get more candidates
		"filters": map[string]string{
			"kind": kind,
		},
	}
	
	// Query the vector database
	vectorResults, err := s.ragService.RetrieveContext(ctx, query, options)
	if err != nil {
		s.Logger.Warn("Failed to query vector database", zap.Error(err))
		return -1, false
	}
	
	if vectorResults.TotalResults == 0 {
		s.Logger.Debug("No matches found in vector database")
		return -1, false
	}
	
	// Calculate fuzzy match scores for all mocks
	fuzzyScores := make(map[string]float32)
	for _, mock := range mocks {
		// Skip mocks of different kind
		if string(mock.Kind) != kind {
			continue
		}
		
		// Convert mock to string for fuzzy matching
		mockString, err := ConvertMockToJSON(mock)
		if err != nil {
			s.Logger.Warn("Failed to convert mock to JSON", 
				zap.String("mock_id", mock.Name),
				zap.Error(err))
			continue
		}
		
		// Calculate fuzzy match score
		fuzzyScore := calculateFuzzyScore([]byte(mockString), reqBuff)
		fuzzyScores[mock.Name] = fuzzyScore
	}
	
	// Combine vector and fuzzy scores
	combinedScores := make(map[string]struct{
		Score     float32
		MockIndex int
	})
	
	// Add vector results to combined scores
	for _, result := range vectorResults.Results {
		mockID := result.Metadata.MockID
		vectorScore := result.Score
		
		// Find corresponding mock index
		mockIndex := -1
		for idx, mock := range mocks {
			if mock.Name == mockID {
				mockIndex = idx
				break
			}
		}
		
		if mockIndex == -1 {
			continue
		}
		
		// Get fuzzy score, default to 0 if not found
		fuzzyScore, ok := fuzzyScores[mockID]
		if !ok {
			fuzzyScore = 0
		}
		
		// Weight vector similarity higher than fuzzy matching
		const vectorWeight = 0.7
		const fuzzyWeight = 0.3
		
		combinedScore := (vectorScore * vectorWeight) + (fuzzyScore * fuzzyWeight)
		
		combinedScores[mockID] = struct{
			Score     float32
			MockIndex int
		}{
			Score:     combinedScore,
			MockIndex: mockIndex,
		}
	}
	
	// Find the best match
	var bestScore float32
	bestMockIndex := -1
	
	for _, score := range combinedScores {
		if score.Score > bestScore && score.Score >= s.MinScore {
			bestScore = score.Score
			bestMockIndex = score.MockIndex
		}
	}
	
	// Log the results
	if bestMockIndex != -1 {
		s.Logger.Debug("Found match using hybrid search", 
			zap.String("mock_id", mocks[bestMockIndex].Name),
			zap.Float32("score", bestScore))
		return bestMockIndex, true
	}
	
	return -1, false
}

// calculateFuzzyScore computes a fuzzy similarity score between two byte slices
func calculateFuzzyScore(text1, text2 []byte) float32 {
	// For short inputs, use exact matching
	if len(text1) < 50 || len(text2) < 50 {
		if bytes.Equal(text1, text2) {
			return 1.0
		}
	}
	
	// Use Jaccard similarity with adaptive shingle size
	k := adaptiveShingleSize(min(len(text1), len(text2)))
	shingles1 := createShingles(text1, k)
	shingles2 := createShingles(text2, k)
	
	return jaccardSimilarity(shingles1, shingles2)
}

// adaptiveShingleSize determines the appropriate shingle size based on document length
func adaptiveShingleSize(length int) int {
	switch {
	case length < 100:
		return 2
	case length < 1000:
		return 4
	case length < 10000:
		return 8
	default:
		return 12
	}
}

// createShingles creates a set of shingles from a byte slice
func createShingles(data []byte, k int) map[string]struct{} {
	shingles := make(map[string]struct{})
	for i := 0; i <= len(data)-k; i++ {
		shingle := string(data[i : i+k])
		shingles[shingle] = struct{}{}
	}
	return shingles
}

// jaccardSimilarity calculates the Jaccard similarity between two sets
func jaccardSimilarity(set1, set2 map[string]struct{}) float32 {
	if len(set1) == 0 && len(set2) == 0 {
		return 1.0
	}
	
	// Count the intersection
	intersection := 0
	for shingle := range set1 {
		if _, exists := set2[shingle]; exists {
			intersection++
		}
	}
	
	// Calculate the union size
	unionSize := len(set1) + len(set2) - intersection
	
	return float32(intersection) / float32(unionSize)
}

// min returns the smaller of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ShouldFallbackToFuzzy returns whether to fall back to fuzzy matching
func (s *IntegrationService) ShouldFallbackToFuzzy() bool {
	return s.FallbackToFuzzy
}

// IndexMocks indexes multiple mocks in the vector database
func (s *IntegrationService) IndexMocks(ctx context.Context, mocks []*models.Mock) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	
	if !s.Enabled || !s.initialized || s.ragService == nil {
		return nil
	}
	
	err := s.ragService.IndexMocks(ctx, mocks)
	if err != nil {
		return fmt.Errorf("failed to index mocks: %w", err)
	}
	
	return nil
}

// UpdateMock updates a mock in the vector database
func (s *IntegrationService) UpdateMock(ctx context.Context, mock *models.Mock) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	
	if !s.Enabled || !s.initialized || s.ragService == nil {
		return nil
	}
	
	err := s.ragService.UpdateContext(ctx, mock)
	if err != nil {
		return fmt.Errorf("failed to update mock: %w", err)
	}
	
	return nil
}

// DeleteMock deletes a mock from the vector database
func (s *IntegrationService) DeleteMock(ctx context.Context, mockID string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	
	if !s.Enabled || !s.initialized || s.ragService == nil {
		return nil
	}
	
	err := s.ragService.DeleteContext(ctx, mockID)
	if err != nil {
		return fmt.Errorf("failed to delete mock: %w", err)
	}
	
	return nil
}

// ConvertMockToJSON converts a mock to JSON format for indexing
func ConvertMockToJSON(mock *models.Mock) (string, error) {
	jsonBytes, err := json.Marshal(mock)
	if err != nil {
		return "", fmt.Errorf("failed to marshal mock: %w", err)
	}
	return string(jsonBytes), nil
}

// CleanTextForEmbedding cleans text for embedding generation
func CleanTextForEmbedding(text string) string {
	// Remove excessive whitespace
	text = strings.Join(strings.Fields(text), " ")
	return text
} 