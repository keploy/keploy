package vector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

const (
	DefaultChromaDBTimeout = 30 * time.Second
	DefaultChromaBatchSize = 100
)

// ChromaDBService implements the VectorDBService interface using ChromaDB
type ChromaDBService struct {
	BaseURL    string
	HTTPClient *http.Client
	Logger     *zap.Logger
	BatchSize  int
}

// ChromaCollection represents a ChromaDB collection
type ChromaCollection struct {
	Name      string `json:"name"`
	Metadata  any    `json:"metadata,omitempty"`
	GetOrCreate bool `json:"get_or_create,omitempty"`
}

// ChromaEmbedding represents a document in ChromaDB
type ChromaEmbedding struct {
	ID        string                 `json:"id"`
	Embedding []float32              `json:"embedding,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	Document  string                 `json:"document,omitempty"`
}

// ChromaAddRequest represents a request to add documents to ChromaDB
type ChromaAddRequest struct {
	IDs       []string                `json:"ids"`
	Embeddings [][]float32             `json:"embeddings,omitempty"`
	Metadatas []map[string]interface{} `json:"metadatas,omitempty"`
	Documents []string                 `json:"documents,omitempty"`
}

// ChromaQueryRequest represents a request to query ChromaDB
type ChromaQueryRequest struct {
	QueryEmbeddings [][]float32             `json:"query_embeddings,omitempty"`
	QueryTexts      []string                 `json:"query_texts,omitempty"`
	NResults        int                      `json:"n_results"`
	Where           map[string]interface{}   `json:"where,omitempty"`
	WhereDocument   map[string]interface{}   `json:"where_document,omitempty"`
	Include         []string                 `json:"include,omitempty"`
}

// ChromaQueryResult represents a result from a ChromaDB query
type ChromaQueryResult struct {
	IDs        [][]string                 `json:"ids"`
	Embeddings [][][]float32              `json:"embeddings,omitempty"`
	Documents  [][]string                 `json:"documents,omitempty"`
	Metadatas  [][]map[string]interface{} `json:"metadatas,omitempty"`
	Distances  [][]float32                `json:"distances,omitempty"`
}

// ChromaGetRequest represents a request to get documents from ChromaDB
type ChromaGetRequest struct {
	IDs     []string               `json:"ids,omitempty"`
	Where   map[string]interface{} `json:"where,omitempty"`
	Limit   int                    `json:"limit,omitempty"`
	Offset  int                    `json:"offset,omitempty"`
	Include []string               `json:"include,omitempty"`
}

// ChromaDeleteRequest represents a request to delete documents from ChromaDB
type ChromaDeleteRequest struct {
	IDs   []string               `json:"ids,omitempty"`
	Where map[string]interface{} `json:"where,omitempty"`
}

// NewChromaDBService creates a new ChromaDB service
func NewChromaDBService(logger *zap.Logger, baseURL string) *ChromaDBService {
	if baseURL == "" {
		baseURL = "http://localhost:8000" // Default ChromaDB address
	}

	return &ChromaDBService{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: DefaultChromaDBTimeout},
		Logger:     logger,
		BatchSize:  DefaultChromaBatchSize,
	}
}

// Initialize implements the VectorDBService interface
func (s *ChromaDBService) Initialize(ctx context.Context) error {
	// Check if ChromaDB is accessible
	req, err := http.NewRequestWithContext(ctx, "GET", s.BaseURL+"/api/v1/heartbeat", nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to ChromaDB: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ChromaDB returned unexpected status: %d", resp.StatusCode)
	}

	s.Logger.Info("Connected to ChromaDB", zap.String("baseURL", s.BaseURL))
	return nil
}

// Close implements the VectorDBService interface
func (s *ChromaDBService) Close(ctx context.Context) error {
	return nil
}

// CreateCollection implements the VectorDBService interface
func (s *ChromaDBService) CreateCollection(ctx context.Context, name string, dimension int) error {
	collection := ChromaCollection{
		Name: name,
		GetOrCreate: true,
	}

	reqBytes, err := json.Marshal(collection)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/v1/collections", s.BaseURL), bytes.NewBuffer(reqBytes))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to create collection: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("failed to create collection, status: %d", resp.StatusCode)
	}

	s.Logger.Info("Created ChromaDB collection", zap.String("name", name))
	return nil
}

// DeleteCollection implements the VectorDBService interface
func (s *ChromaDBService) DeleteCollection(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE", fmt.Sprintf("%s/api/v1/collections/%s", s.BaseURL, name), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to delete collection: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("failed to delete collection, status: %d", resp.StatusCode)
	}

	s.Logger.Info("Deleted ChromaDB collection", zap.String("name", name))
	return nil
}

// ListCollections implements the VectorDBService interface
func (s *ChromaDBService) ListCollections(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/v1/collections", s.BaseURL), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to list collections: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to list collections, status: %d", resp.StatusCode)
	}

	var result struct {
		Collections []struct {
			Name     string `json:"name"`
			ID       string `json:"id"`
			Metadata any    `json:"metadata"`
		} `json:"collections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	collections := make([]string, len(result.Collections))
	for i, col := range result.Collections {
		collections[i] = col.Name
	}

	return collections, nil
}

