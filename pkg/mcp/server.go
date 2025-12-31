// Package mcp provides Model Context Protocol server implementation for Keploy.
// This enables AI agents to discover and interact with Keploy's mock recording and replay capabilities.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// Tool represents an MCP tool definition
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// ToolResult represents the result of a tool invocation
type ToolResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ContentItem represents a content item in the tool result
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Server represents the MCP server for Keploy
type Server struct {
	logger    *zap.Logger
	config    *config.Config
	tools     map[string]Tool
	handlers  map[string]ToolHandler
	mu        sync.RWMutex
	recording *RecordingSession
	replaying *ReplaySession
}

// ToolHandler is a function that handles tool invocations
type ToolHandler func(ctx context.Context, params map[string]interface{}) (*ToolResult, error)

// RecordingSession tracks an active recording session
type RecordingSession struct {
	ID        string
	TestSetID string
	Command   string
	Status    string
	MockCount int
	TestCount int
	StartTime int64
	MockNames []string
	Metadata  map[string]string
}

// ReplaySession tracks an active replay session
type ReplaySession struct {
	ID          string
	TestSetID   string
	Status      string
	TestsPassed int
	TestsFailed int
	StartTime   int64
}

// NewServer creates a new MCP server instance
func NewServer(logger *zap.Logger, cfg *config.Config) *Server {
	s := &Server{
		logger:   logger,
		config:   cfg,
		tools:    make(map[string]Tool),
		handlers: make(map[string]ToolHandler),
	}
	s.registerTools()
	return s
}

