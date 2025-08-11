package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.keploy.io/server/v2/pkg/service/embed"
	"go.uber.org/zap"
)

type ConverseResponse struct {
	Question      string               `json:"question"`
	SearchResults []embed.SearchResult `json:"search_results"`
	TotalResults  int                  `json:"total_results"`
	Context       string               `json:"context"`
	Success       bool                 `json:"success"`
	Error         string               `json:"error,omitempty"`
}

func HandleConverseForMCP(ctx context.Context, embedService embed.Service, query string, logger *zap.Logger) (*ConverseResponse, error) {
	logger.Info("MCP converse handler called - starting search without OpenAI API call", zap.String("query", query))

	response := &ConverseResponse{
		Question: query,
		Success:  false,
	}

	// 1. Generate an embedding for the user's query
	logger.Info("Generating embedding for query", zap.String("query", query))
	queryEmbeddings, err := embedService.GenerateEmbeddingsForQ(ctx, []string{query})
	if err != nil {
		response.Error = fmt.Sprintf("failed to generate embedding for query: %v", err)
		return response, fmt.Errorf("failed to generate embedding for query: %w", err)
	}
	if len(queryEmbeddings) == 0 {
		response.Error = "received no embedding for the query"
		return response, fmt.Errorf("received no embedding for the query")
	}
	queryEmbedding := queryEmbeddings[0]

	// 2. Find similar code chunks from vector DB
	logger.Info("Searching for similar code chunks in the database")
	searchResults, err := embedService.SearchSimilarCode(ctx, queryEmbedding, 10)
	if err != nil {
		response.Error = fmt.Sprintf("failed to search for similar code: %v", err)
		return response, fmt.Errorf("failed to search for similar code: %w", err)
	}

	// 3. Build context from vector search results
	var contextBuilder strings.Builder

	if len(searchResults) == 0 {
		logger.Warn("No relevant code snippets or symbols found for the query.")
		response.Error = "No relevant code snippets found for the query. Please try rephrasing or be more specific."
		return response, nil
	}

	for _, res := range searchResults {
		contextBuilder.WriteString(fmt.Sprintf("--- Code Snippet from file: %s ---\n", res.FilePath))
		contextBuilder.WriteString(res.Content)
		contextBuilder.WriteString("\n---\n\n")
	}

	// 4. Prepare the response
	response.SearchResults = searchResults
	response.TotalResults = len(searchResults)
	response.Context = contextBuilder.String()
	response.Success = true

	logger.Info("MCP converse handler completed successfully",
		zap.Int("total_results", response.TotalResults),
		zap.String("query", query),
		zap.Bool("success", response.Success))

	return response, nil
}

// ConverseResponseToJSON converts the response to JSON string
func ConverseResponseToJSON(response *ConverseResponse) (string, error) {
	jsonBytes, err := json.MarshalIndent(response, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal response to JSON: %w", err)
	}
	return string(jsonBytes), nil
}