// UpsertEmbedding implements the VectorDBService interface
func (s *ChromaDBService) UpsertEmbedding(ctx context.Context, collection string, embedding *models.Embedding) error {
	embeddings := []*models.Embedding{embedding}
	return s.UpsertEmbeddings(ctx, collection, embeddings)
}

// UpsertEmbeddings implements the VectorDBService interface
func (s *ChromaDBService) UpsertEmbeddings(ctx context.Context, collection string, embeddings []*models.Embedding) error {
	if len(embeddings) == 0 {
		return nil
	}

	// Process in batches to avoid exceeding API limits
	for i := 0; i < len(embeddings); i += s.BatchSize {
		end := i + s.BatchSize
		if end > len(embeddings) {
			end = len(embeddings)
		}
		batch := embeddings[i:end]

		ids := make([]string, len(batch))
		embeddingVectors := make([][]float32, len(batch))
		documents := make([]string, len(batch))
		metadatas := make([]map[string]interface{}, len(batch))

		for j, emb := range batch {
			ids[j] = emb.ID
			embeddingVectors[j] = emb.Vector
			documents[j] = emb.Content

			// Convert the metadata to a map[string]interface{}
			metadata := make(map[string]interface{})
			metadataBytes, err := json.Marshal(emb.Metadata)
			if err != nil {
				s.Logger.Warn("Failed to marshal metadata", zap.Error(err))
				continue
			}
			if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
				s.Logger.Warn("Failed to unmarshal metadata", zap.Error(err))
				continue
			}
			metadatas[j] = metadata
		}

		addRequest := ChromaAddRequest{
			IDs:        ids,
			Embeddings: embeddingVectors,
			Documents:  documents,
			Metadatas:  metadatas,
		}

		reqBytes, err := json.Marshal(addRequest)
		if err != nil {
			return fmt.Errorf("failed to marshal request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/v1/collections/%s/add", s.BaseURL, collection), bytes.NewBuffer(reqBytes))
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := s.HTTPClient.Do(req)
		if err != nil {
			return fmt.Errorf("failed to upsert embeddings: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("failed to upsert embeddings, status: %d", resp.StatusCode)
		}
	}

	s.Logger.Debug("Upserted embeddings", zap.Int("count", len(embeddings)), zap.String("collection", collection))
	return nil
}

// DeleteEmbedding implements the VectorDBService interface
func (s *ChromaDBService) DeleteEmbedding(ctx context.Context, collection string, id string) error {
	return s.DeleteEmbeddings(ctx, collection, []string{id})
}

// DeleteEmbeddings implements the VectorDBService interface
func (s *ChromaDBService) DeleteEmbeddings(ctx context.Context, collection string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	deleteRequest := ChromaDeleteRequest{
		IDs: ids,
	}

	reqBytes, err := json.Marshal(deleteRequest)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/v1/collections/%s/delete", s.BaseURL, collection), bytes.NewBuffer(reqBytes))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to delete embeddings: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("failed to delete embeddings, status: %d", resp.StatusCode)
	}

	s.Logger.Debug("Deleted embeddings", zap.Int("count", len(ids)), zap.String("collection", collection))
	return nil
}

