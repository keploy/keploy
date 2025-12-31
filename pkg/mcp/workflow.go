// Package mcp provides workflow orchestration for mock recording and replay.
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

// WorkflowPhase represents the current phase of the workflow
type WorkflowPhase string

const (
	PhaseIdle       WorkflowPhase = "idle"
	PhaseRecording  WorkflowPhase = "recording"
	PhaseProcessing WorkflowPhase = "processing"
	PhaseReplaying  WorkflowPhase = "replaying"
	PhaseCompleted  WorkflowPhase = "completed"
	PhaseFailed     WorkflowPhase = "failed"
)

// WorkflowResult contains the final result of a workflow execution
type WorkflowResult struct {
	Success        bool                `json:"success"`
	Phase          WorkflowPhase       `json:"phase"`
	TestSetID      string              `json:"testSetId"`
	RecordStats    *RecordingStats     `json:"recordStats,omitempty"`
	ReplayStats    *ReplayStats        `json:"replayStats,omitempty"`
	MockFiles      []MockFileInfo      `json:"mockFiles,omitempty"`
	Errors         []string            `json:"errors,omitempty"`
	Duration       time.Duration       `json:"duration"`
	IsolationValid bool                `json:"isolationValid"`
}

// RecordingStats contains statistics from the recording phase
type RecordingStats struct {
	TotalMocks      int               `json:"totalMocks"`
	MocksByKind     map[string]int    `json:"mocksByKind"`
	TotalTestCases  int               `json:"totalTestCases"`
	Duration        time.Duration     `json:"duration"`
	NetworkCalls    int               `json:"networkCalls"`
	ExternalServices []string         `json:"externalServices"`
}

// ReplayStats contains statistics from the replay phase
type ReplayStats struct {
	TotalTests    int           `json:"totalTests"`
	Passed        int           `json:"passed"`
	Failed        int           `json:"failed"`
	Skipped       int           `json:"skipped"`
	Duration      time.Duration `json:"duration"`
	MocksUsed     int           `json:"mocksUsed"`
	MocksMissed   int           `json:"mocksMissed"`
	RealCallsMade int           `json:"realCallsMade"`
}

// MockFileInfo contains information about a generated mock file
type MockFileInfo struct {
	Name         string      `json:"name"`
	ContextName  string      `json:"contextName"`
	Kind         models.Kind `json:"kind"`
	Path         string      `json:"path"`
	ServiceName  string      `json:"serviceName,omitempty"`
	Endpoint     string      `json:"endpoint,omitempty"`
	Description  string      `json:"description,omitempty"`
}

// WorkflowOrchestrator coordinates the record/replay workflow
type WorkflowOrchestrator struct {
	logger      *zap.Logger
	config      *config.Config
	namer       *ContextualNamer
	mu          sync.RWMutex
	currentPhase WorkflowPhase
	currentWorkflow *WorkflowExecution
}

// WorkflowExecution represents an active workflow execution
type WorkflowExecution struct {
	ID            string
	TestCommand   string
	APIDescription string
	TestSetID     string
	Phase         WorkflowPhase
	StartTime     time.Time
	RecordStats   *RecordingStats
	ReplayStats   *ReplayStats
	MockFiles     []MockFileInfo
	Errors        []string
	CancelFunc    context.CancelFunc
}

// NewWorkflowOrchestrator creates a new workflow orchestrator
func NewWorkflowOrchestrator(logger *zap.Logger, cfg *config.Config) *WorkflowOrchestrator {
	return &WorkflowOrchestrator{
		logger:       logger,
		config:       cfg,
		namer:        NewContextualNamer(),
		currentPhase: PhaseIdle,
	}
}

