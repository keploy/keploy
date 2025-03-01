package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type EmbeddingService struct {
	logger    *zap.Logger
	openaiKey string
	modelName string
	dimension int
	maxTokens int
}

func NewEmbeddingService(logger *zap.Logger, openaiKey string) *EmbeddingService {
	return &EmbeddingService{
		logger:    logger,
		openaiKey: openaiKey,
		modelName: "text-embedding-ada-002", // Default model
		dimension: 1536,                     // Dimension for ada-002
		maxTokens: 8191,                     // Maximum tokens for ada-002
	}
}

func (e *EmbeddingService) GenerateEmbedding(ctx context.Context, codeSnippet string) ([]float32, error) {
	// Prepare the request to OpenAI API
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": e.modelName,
		"input": codeSnippet,
	})
	if err != nil {
		utils.LogError(e.logger, err, "failed to marshal embedding request")
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/embeddings", bytes.NewBuffer(reqBody))
	if err != nil {
		utils.LogError(e.logger, err, "failed to create embedding request")
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", e.openaiKey))

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		utils.LogError(e.logger, err, "failed to send embedding request")
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		utils.LogError(e.logger, err, "failed to read embedding response")
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		utils.LogError(e.logger, nil, fmt.Sprintf("embedding API returned status code %d: %s", resp.StatusCode, string(body)))
		return nil, fmt.Errorf("embedding API returned status code %d", resp.StatusCode)
	}

	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	err = json.Unmarshal(body, &result)
	if err != nil {
		utils.LogError(e.logger, err, "failed to unmarshal embedding response")
		return nil, err
	}

	if len(result.Data) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return result.Data[0].Embedding, nil
}

func (e *EmbeddingService) GetDimension() int {
	return e.dimension
}
