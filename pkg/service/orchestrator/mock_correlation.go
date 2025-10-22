package orchestrator

import (
	"context"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestContext represents the context of a test execution
type TestContext struct {
	TestID      string
	TestSet     string
	MockChannel chan *models.Mock
	Done        chan struct{}
}

// MockCorrelationManager manages the correlation between tests and mocks
type MockCorrelationManager struct {
	// Map of test execution ID to mock channel
	testMockChannels map[string]chan *models.Mock
	// Map of test execution ID to test metadata
	activeTests map[string]TestContext
	// Mutex for thread safety
	mutex sync.RWMutex
	// Global mock channel from proxy
	globalMockCh chan *models.Mock
	// Context for shutdown
	ctx    context.Context
	logger *zap.Logger
	// Router for mock routing strategy
	router MockRouter
}

// MockRouter interface for routing mocks to appropriate tests
type MockRouter interface {
	RouteToTest(mock *models.Mock, activeTests map[string]TestContext) string
}

type ActiveTestRouter struct{}

// activeTestRouter routes mocks to only one active test (if any), because it is running serially
func (atr *ActiveTestRouter) RouteToTest(mock *models.Mock, activeTests map[string]TestContext) string {
	for testID := range activeTests {
		return testID
	}
	return ""
}

// NewMockCorrelationManager creates a new mock correlation manager
func NewMockCorrelationManager(ctx context.Context, globalMockCh chan *models.Mock, logger *zap.Logger) *MockCorrelationManager {
	mcm := &MockCorrelationManager{
		testMockChannels: make(map[string]chan *models.Mock),
		activeTests:      make(map[string]TestContext),
		globalMockCh:     globalMockCh,
		ctx:              ctx,
		logger:           logger,
		router:           &ActiveTestRouter{},
	}

	return mcm
}

// routeMocks continuously routes incoming mocks to appropriate test channels
func (mcm *MockCorrelationManager) routeMocks() {
	for {
		select {
		case mock := <-mcm.globalMockCh:
			mcm.routeMockToTest(mock)
		case <-mcm.ctx.Done():
			mcm.closeAllChannels()
			return
		}
	}
}

// routeMockToTest routes a mock to the appropriate test channel
func (mcm *MockCorrelationManager) routeMockToTest(mock *models.Mock) {
	mcm.mutex.RLock()
	defer mcm.mutex.RUnlock()

	if len(mcm.activeTests) == 0 {
		mcm.logger.Debug("No active tests to route mock to", zap.String("mockKind", mock.GetKind()))
		return
	}

	// Use router to determine which test should receive this mock
	targetTestID := mcm.router.RouteToTest(mock, mcm.activeTests)

	if targetTestID == "" {
		mcm.logger.Debug("No suitable test found for mock", zap.String("mockKind", mock.GetKind()))
		return
	}

	if mockCh, exists := mcm.testMockChannels[targetTestID]; exists {
		select {
		case mockCh <- mock:
			mcm.logger.Debug("Mock routed to test",
				zap.String("testID", targetTestID),
				zap.String("mockKind", mock.GetKind()))
		default:
			mcm.logger.Warn("Mock channel full, dropping mock",
				zap.String("testID", targetTestID),
				zap.String("mockKind", mock.GetKind()))
		}
	}
}

// RegisterTest registers a new test execution and creates a mock channel for it
func (mcm *MockCorrelationManager) RegisterTest(testCtx TestContext) {
	mcm.mutex.Lock()
	defer mcm.mutex.Unlock()

	// Create buffered channel for mocks
	mockCh := make(chan *models.Mock, 100)
	testCtx.MockChannel = mockCh
	testCtx.Done = make(chan struct{})

	mcm.testMockChannels[testCtx.TestID] = mockCh
	mcm.activeTests[testCtx.TestID] = testCtx

	mcm.logger.Debug("Test registered for mock correlation",
		zap.String("testID", testCtx.TestID))
}

// UnregisterTest removes a test from active tracking and closes its mock channel
func (mcm *MockCorrelationManager) UnregisterTest(testID string) {
	mcm.mutex.Lock()
	defer mcm.mutex.Unlock()

	if testCtx, exists := mcm.activeTests[testID]; exists {
		// Signal that test is done
		close(testCtx.Done)

		// Close mock channel
		if mockCh, chExists := mcm.testMockChannels[testID]; chExists {
			close(mockCh)
			delete(mcm.testMockChannels, testID)
		}

		delete(mcm.activeTests, testID)

		mcm.logger.Debug("Test unregistered from mock correlation",
			zap.String("testID", testID))
	}
}

// GetTestMocks returns the mock channel for a specific test
func (mcm *MockCorrelationManager) GetTestMocks(testID string) <-chan *models.Mock {
	mcm.mutex.RLock()
	defer mcm.mutex.RUnlock()

	if mockCh, exists := mcm.testMockChannels[testID]; exists {
		return mockCh
	}
	return nil
}

// GetTestContext returns the test context for a specific test
func (mcm *MockCorrelationManager) GetTestContext(testID string) (TestContext, bool) {
	mcm.mutex.RLock()
	defer mcm.mutex.RUnlock()

	testCtx, exists := mcm.activeTests[testID]
	return testCtx, exists
}

// GetActiveTestCount returns the number of currently active tests
func (mcm *MockCorrelationManager) GetActiveTestCount() int {
	mcm.mutex.RLock()
	defer mcm.mutex.RUnlock()

	return len(mcm.activeTests)
}

// closeAllChannels closes all active test channels during shutdown
func (mcm *MockCorrelationManager) closeAllChannels() {
	mcm.mutex.Lock()
	defer mcm.mutex.Unlock()

	for testID, testCtx := range mcm.activeTests {
		close(testCtx.Done)
		if mockCh, exists := mcm.testMockChannels[testID]; exists {
			close(mockCh)
		}
	}

	// Clear maps
	mcm.testMockChannels = make(map[string]chan *models.Mock)
	mcm.activeTests = make(map[string]TestContext)
}

// SetRouter allows changing the routing strategy
func (mcm *MockCorrelationManager) SetRouter(router MockRouter) {
	mcm.mutex.Lock()
	defer mcm.mutex.Unlock()

	mcm.router = router
	mcm.logger.Debug("Mock routing strategy updated")
}