// ExecuteFullWorkflow executes the complete record and replay workflow
func (wo *WorkflowOrchestrator) ExecuteFullWorkflow(ctx context.Context, testCommand, apiDescription string, autoReplay bool) (*WorkflowResult, error) {
	wo.mu.Lock()
	if wo.currentPhase != PhaseIdle && wo.currentPhase != PhaseCompleted && wo.currentPhase != PhaseFailed {
		wo.mu.Unlock()
		return nil, fmt.Errorf("workflow already in progress: %s", wo.currentPhase)
	}

	workflowCtx, cancel := context.WithCancel(ctx)
	execution := &WorkflowExecution{
		ID:             fmt.Sprintf("workflow-%d", time.Now().UnixNano()),
		TestCommand:    testCommand,
		APIDescription: apiDescription,
		Phase:          PhaseRecording,
		StartTime:      time.Now(),
		CancelFunc:     cancel,
	}
	wo.currentWorkflow = execution
	wo.currentPhase = PhaseRecording
	wo.mu.Unlock()

	result := &WorkflowResult{
		Phase: PhaseRecording,
	}

	defer func() {
		cancel()
		wo.mu.Lock()
		wo.currentPhase = result.Phase
		wo.mu.Unlock()
	}()

	wo.logger.Info("Starting full mock workflow",
		zap.String("workflowID", execution.ID),
		zap.String("command", testCommand),
		zap.String("apiDescription", apiDescription),
	)

	// Phase 1: Recording
	recordStats, mockFiles, err := wo.executeRecordPhase(workflowCtx, execution)
	if err != nil {
		result.Phase = PhaseFailed
		result.Errors = append(result.Errors, fmt.Sprintf("Recording failed: %v", err))
		return result, err
	}

	result.RecordStats = recordStats
	result.MockFiles = mockFiles
	result.TestSetID = execution.TestSetID
	execution.RecordStats = recordStats
	execution.MockFiles = mockFiles

	wo.logger.Info("Recording phase completed",
		zap.Int("mocksRecorded", recordStats.TotalMocks),
		zap.Int("testCases", recordStats.TotalTestCases),
	)

	// Phase 2: Processing (apply contextual naming)
	wo.mu.Lock()
	wo.currentPhase = PhaseProcessing
	execution.Phase = PhaseProcessing
	wo.mu.Unlock()

	result.Phase = PhaseProcessing

	namedMocks, err := wo.applyContextualNaming(workflowCtx, mockFiles, apiDescription)
	if err != nil {
		wo.logger.Warn("Contextual naming partially failed", zap.Error(err))
		// Continue with original names
	} else {
		result.MockFiles = namedMocks
	}

	// Phase 3: Replay (if autoReplay is enabled)
	if autoReplay {
		wo.mu.Lock()
		wo.currentPhase = PhaseReplaying
		execution.Phase = PhaseReplaying
		wo.mu.Unlock()

		result.Phase = PhaseReplaying

		replayStats, err := wo.executeReplayPhase(workflowCtx, execution)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("Replay failed: %v", err))
			// Don't fail the entire workflow for replay issues
		} else {
			result.ReplayStats = replayStats
			execution.ReplayStats = replayStats
			result.IsolationValid = replayStats.RealCallsMade == 0
		}

		wo.logger.Info("Replay phase completed",
			zap.Int("passed", replayStats.Passed),
			zap.Int("failed", replayStats.Failed),
			zap.Bool("isolationValid", result.IsolationValid),
		)
	}

	// Finalize
	result.Phase = PhaseCompleted
	result.Success = len(result.Errors) == 0
	result.Duration = time.Since(execution.StartTime)

	wo.logger.Info("Workflow completed",
		zap.String("workflowID", execution.ID),
		zap.Bool("success", result.Success),
		zap.Duration("duration", result.Duration),
	)

	return result, nil
}

// executeRecordPhase handles the mock recording phase
func (wo *WorkflowOrchestrator) executeRecordPhase(ctx context.Context, execution *WorkflowExecution) (*RecordingStats, []MockFileInfo, error) {
	startTime := time.Now()

	// Generate test set name using contextual namer
	testSetID := wo.namer.GenerateTestSetName(execution.APIDescription, time.Now())
	execution.TestSetID = testSetID

	wo.logger.Info("Starting recording phase",
		zap.String("testSetID", testSetID),
		zap.String("command", execution.TestCommand),
	)

	// The actual recording is delegated to the record service
	// This is a placeholder for the integration point
	stats := &RecordingStats{
		TotalMocks:       0,
		MocksByKind:      make(map[string]int),
		TotalTestCases:   0,
		Duration:         time.Since(startTime),
		NetworkCalls:     0,
		ExternalServices: []string{},
	}

	mockFiles := []MockFileInfo{}

	return stats, mockFiles, nil
}

// executeReplayPhase handles the mock replay phase
func (wo *WorkflowOrchestrator) executeReplayPhase(ctx context.Context, execution *WorkflowExecution) (*ReplayStats, error) {
	startTime := time.Now()

	wo.logger.Info("Starting replay phase",
		zap.String("testSetID", execution.TestSetID),
		zap.String("command", execution.TestCommand),
	)

	// The actual replay is delegated to the replay service
	// This is a placeholder for the integration point
	stats := &ReplayStats{
		TotalTests:    0,
		Passed:        0,
		Failed:        0,
		Skipped:       0,
		Duration:      time.Since(startTime),
		MocksUsed:     0,
		MocksMissed:   0,
		RealCallsMade: 0,
	}

	return stats, nil
}

