//go:build linux

package orchestrator

import (
	"context"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// TestContext represents the context of a test execution
type TestContext struct {
	TestID      string
	TestName    string
	TestSet     string
	StartTime   time.Time
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

// TimeBasedRouter routes mocks based on timestamp correlation
type TimeBasedRouter struct{}

// RouteToTest routes mock to the test that was active when the mock was received
func (tbr *TimeBasedRouter) RouteToTest(mock *models.Mock, activeTests map[string]TestContext) string {
	now := time.Now()

	// For time-based routing, route to the most recent active test
	var mostRecentTestID string
	var mostRecentTime time.Time

	for testID, testCtx := range activeTests {
		// Check if mock was received during test execution window
		if testCtx.StartTime.Before(now) && testCtx.StartTime.After(mostRecentTime) {
			mostRecentTestID = testID
			mostRecentTime = testCtx.StartTime
		}
	}

	return mostRecentTestID
}

// NewMockCorrelationManager creates a new mock correlation manager
func NewMockCorrelationManager(ctx context.Context, globalMockCh chan *models.Mock, logger *zap.Logger) *MockCorrelationManager {
	mcm := &MockCorrelationManager{
		testMockChannels: make(map[string]chan *models.Mock),
		activeTests:      make(map[string]TestContext),
		globalMockCh:     globalMockCh,
		ctx:              ctx,
		logger:           logger,
		router:           &TimeBasedRouter{},
	}

	// Start the mock routing goroutine
	go mcm.routeMocks()

	return mcm
}

// routeMocks continuously routes incoming mocks to appropriate test channels
func (mcm *MockCorrelationManager) routeMocks() {
	for {
		select {
		case mock := <-mcm.globalMockCh:
			mcm.routeMockToTest(mock)
		case <-mcm.ctx.Done():
			mcm.logger.Info("Mock correlation manager shutting down")
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
		mcm.logger.Info("No active tests to route mock to", zap.String("mockKind", mock.GetKind()))
		return
	}

	// Use router to determine which test should receive this mock
	targetTestID := mcm.router.RouteToTest(mock, mcm.activeTests)

	if targetTestID == "" {
		mcm.logger.Info("No suitable test found for mock", zap.String("mockKind", mock.GetKind()))
		return
	}

	if mockCh, exists := mcm.testMockChannels[targetTestID]; exists {
		select {
		case mockCh <- mock:
			mcm.logger.Info("Mock routed to test",
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

	mcm.logger.Info("Test registered for mock correlation",
		zap.String("testID", testCtx.TestID),
		zap.String("testName", testCtx.TestName))
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

		mcm.logger.Info("Test unregistered from mock correlation",
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
	mcm.logger.Info("Mock routing strategy updated")
}
