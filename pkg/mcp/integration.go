// Package mcp provides integration between the MCP server and Keploy services.
package mcp

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// MockNamingConfig holds configuration for contextual mock naming
type MockNamingConfig struct {
	APIDescription   string
	ServiceContext   string
	EnableContextual bool
}

var (
	globalNamingConfig *MockNamingConfig
	namingConfigMu     sync.RWMutex
	globalNamer        *ContextualNamer
)

func init() {
	globalNamer = NewContextualNamer()
}

// SetNamingConfig sets the global naming configuration for mock recording
func SetNamingConfig(cfg *MockNamingConfig) {
	namingConfigMu.Lock()
	defer namingConfigMu.Unlock()
	globalNamingConfig = cfg
}

// GetNamingConfig returns the current naming configuration
func GetNamingConfig() *MockNamingConfig {
	namingConfigMu.RLock()
	defer namingConfigMu.RUnlock()
	return globalNamingConfig
}

// ClearNamingConfig clears the naming configuration
func ClearNamingConfig() {
	namingConfigMu.Lock()
	defer namingConfigMu.Unlock()
	globalNamingConfig = nil
}

// GenerateContextualMockName generates a contextual name for a mock based on its type and content
func GenerateContextualMockName(mock *models.Mock) string {
	cfg := GetNamingConfig()
	
	if cfg == nil || !cfg.EnableContextual {
		// Fall back to default sequential naming
		return ""
	}

	ctx := NamingContext{
		APIDescription: cfg.APIDescription,
		MockKind:       mock.Kind,
		Timestamp:      time.Now(),
	}

	// Extract additional context based on mock kind
	switch mock.Kind {
	case models.HTTP:
		if mock.Spec.HTTPReq != nil {
			ctx.HTTPMethod = string(mock.Spec.HTTPReq.Method)
			ctx.Endpoint = mock.Spec.HTTPReq.URL
		}
		if mock.Spec.Metadata != nil {
			if host, ok := mock.Spec.Metadata["host"]; ok {
				ctx.ServiceName = host
			}
		}
	case models.Postgres, models.MySQL:
		if mock.Spec.Metadata != nil {
			if op, ok := mock.Spec.Metadata["operation"]; ok {
				ctx.OperationType = op
			}
			if table, ok := mock.Spec.Metadata["table"]; ok {
				ctx.Endpoint = "/" + table
			}
		}
	case models.GENERIC:
		if mock.Spec.Metadata != nil {
			if svc, ok := mock.Spec.Metadata["service"]; ok {
				ctx.ServiceName = svc
			}
		}
	case models.Mongo:
		if mock.Spec.Metadata != nil {
			if col, ok := mock.Spec.Metadata["collection"]; ok {
				ctx.Endpoint = "/" + col
			}
			if op, ok := mock.Spec.Metadata["operation"]; ok {
				ctx.OperationType = op
			}
		}
	case models.REDIS:
		if mock.Spec.Metadata != nil {
			if cmd, ok := mock.Spec.Metadata["command"]; ok {
				ctx.OperationType = cmd
			}
		}
	}

	return globalNamer.GenerateName(ctx)
}

// MockRecordingSession represents an active mock recording session with contextual naming
type MockRecordingSession struct {
	ID              string
	TestSetID       string
	APIDescription  string
	StartTime       time.Time
	MockCount       int
	MocksByKind     map[models.Kind]int
	ContextualNames map[string]string // map of original name to contextual name
	mu              sync.Mutex
}

// NewMockRecordingSession creates a new mock recording session
func NewMockRecordingSession(testSetID, apiDescription string) *MockRecordingSession {
	return &MockRecordingSession{
		ID:              fmt.Sprintf("session-%d", time.Now().UnixNano()),
		TestSetID:       testSetID,
		APIDescription:  apiDescription,
		StartTime:       time.Now(),
		MocksByKind:     make(map[models.Kind]int),
		ContextualNames: make(map[string]string),
	}
}

// AddMock registers a new mock with the session
func (s *MockRecordingSession) AddMock(mock *models.Mock) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.MockCount++
	s.MocksByKind[mock.Kind]++

	// Generate contextual name if enabled
	cfg := GetNamingConfig()
	if cfg != nil && cfg.EnableContextual {
		contextualName := GenerateContextualMockName(mock)
		if contextualName != "" {
			s.ContextualNames[mock.Name] = contextualName
			return contextualName
		}
	}

	return mock.Name
}

