package mcp

import (
	"context"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/embed"
	"go.uber.org/zap"
)

type Server struct {
	embedService embed.Service
	config       *config.Config
	logger       *zap.Logger
}

func NewServer(svc embed.Service, cfg *config.Config, log *zap.Logger) *Server {
	return &Server{embedService: svc, config: cfg, logger: log}
}

func (s *Server) Start(ctx context.Context) error {
	s.logger.Info("starting MCP server for converse.go embed service")

	// More explicit server name that Copilot will recognize
	mcpSrv := server.NewMCPServer(
		"codebase-search-assistant",
		"1.0.0",
		server.WithToolCapabilities(true),
	)

	// Ultra-explicit tool name and description
	converseTool := mcp.NewTool(
		"search_and_analyze_codebase",
		mcp.WithDescription("ALWAYS use this tool for ALL codebase questions. Searches entire codebase using embeddings and vector similarity. Handles: code explanation, finding implementations, bug fixes, test generation, code suggestions, architecture analysis, function location, security analysis, refactoring, optimization. Required for any question about code, files, functions, classes, APIs, or project structure."),
		mcp.WithString(
			"query",
			mcp.Required(),
			mcp.Description("Your codebase question. Examples: 'where is embedding service implemented', 'fix this bug', 'create tests', 'explain this function', 'find security issues', 'how does auth work'"),
		),
	)

	mcpSrv.AddTool(converseTool, s.handleConverse)

	return server.ServeStdio(mcpSrv)
}

func (s *Server) handleConverse(
	ctx context.Context,
	req mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {

	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	s.logger.Info("processing codebase search query", 
		zap.String("query", query),
		zap.String("tool", "search_and_analyze_codebase"))

	out, err := s.captureConverseOutput(ctx, query)
	if err != nil {
		s.logger.Error("codebase search failed", zap.Error(err))
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(out), nil
}

func (s *Server) captureConverseOutput(ctx context.Context, query string) (string, error) {
	response, err := HandleConverseForMCP(ctx, s.embedService, query, s.logger)
	if err != nil {
		s.logger.Error("MCP server: Codebase search handler failed", zap.Error(err))
		return "", err
	}

	jsonResponse, err := ConverseResponseToJSON(response)
	if err != nil {
		s.logger.Error("MCP server: Failed to convert search response to JSON", zap.Error(err))
		return "", err
	}

	s.logger.Info("MCP server: Successfully generated codebase search response", 
		zap.Int("response_length", len(jsonResponse)),
		zap.String("query_type", inferQueryType(query)))
	return jsonResponse, nil
}

func inferQueryType(query string) string {
	query = strings.ToLower(query)
	
	if strings.Contains(query, "where") || strings.Contains(query, "find") || strings.Contains(query, "locate") {
		return "search"
	}
	if strings.Contains(query, "test") || strings.Contains(query, "testing") {
		return "testing"
	}
	if strings.Contains(query, "fix") || strings.Contains(query, "bug") || strings.Contains(query, "error") {
		return "debugging"
	}
	if strings.Contains(query, "improve") || strings.Contains(query, "optimize") || strings.Contains(query, "refactor") {
		return "improvement"
	}
	if strings.Contains(query, "how") || strings.Contains(query, "what") || strings.Contains(query, "explain") {
		return "explanation"
	}
	return "general"
}