// GetEmbedding implements the VectorDBService interface
func (s *ChromaDBService) GetEmbedding(ctx context.Context, collection string, id string) (*models.Embedding, error) {
	getRequest := ChromaGetRequest{
		IDs:     []string{id},
		Include: []string{"embeddings", "documents", "metadatas"},
	}

	reqBytes, err := json.Marshal(getRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/v1/collections/%s/get", s.BaseURL, collection), bytes.NewBuffer(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get embedding: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get embedding, status: %d", resp.StatusCode)
	}

	var result struct {
		IDs        []string                 `json:"ids"`
		Embeddings [][]float32              `json:"embeddings"`
		Documents  []string                 `json:"documents"`
		Metadatas  []map[string]interface{} `json:"metadatas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(result.IDs) == 0 {
		return nil, fmt.Errorf("embedding not found: %s", id)
	}

	// Convert the metadata back to our model
	var metadata models.Metadata
	metadataBytes, err := json.Marshal(result.Metadatas[0])
	if err != nil {
		return nil, fmt.Errorf("failed to marshal metadata: %w", err)
	}
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return nil, fmt.Errorf("failed to unmarshal metadata: %w", err)
	}

	embedding := &models.Embedding{
		ID:       result.IDs[0],
		Vector:   result.Embeddings[0],
		Content:  result.Documents[0],
		Metadata: metadata,
	}

	return embedding, nil
}

// QueryEmbeddings implements the VectorDBService interface
func (s *ChromaDBService) QueryEmbeddings(ctx context.Context, collection string, query *models.VectorQuery) (*models.VectorQueryResult, error) {
	startTime := time.Now()

	// Build query filters if needed
	var where map[string]interface{}
	if len(query.Filters) > 0 {
		where = make(map[string]interface{})
		for k, v := range query.Filters {
			where[k] = v
		}
	}

	include := []string{"documents", "metadatas", "distances"}
	if query.IncludeVectors {
		include = append(include, "embeddings")
	}

	chromaQuery := ChromaQueryRequest{
		NResults: query.TopK,
		Include:  include,
		Where:    where,
	}

	// Use either the query vector or the query text
	if len(query.QueryVector) > 0 {
		chromaQuery.QueryEmbeddings = [][]float32{query.QueryVector}
	} else if query.Query != "" {
		chromaQuery.QueryTexts = []string{query.Query}
	} else {
		return nil, fmt.Errorf("either query or query_vector must be provided")
	}

	reqBytes, err := json.Marshal(chromaQuery)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/v1/collections/%s/query", s.BaseURL, collection), bytes.NewBuffer(reqBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to query embeddings: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to query embeddings, status: %d", resp.StatusCode)
	}

	var result ChromaQueryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Convert the query results to our model
	var searchResults []models.SearchResult
	if len(result.IDs) > 0 && len(result.IDs[0]) > 0 {
		for i, id := range result.IDs[0] {
			var metadata models.Metadata
			metadataBytes, err := json.Marshal(result.Metadatas[0][i])
			if err != nil {
				s.Logger.Warn("Failed to marshal metadata", zap.Error(err))
				continue
			}
			if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
				s.Logger.Warn("Failed to unmarshal metadata", zap.Error(err))
				continue
			}

			// Apply enhanced scoring algorithm instead of basic distance conversion
			distance := result.Distances[0][i]
			score := calculateEnhancedScore(distance, &metadata, query.Query)
			
			// Apply minimum score filter
			if query.MinScore > 0 && score < query.MinScore {
				continue
			}

			searchResult := models.SearchResult{
				ID:       id,
				Content:  result.Documents[0][i],
				Metadata: metadata,
				Score:    score,
				Distance: distance,
			}
			searchResults = append(searchResults, searchResult)
		}
	}

	// Sort results by enhanced score in descending order
	sort.Slice(searchResults, func(i, j int) bool {
		return searchResults[i].Score > searchResults[j].Score
	})

	queryTime := time.Since(startTime).Milliseconds()
	return &models.VectorQueryResult{
		Results:      searchResults,
		TotalResults: len(searchResults),
		QueryTime:    queryTime,
	}, nil
}

// calculateEnhancedScore provides a more sophisticated scoring algorithm
// that considers multiple factors beyond just vector distance
func calculateEnhancedScore(distance float32, metadata *models.Metadata, query string) float32 {
	// Base score from distance (reversed to make it a similarity score)
	baseScore := 1.0 - distance
	
	// Weight for the base score (vector similarity)
	const baseScoreWeight = 0.7
	
	// Content relevance based on kind matching
	contentRelevanceWeight := 0.15
	contentRelevance := calculateContentRelevance(metadata, query)
	
	// Recency boost - newer documents get slightly higher scores
	recencyWeight := 0.05
	recencyBoost := calculateRecencyBoost(metadata.CreatedAt)
	
	// Domain specificity boost - higher score for matching the kind
	domainWeight := 0.1
	domainBoost := calculateDomainBoost(string(metadata.Kind), query)
	
	// Combine scores with weighting
	weightedScore := (baseScore * baseScoreWeight) + 
	                 (contentRelevance * contentRelevanceWeight) + 
	                 (recencyBoost * recencyWeight) + 
	                 (domainBoost * domainWeight)
	
	// Ensure score is between 0 and 1
	if weightedScore > 1.0 {
		weightedScore = 1.0
	} else if weightedScore < 0.0 {
		weightedScore = 0.0
	}
	
	return weightedScore
}

// calculateContentRelevance determines how relevant the content is to the query
// based on keyword matching and other factors
func calculateContentRelevance(metadata *models.Metadata, query string) float32 {
	if query == "" || metadata == nil {
		return 0.0
	}
	
	relevance := float32(0.0)
	
	// Check for important keywords in query that match metadata
	queryLower := strings.ToLower(query)
	
	// Match against the source
	if metadata.Source != "" && strings.Contains(queryLower, strings.ToLower(metadata.Source)) {
		relevance += 0.3
	}
	
	// Match against labels
	if metadata.Labels != nil {
		for key, value := range metadata.Labels {
			if strings.Contains(queryLower, strings.ToLower(key)) || 
			   strings.Contains(queryLower, strings.ToLower(value)) {
				relevance += 0.2
				break
			}
		}
	}
	
	// Match against MockID
	if metadata.MockID != "" && strings.Contains(queryLower, strings.ToLower(metadata.MockID)) {
		relevance += 0.2
	}
	
	// Cap relevance at 1.0
	if relevance > 1.0 {
		relevance = 1.0
	}
	
	return relevance
}

// calculateRecencyBoost gives a small boost to more recent documents
func calculateRecencyBoost(createdAt int64) float32 {
	if createdAt == 0 {
		return 0.0
	}
	
	// Calculate how recent the document is (in days)
	nowUnix := time.Now().Unix()
	ageInSeconds := nowUnix - createdAt
	ageInDays := ageInSeconds / (60 * 60 * 24)
	
	// Documents less than 7 days old get maximum boost
	if ageInDays < 7 {
		return 1.0
	}
	
	// Documents less than 30 days old get moderate boost
	if ageInDays < 30 {
		return 0.7
	}
	
	// Documents less than 90 days old get small boost
	if ageInDays < 90 {
		return 0.4
	}
	
	// Older documents get minimal boost
	return 0.1
}

// calculateDomainBoost gives a boost if the kind/domain matches the query
func calculateDomainBoost(kind string, query string) float32 {
	if kind == "" || query == "" {
		return 0.0
	}
	
	queryLower := strings.ToLower(query)
	kindLower := strings.ToLower(kind)
	
	// Direct match with the kind
	if strings.Contains(queryLower, kindLower) {
		return 1.0
	}
	
	// Check for related terms based on the kind
	switch kindLower {
	case "http":
		for _, term := range []string{"get", "post", "put", "delete", "api", "rest", "http"} {
			if strings.Contains(queryLower, term) {
				return 0.8
			}
		}
	case "redis":
		for _, term := range []string{"cache", "set", "get", "redis"} {
			if strings.Contains(queryLower, term) {
				return 0.8
			}
		}
	case "mongo":
		for _, term := range []string{"document", "collection", "query", "mongo", "mongodb"} {
			if strings.Contains(queryLower, term) {
				return 0.8
			}
		}
	case "postgres", "mysql":
		for _, term := range []string{"sql", "database", "query", "table", "db"} {
			if strings.Contains(queryLower, term) {
				return 0.8
			}
		}
	}
	
	return 0.0
} 