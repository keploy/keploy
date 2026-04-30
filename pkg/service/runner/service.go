package integration

import (
	"context"
	"encoding/json"
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

// MockRef identifies a single mock in a mismatch report. Kind
// carries the protocol (Http / Postgres / MySQL / …) so downstream
// consumers can render the right icon / classification without running
// name-substring heuristics at the UI layer.
type MockRef struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
}

// UnmarshalJSON accepts either the new {name, kind} object form or the
// legacy bare-string form on the wire. Lets older runners / test reports
// keep round-tripping through code built against the new schema until
// every producer is upgraded.
func (e *MockRef) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		e.Name = s
		return nil
	}
	type raw MockRef
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	*e = MockRef(r)
	return nil
}

// MockMismatch captures expected vs consumed mocks for a test step.
// Entries now carry kind (previously just names) so persisted reports
// and downstream UI do not need to re-derive kind from the mock name.
type MockMismatch struct {
	ExpectedMocks []MockRef `json:"expected_mocks"`
	ConsumedMocks []MockRef `json:"consumed_mocks"`
}

// RunTestOpts contains the minimal inputs to run a single integration test.
type RunTestOpts struct {
	TestSetID  string
	TestStepID string // TestStepID is the test case name used to load the case from TestCaseDB.
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
