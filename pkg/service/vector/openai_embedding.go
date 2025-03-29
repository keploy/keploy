package vector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
)

const (
	DefaultOpenAIEmbeddingModel = "text-embedding-3-small"
	DefaultOpenAITimeout        = 30 * time.Second
	DefaultOpenAIBatchSize      = 100
	DefaultOpenAIDimension      = 1536
)

// OpenAIEmbeddingRequest represents the request to the OpenAI embedding API
type OpenAIEmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// OpenAIEmbeddingResponse represents the response from the OpenAI embedding API
type OpenAIEmbeddingResponse struct {
	Object string                        `json:"object"`
	Data   []OpenAIEmbeddingResponseData `json:"data"`
	Model  string                        `json:"model"`
	Usage  OpenAIUsage                   `json:"usage"`
}

// OpenAIEmbeddingResponseData represents the embedding data from the OpenAI API
type OpenAIEmbeddingResponseData struct {
	Object    string    `json:"object"`
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

// OpenAIUsage represents the token usage information
type OpenAIUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// OpenAIEmbeddingService implements the EmbeddingService interface using OpenAI
type OpenAIEmbeddingService struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
	Logger     *zap.Logger
	BatchSize  int
	Dimension  int
}

// NewOpenAIEmbeddingService creates a new OpenAI embedding service
func NewOpenAIEmbeddingService(logger *zap.Logger, apiKey string) *OpenAIEmbeddingService {
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}

	return &OpenAIEmbeddingService{
		APIKey:     apiKey,
		Model:      DefaultOpenAIEmbeddingModel,
		BaseURL:    "https://api.openai.com/v1/embeddings",
		HTTPClient: &http.Client{Timeout: DefaultOpenAITimeout},
		Logger:     logger,
		BatchSize:  DefaultOpenAIBatchSize,
		Dimension:  DefaultOpenAIDimension,
	}
}

// Initialize implements the EmbeddingService interface
func (s *OpenAIEmbeddingService) Initialize(ctx context.Context) error {
	if s.APIKey == "" {
		return fmt.Errorf("OpenAI API key is required")
	}
	s.Logger.Info("Initialized OpenAI embedding service", zap.String("model", s.Model))
	return nil
}

// Close implements the EmbeddingService interface
func (s *OpenAIEmbeddingService) Close(ctx context.Context) error {
	return nil
}

// GetEmbedding implements the EmbeddingService interface
func (s *OpenAIEmbeddingService) GetEmbedding(ctx context.Context, text string) ([]float32, error) {
	embeddings, err := s.GetEmbeddings(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(embeddings) == 0 {
		return nil, fmt.Errorf("no embeddings returned")
	}
	return embeddings[0], nil
}

// GetEmbeddings implements the EmbeddingService interface
func (s *OpenAIEmbeddingService) GetEmbeddings(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}

	// Process in batches to avoid exceeding API limits
	var allEmbeddings [][]float32
	for i := 0; i < len(texts); i += s.BatchSize {
		end := i + s.BatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]

		// Clean the text inputs to prevent API errors
		for j := range batch {
			batch[j] = strings.TrimSpace(batch[j])
		}

		// Create the request
		reqBody := OpenAIEmbeddingRequest{
			Model: s.Model,
			Input: batch,
		}
		reqBytes, err := json.Marshal(reqBody)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request: %w", err)
		}

		// Create the HTTP request
		req, err := http.NewRequestWithContext(ctx, "POST", s.BaseURL, bytes.NewBuffer(reqBytes))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+s.APIKey)

		// Send the request
		resp, err := s.HTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to send request: %w", err)
		}
		defer resp.Body.Close()

		// Check for errors
		if resp.StatusCode != http.StatusOK {
			var errorResponse map[string]interface{}
			if err := json.NewDecoder(resp.Body).Decode(&errorResponse); err != nil {
				return nil, fmt.Errorf("failed to parse error response: %w", err)
			}
			return nil, fmt.Errorf("API error: %v", errorResponse)
		}

		// Parse the response
		var embeddingResp OpenAIEmbeddingResponse
		if err := json.NewDecoder(resp.Body).Decode(&embeddingResp); err != nil {
			return nil, fmt.Errorf("failed to parse response: %w", err)
		}

		// Extract the embeddings
		batchEmbeddings := make([][]float32, len(embeddingResp.Data))
		for j, data := range embeddingResp.Data {
			batchEmbeddings[data.Index] = data.Embedding
		}

		allEmbeddings = append(allEmbeddings, batchEmbeddings...)
		s.Logger.Debug("Generated embeddings batch",
			zap.Int("batch_size", len(batch)),
			zap.Int("tokens_used", embeddingResp.Usage.TotalTokens))
	}

	return allEmbeddings, nil
} 