// GetStats returns statistics about the recording session
func (s *MockRecordingSession) GetStats() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	return map[string]interface{}{
		"id":              s.ID,
		"testSetId":       s.TestSetID,
		"apiDescription":  s.APIDescription,
		"duration":        time.Since(s.StartTime).String(),
		"totalMocks":      s.MockCount,
		"mocksByKind":     s.MocksByKind,
		"contextualNames": len(s.ContextualNames),
	}
}

// ServiceIntegration provides integration between MCP and Keploy services
type ServiceIntegration struct {
	logger       *zap.Logger
	config       *config.Config
	mcpServer    *Server
	orchestrator *WorkflowOrchestrator
	sessions     map[string]*MockRecordingSession
	sessionsMu   sync.RWMutex
}

// NewServiceIntegration creates a new service integration instance
func NewServiceIntegration(logger *zap.Logger, cfg *config.Config) *ServiceIntegration {
	return &ServiceIntegration{
		logger:       logger,
		config:       cfg,
		mcpServer:    NewServer(logger, cfg),
		orchestrator: NewWorkflowOrchestrator(logger, cfg),
		sessions:     make(map[string]*MockRecordingSession),
	}
}

// GetMCPServer returns the MCP server instance
func (si *ServiceIntegration) GetMCPServer() *Server {
	return si.mcpServer
}

// GetOrchestrator returns the workflow orchestrator
func (si *ServiceIntegration) GetOrchestrator() *WorkflowOrchestrator {
	return si.orchestrator
}

// StartRecordingSession starts a new recording session with contextual naming
func (si *ServiceIntegration) StartRecordingSession(testSetID, apiDescription string) *MockRecordingSession {
	session := NewMockRecordingSession(testSetID, apiDescription)

	si.sessionsMu.Lock()
	si.sessions[session.ID] = session
	si.sessionsMu.Unlock()

	// Configure global naming
	SetNamingConfig(&MockNamingConfig{
		APIDescription:   apiDescription,
		EnableContextual: true,
	})

	si.logger.Info("Started mock recording session",
		zap.String("sessionID", session.ID),
		zap.String("testSetID", testSetID),
		zap.String("apiDescription", apiDescription),
	)

	return session
}

// EndRecordingSession ends a recording session and clears naming config
func (si *ServiceIntegration) EndRecordingSession(sessionID string) *MockRecordingSession {
	si.sessionsMu.Lock()
	session, exists := si.sessions[sessionID]
	if exists {
		delete(si.sessions, sessionID)
	}
	si.sessionsMu.Unlock()

	ClearNamingConfig()

	if session != nil {
		si.logger.Info("Ended mock recording session",
			zap.String("sessionID", session.ID),
			zap.Int("totalMocks", session.MockCount),
		)
	}

	return session
}

// GetSession returns a session by ID
func (si *ServiceIntegration) GetSession(sessionID string) *MockRecordingSession {
	si.sessionsMu.RLock()
	defer si.sessionsMu.RUnlock()
	return si.sessions[sessionID]
}

// ExecuteRecordWorkflow executes the mock recording workflow
func (si *ServiceIntegration) ExecuteRecordWorkflow(ctx context.Context, testCommand, apiDescription string) (*RecordingStats, []MockFileInfo, error) {
	return si.orchestrator.RecordOnly(ctx, testCommand, apiDescription)
}

// ExecuteReplayWorkflow executes the mock replay workflow
func (si *ServiceIntegration) ExecuteReplayWorkflow(ctx context.Context, testCommand, testSetID string) (*ReplayStats, error) {
	return si.orchestrator.ReplayOnly(ctx, testCommand, testSetID)
}

// ExecuteFullWorkflow executes the complete record and replay workflow
func (si *ServiceIntegration) ExecuteFullWorkflow(ctx context.Context, testCommand, apiDescription string, autoReplay bool) (*WorkflowResult, error) {
	return si.orchestrator.ExecuteFullWorkflow(ctx, testCommand, apiDescription, autoReplay)
}