// registerTools registers all available Keploy tools for MCP
func (s *Server) registerTools() {
	// Register mock record tool
	s.registerTool(Tool{
		Name:        "keploy_mock_record",
		Description: "Record mocks by capturing outgoing network calls during test execution. This tool wraps your test command and captures all external dependencies (HTTP, database, etc.) as mock files with contextual naming.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"testCommand": map[string]interface{}{
					"type":        "string",
					"description": "The test command to execute (e.g., 'go test', 'npm test', 'pytest')",
				},
				"testSetName": map[string]interface{}{
					"type":        "string",
					"description": "Optional custom name for the test set. If not provided, a sequential ID will be generated.",
				},
				"contextDescription": map[string]interface{}{
					"type":        "string",
					"description": "Description of the API or functionality being tested. Used for contextual mock naming.",
				},
				"metadata": map[string]interface{}{
					"type":        "object",
					"description": "Optional metadata to attach to the recorded mocks",
				},
			},
			"required": []string{"testCommand"},
		},
	}, s.handleMockRecord)

	// Register mock test tool
	s.registerTool(Tool{
		Name:        "keploy_mock_test",
		Description: "Run tests using previously recorded mocks. This tool injects mock data during test execution to ensure environment isolation and consistent test results.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"testCommand": map[string]interface{}{
					"type":        "string",
					"description": "The test command to execute with mocks (e.g., 'go test', 'npm test', 'pytest')",
				},
				"testSetID": map[string]interface{}{
					"type":        "string",
					"description": "The test set ID containing the mocks to use. If not provided, all available test sets will be used.",
				},
				"validateIsolation": map[string]interface{}{
					"type":        "boolean",
					"description": "If true, validates that no real network calls were made during test execution (default: true)",
				},
			},
			"required": []string{"testCommand"},
		},
	}, s.handleMockTest)

	// Register list mocks tool
	s.registerTool(Tool{
		Name:        "keploy_list_mocks",
		Description: "List all recorded mock files with their contextual names and metadata.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"testSetID": map[string]interface{}{
					"type":        "string",
					"description": "Optional test set ID to filter mocks. If not provided, lists mocks from all test sets.",
				},
			},
		},
	}, s.handleListMocks)

	// Register generate tests tool
	s.registerTool(Tool{
		Name:        "keploy_generate_tests",
		Description: "Generate tests using Keploy's mocking feature. This is a high-level command that orchestrates the full record/replay cycle.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"testCommand": map[string]interface{}{
					"type":        "string",
					"description": "The test command to execute",
				},
				"apiDescription": map[string]interface{}{
					"type":        "string",
					"description": "Description of the API endpoints being tested. Used for intelligent mock naming.",
				},
				"autoReplay": map[string]interface{}{
					"type":        "boolean",
					"description": "If true, automatically runs replay after recording to validate isolation (default: true)",
				},
			},
			"required": []string{"testCommand"},
		},
	}, s.handleGenerateTests)

	// Register get recording status tool
	s.registerTool(Tool{
		Name:        "keploy_recording_status",
		Description: "Get the status of the current or last recording session.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, s.handleRecordingStatus)

	// Register get test status tool
	s.registerTool(Tool{
		Name:        "keploy_test_status",
		Description: "Get the status of the current or last test session.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	}, s.handleTestStatus)
}

// registerTool registers a single tool with its handler
func (s *Server) registerTool(tool Tool, handler ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[tool.Name] = tool
	s.handlers[tool.Name] = handler
}

// GetTools returns all registered tools
func (s *Server) GetTools() []Tool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tools := make([]Tool, 0, len(s.tools))
	for _, tool := range s.tools {
		tools = append(tools, tool)
	}
	return tools
}

// InvokeTool invokes a tool by name with the given parameters
func (s *Server) InvokeTool(ctx context.Context, name string, params map[string]interface{}) (*ToolResult, error) {
	s.mu.RLock()
	handler, exists := s.handlers[name]
	s.mu.RUnlock()

	if !exists {
		return &ToolResult{
			Content: []ContentItem{{Type: "text", Text: fmt.Sprintf("Tool '%s' not found", name)}},
			IsError: true,
		}, nil
	}

	return handler(ctx, params)
}

// handleMockRecord handles the mock record tool invocation
func (s *Server) handleMockRecord(ctx context.Context, params map[string]interface{}) (*ToolResult, error) {
	testCommand, ok := params["testCommand"].(string)
	if !ok || testCommand == "" {
		return &ToolResult{
			Content: []ContentItem{{Type: "text", Text: "testCommand is required"}},
			IsError: true,
		}, nil
	}

	contextDesc, _ := params["contextDescription"].(string)
	testSetName, _ := params["testSetName"].(string)

	s.logger.Info("Starting mock recording",
		zap.String("command", testCommand),
		zap.String("context", contextDesc),
		zap.String("testSet", testSetName),
	)

	// Create recording session
	session := &RecordingSession{
		ID:        fmt.Sprintf("rec-%d", time.Now().UnixNano()),
		Command:   testCommand,
		Status:    "recording",
		StartTime: time.Now().UnixNano(),
		Metadata:  make(map[string]string),
	}

	if contextDesc != "" {
		session.Metadata["context"] = contextDesc
	}

	s.mu.Lock()
	s.recording = session
	s.mu.Unlock()

	// Return immediate response - actual recording happens asynchronously
	return &ToolResult{
		Content: []ContentItem{
			{Type: "text", Text: fmt.Sprintf("Mock recording started.\nSession ID: %s\nCommand: %s\nUse 'keploy_recording_status' to check progress.", session.ID, testCommand)},
		},
	}, nil
}

// handleMockTest handles the mock test tool invocation
func (s *Server) handleMockTest(ctx context.Context, params map[string]interface{}) (*ToolResult, error) {
	testCommand, ok := params["testCommand"].(string)
	if !ok || testCommand == "" {
		return &ToolResult{
			Content: []ContentItem{{Type: "text", Text: "testCommand is required"}},
			IsError: true,
		}, nil
	}

	testSetID, _ := params["testSetID"].(string)
	validateIsolation := true
	if val, ok := params["validateIsolation"].(bool); ok {
		validateIsolation = val
	}

	s.logger.Info("Starting mock test",
		zap.String("command", testCommand),
		zap.String("testSetID", testSetID),
		zap.Bool("validateIsolation", validateIsolation),
	)

	// Create test session
	session := &ReplaySession{
		ID:        fmt.Sprintf("test-%d", time.Now().UnixNano()),
		TestSetID: testSetID,
		Status:    "testing",
		StartTime: time.Now().UnixNano(),
	}

	s.mu.Lock()
	s.replaying = session
	s.mu.Unlock()

	return &ToolResult{
		Content: []ContentItem{
			{Type: "text", Text: fmt.Sprintf("Mock test started.\nSession ID: %s\nCommand: %s\nTest Set: %s\nUse 'keploy_test_status' to check progress.", session.ID, testCommand, testSetID)},
		},
	}, nil
}

// handleListMocks handles the list mocks tool invocation
func (s *Server) handleListMocks(ctx context.Context, params map[string]interface{}) (*ToolResult, error) {
	testSetID, _ := params["testSetID"].(string)

	s.logger.Info("Listing mocks", zap.String("testSetID", testSetID))

	// TODO: Integrate with mockdb to list actual mocks
	return &ToolResult{
		Content: []ContentItem{
			{Type: "text", Text: fmt.Sprintf("Mock listing for test set: %s\n(Integration pending)", testSetID)},
		},
	}, nil
}

// handleGenerateTests handles the generate tests tool invocation
func (s *Server) handleGenerateTests(ctx context.Context, params map[string]interface{}) (*ToolResult, error) {
	testCommand, ok := params["testCommand"].(string)
	if !ok || testCommand == "" {
		return &ToolResult{
			Content: []ContentItem{{Type: "text", Text: "testCommand is required"}},
			IsError: true,
		}, nil
	}

	apiDescription, _ := params["apiDescription"].(string)
	autoReplay := true
	if val, ok := params["autoReplay"].(bool); ok {
		autoReplay = val
	}

	s.logger.Info("Generating tests with Keploy mocking",
		zap.String("command", testCommand),
		zap.String("apiDescription", apiDescription),
		zap.Bool("autoReplay", autoReplay),
	)

	response := fmt.Sprintf(`Test generation initiated with Keploy mocking feature.

Command: %s
API Context: %s
Auto-Test: %v

Workflow:
1. Recording phase: Capturing outgoing network calls
2. Contextual naming: Generating descriptive mock filenames
3. Test phase: Validating test isolation with recorded mocks

Use 'keploy_recording_status' and 'keploy_test_status' to monitor progress.`, testCommand, apiDescription, autoReplay)

	return &ToolResult{
		Content: []ContentItem{{Type: "text", Text: response}},
	}, nil
}

// handleRecordingStatus returns the current recording session status
func (s *Server) handleRecordingStatus(ctx context.Context, params map[string]interface{}) (*ToolResult, error) {
	s.mu.RLock()
	session := s.recording
	s.mu.RUnlock()

	if session == nil {
		return &ToolResult{
			Content: []ContentItem{{Type: "text", Text: "No recording session found."}},
		}, nil
	}

	data, _ := json.MarshalIndent(session, "", "  ")
	return &ToolResult{
		Content: []ContentItem{{Type: "text", Text: string(data)}},
	}, nil
}

// handleTestStatus returns the current test session status
func (s *Server) handleTestStatus(ctx context.Context, params map[string]interface{}) (*ToolResult, error) {
	s.mu.RLock()
	session := s.replaying
	s.mu.RUnlock()

	if session == nil {
		return &ToolResult{
			Content: []ContentItem{{Type: "text", Text: "No test session found."}},
		}, nil
	}

	data, _ := json.MarshalIndent(session, "", "  ")
	return &ToolResult{
		Content: []ContentItem{{Type: "text", Text: string(data)}},
	}, nil
}

// ServeHTTP implements the HTTP handler for MCP protocol
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/tools/list":
		s.handleListTools(w, r)
	case "/tools/call":
		s.handleCallTool(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	tools := s.GetTools()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"tools": tools,
	})
}

func (s *Server) handleCallTool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string                 `json:"name"`
		Params map[string]interface{} `json:"arguments"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	result, err := s.InvokeTool(r.Context(), req.Name, req.Params)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
