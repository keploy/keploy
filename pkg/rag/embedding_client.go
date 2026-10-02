package rag

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

func GetEmbeddingHTTP(serviceURL, text string) ([]float64, error) {
	reqBody := map[string]string{"text": text}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := httpClient.Post(
		serviceURL+"/embed_text",
		"application/json",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding service returned status %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	embedding, ok := result["embedding"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid embedding format in response")
	}

	float64Embedding := make([]float64, len(embedding))
	for i, v := range embedding {
		if f, ok := v.(float64); ok {
			float64Embedding[i] = f
		} else {
			return nil, fmt.Errorf("invalid embedding value at index %d", i)
		}
	}

	return float64Embedding, nil
}

func GetBatchEmbeddingHTTP(serviceURL string, texts []string) ([][]float64, error) {
	reqBody := map[string][]string{"texts": texts}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	resp, err := httpClient.Post(
		serviceURL+"/batch_embed",
		"application/json",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	embeddingsRaw, ok := result["embeddings"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid embeddings format in response")
	}

	embeddings := make([][]float64, len(embeddingsRaw))
	for i, embeddingRaw := range embeddingsRaw {
		embedding, ok := embeddingRaw.([]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid embedding format at index %d", i)
		}

		float64Embedding := make([]float64, len(embedding))
		for j, v := range embedding {
			if f, ok := v.(float64); ok {
				float64Embedding[j] = f
			} else {
				return nil, fmt.Errorf("invalid embedding value at [%d][%d]", i, j)
			}
		}
		embeddings[i] = float64Embedding
	}

	return embeddings, nil
}