// applyContextualNaming applies contextual names to mock files
func (wo *WorkflowOrchestrator) applyContextualNaming(ctx context.Context, mocks []MockFileInfo, apiDescription string) ([]MockFileInfo, error) {
	namedMocks := make([]MockFileInfo, len(mocks))

	for i, mock := range mocks {
		namedMock := mock

		// Generate contextual name based on mock kind and available info
		namingCtx := NamingContext{
			APIDescription: apiDescription,
			MockKind:       mock.Kind,
			ServiceName:    mock.ServiceName,
			Endpoint:       mock.Endpoint,
			Timestamp:      time.Now(),
		}

		namedMock.ContextName = wo.namer.GenerateName(namingCtx)
		namedMocks[i] = namedMock
	}

	return namedMocks, nil
}

// GetCurrentPhase returns the current workflow phase
func (wo *WorkflowOrchestrator) GetCurrentPhase() WorkflowPhase {
	wo.mu.RLock()
	defer wo.mu.RUnlock()
	return wo.currentPhase
}

// GetCurrentWorkflow returns information about the current workflow execution
func (wo *WorkflowOrchestrator) GetCurrentWorkflow() *WorkflowExecution {
	wo.mu.RLock()
	defer wo.mu.RUnlock()
	return wo.currentWorkflow
}

// CancelCurrentWorkflow cancels any running workflow
func (wo *WorkflowOrchestrator) CancelCurrentWorkflow() error {
	wo.mu.Lock()
	defer wo.mu.Unlock()

	if wo.currentWorkflow == nil {
		return fmt.Errorf("no workflow in progress")
	}

	if wo.currentWorkflow.CancelFunc != nil {
		wo.currentWorkflow.CancelFunc()
	}

	wo.currentPhase = PhaseFailed
	return nil
}

// RecordOnly executes only the recording phase
func (wo *WorkflowOrchestrator) RecordOnly(ctx context.Context, testCommand, apiDescription string) (*RecordingStats, []MockFileInfo, error) {
	wo.mu.Lock()
	if wo.currentPhase != PhaseIdle && wo.currentPhase != PhaseCompleted && wo.currentPhase != PhaseFailed {
		wo.mu.Unlock()
		return nil, nil, fmt.Errorf("workflow already in progress: %s", wo.currentPhase)
	}

	workflowCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	execution := &WorkflowExecution{
		ID:             fmt.Sprintf("record-%d", time.Now().UnixNano()),
		TestCommand:    testCommand,
		APIDescription: apiDescription,
		Phase:          PhaseRecording,
		StartTime:      time.Now(),
		CancelFunc:     cancel,
	}
	wo.currentWorkflow = execution
	wo.currentPhase = PhaseRecording
	wo.mu.Unlock()

	defer func() {
		wo.mu.Lock()
		wo.currentPhase = PhaseCompleted
		wo.mu.Unlock()
	}()

	return wo.executeRecordPhase(workflowCtx, execution)
}

// ReplayOnly executes only the replay phase
func (wo *WorkflowOrchestrator) ReplayOnly(ctx context.Context, testCommand, testSetID string) (*ReplayStats, error) {
	wo.mu.Lock()
	if wo.currentPhase != PhaseIdle && wo.currentPhase != PhaseCompleted && wo.currentPhase != PhaseFailed {
		wo.mu.Unlock()
		return nil, fmt.Errorf("workflow already in progress: %s", wo.currentPhase)
	}

	workflowCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	execution := &WorkflowExecution{
		ID:          fmt.Sprintf("replay-%d", time.Now().UnixNano()),
		TestCommand: testCommand,
		TestSetID:   testSetID,
		Phase:       PhaseReplaying,
		StartTime:   time.Now(),
		CancelFunc:  cancel,
	}
	wo.currentWorkflow = execution
	wo.currentPhase = PhaseReplaying
	wo.mu.Unlock()

	defer func() {
		wo.mu.Lock()
		wo.currentPhase = PhaseCompleted
		wo.mu.Unlock()
	}()

	return wo.executeReplayPhase(workflowCtx, execution)
}
