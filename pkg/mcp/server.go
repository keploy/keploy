package mcp

import (
	"context"

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

	mcpSrv := server.NewMCPServer(
		"keploy-embed-converse",
		"1.0.0",
		server.WithToolCapabilities(true),
	)

	converseTool := mcp.NewTool(
		"converse",
		mcp.WithDescription("Run Keploy's embed.Converse pipeline on an input query"),
		mcp.WithString(
			"query",
			mcp.Required(),
			mcp.Description("User query text"),
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

	s.logger.Info("processing converse query", zap.String("query", query))

	out, err := s.captureConverseOutput(ctx, query)
	if err != nil {
		s.logger.Error("converse pipeline failed", zap.Error(err))
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultText(out), nil
}

func (s *Server) captureConverseOutput(ctx context.Context, query string) (string, error) {
	response, err := HandleConverseForMCP(ctx, s.embedService, query, s.logger)
	if err != nil {
		s.logger.Error("MCP server: New converse handler failed", zap.Error(err))
		return "", err
	}

	jsonResponse, err := ConverseResponseToJSON(response)
	if err != nil {
		s.logger.Error("MCP server: Failed to convert response to JSON", zap.Error(err))
		return "", err
	}

	s.logger.Info("MCP server: Successfully generated JSON response", zap.Int("response_length", len(jsonResponse)))
	return jsonResponse, nil
}
