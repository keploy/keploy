package integration

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestResult is the outcome of a single integration test step.
type TestResult struct {
	Passed         bool
	Diff           string
	Error          string
	MockMismatches *MockMismatch
	Noise          map[string][]string
}

// MockMismatch captures expected vs consumed mocks for a test step.
type MockMismatch struct {
	ExpectedMocks []string `json:"expected_mocks"`
	ConsumedMocks []string `json:"consumed_mocks"`
}

// RunTestOpts contains the minimal inputs to run a single integration test.
type RunTestOpts struct {
	TestSetID  string
	TestStepID string
	ServiceURL string
}

// Service runs a single integration test end-to-end.
type Service interface {
	RunTest(ctx context.Context, opts RunTestOpts) *TestResult
}

// --- Dependencies (defined here so the runner is self-contained) ---

// TestCaseDB loads recorded test cases from storage.
// Implemented by go.keploy.io/server/v3/pkg/platform/yaml/testdb.TestYaml.
type TestCaseDB interface {
	GetTestCase(ctx context.Context, testSetID string, testCaseName string) (*models.TestCase, error)
}

// Instrumentation is the agent/proxy control surface needed for mock loading.
type Instrumentation interface {
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) error
	MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error
	StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	UpdateMockParams(ctx context.Context, params models.MockFilterParams) error
	NotifyGracefulShutdown(ctx context.Context) error
	MakeAgentReadyForDockerCompose(ctx context.Context) error
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
}

// MockDB reads mocks from storage.
type MockDB interface {
	GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error)
	GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error)
}

// MappingDB provides test-case → mock-name mapping.
type MappingDB interface {
	Get(ctx context.Context, testSetID string) (map[string][]models.MockEntry, bool, error)
}
