package replay

import (
	"context"
	"io"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

type Instrumentation interface {
	//Setup prepares the environment for the recording
	Setup(ctx context.Context, cmd string, opts models.SetupOptions) error

	MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error
	// GetConsumedMocks to log the names of the mocks that were consumed during the test run of failed test cases
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
	// Run is blocking call and will execute until error
	Run(ctx context.Context, opts models.RunOptions) models.AppError
	// GetErrorChannel returns the error channel from the proxy for monitoring proxy errors
	GetErrorChannel() <-chan error
	// GetMockErrors returns mock-not-found errors collected during replay
	GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error)
	BeforeSimulate(ctx context.Context, timestamp *time.Time, testSetID string, testCaseName string) error
	AfterSimulate(ctx context.Context, tcName string, testSetID string) error
	BeforeTestRun(ctx context.Context, testRunID string) error
	BeforeTestSetCompose(ctx context.Context, testRunID string, testSetID string, firstRun bool) error
	AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error
	// New methods for improved mock management
	StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	UpdateMockParams(ctx context.Context, params models.MockFilterParams) error
	GetRecentAppLogs(ctx context.Context) string
	MakeAgentReadyForDockerCompose(ctx context.Context) error
	// NotifyGracefulShutdown notifies the agent that the application is shutting down gracefully.
	// When this is called, connection errors will be logged as debug instead of error.
	NotifyGracefulShutdown(ctx context.Context) error
	// ComposeDownOnSetupFailure tears down the docker-compose stack (agent + app
	// + dependency containers + project network) when per-test-set setup fails
	// (e.g. agent-readiness timeout), so a retry's `compose up` does not hit a
	// "container name already in use" conflict. No-op for non-compose apps.
	ComposeDownOnSetupFailure(ctx context.Context) error
}

// ConsumerInstrumentation is the OPTIONAL extension of Instrumentation that a
// Kind: Consumer test needs. RunTestSet reaches it by type assertion, exactly
// as it reaches TestCaseMutator and MockMutator.
//
// IT IS A SEPARATE INTERFACE ON PURPOSE, and this is not a style preference.
// Instrumentation is a fifteen-method interface implemented in this repository
// AND in two others that pin an older module version of it. Go has no optional
// interface methods, so adding these three to Instrumentation would break the
// enterprise agent and k8s-proxy at COMPILE TIME the moment they picked up the
// tag — forcing exactly the lock-step three-repository bump this design exists
// to avoid, for a capability neither of them can use until they choose to.
//
// WHERE THE IMPLEMENTATIONS ARE. `keploy test` always reaches the agent over
// HTTP — cli/provider hands the replayer an *platform/http.AgentClient — so the
// seam is only closed if all three links exist, and they do:
//
//	pkg/platform/http.AgentClient   the replayer's side: POSTs /agent/consumer/{arm,await,reset}
//	pkg/agent/routes                those three routes, reached by capability assertion on agent.Service
//	pkg/service/agent.Agent         forwards to the proxy that owns the gate
//	pkg/agent/proxy.Proxy           resolves the recorded trigger and drives consumer.Gate
//
// What is still absent in OSS is a protocol PARSER: nothing here registers a
// consumer.Deliverer or a consumer.Projector, so no CONSUMER test case can be
// recorded and an armed window has nobody to deliver through. That is the
// deliberate inertness of this slice (design §6, §7 slice 5) and it is a
// different thing from an unimplemented seam — an enterprise parser can supply
// the missing half without a second OSS tag.
//
// WHEN THE ASSERTION FAILS THE TEST IS REFUSED, NEVER DEGRADED. An agent that
// cannot arm a delivery window cannot deliver the recorded message at all, so
// the worker produces nothing and the test would otherwise report "the worker
// stopped producing" — blaming the application for a missing capability in
// keploy. SimulateRequest returns a refusal result carrying
// models.CategoryConsumerUnsupportedAgent instead, which is a FAILED test with
// a named reason and a non-zero exit. There is no weak-verdict path here that
// can still print PASSED.
type ConsumerInstrumentation interface {
	// ArmConsumerTrigger opens the delivery window for one test and hands its
	// recorded trigger to the application. It returns once the trigger has
	// been handed over (or stashed for the next poll), NOT once the worker has
	// finished: waiting is AwaitConsumerEffects' job.
	ArmConsumerTrigger(ctx context.Context, arm models.ConsumerArm) error

	// AwaitConsumerEffects blocks until the armed test's window closes under
	// the completion rule (expected effect count observed AND the grace drain
	// elapsed) or its backstop fires, and returns everything observed inside
	// it. A non-nil result with a Refusal set is a NAMED failure, not an
	// error; an error means the request itself did not complete.
	AwaitConsumerEffects(ctx context.Context, testID string) (*models.ConsumerResult, error)

	// ResetConsumerGate returns the delivery gate to its default-closed boot
	// phase at a test-set boundary. It exists because --keep-app-alive reuses
	// one application process across test sets: a gate left armed, or an
	// effect adopted across the boundary, would leak one set's state into the
	// next set's first test.
	//
	// It returns how many effect RECORDS the reset left unattributed. That
	// count is an APPLICATION regression (the worker produced after the last
	// test of the set closed its window, which is the only place an N+1
	// emission at the very end of a run can be seen); the error is this CALL
	// failing. They are separate returns because collapsing them made every
	// unreachable agent read as "your worker over-produces".
	ResetConsumerGate(ctx context.Context, testSetID string) (int, error)
}

type Service interface {
	Start(ctx context.Context) error
	Instrument(ctx context.Context) (*InstrumentState, error)
	GetNextTestRunID(ctx context.Context) (string, error)
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	RunTestSet(ctx context.Context, testSetID string, testRunID string, serveTest bool) (models.TestSetStatus, error)
	GetTestSetStatus(ctx context.Context, testRunID string, testSetID string) (models.TestSetStatus, error)
	GetTestCases(ctx context.Context, testID string) ([]*models.TestCase, error)
	GetTestSetConf(ctx context.Context, testSetID string) (*models.TestSet, error)
	// UpdateTestSetTemplate persists the (possibly updated) template map for a test-set.
	// Used during replay to dynamically refresh values like JWTs/IDs as soon as
	// their producing API responses are observed, so subsequent test cases use the
	// latest values rather than stale ones from the previous run.
	UpdateTestSetTemplate(ctx context.Context, testSetID string, template map[string]interface{}) error
	RunApplication(ctx context.Context, opts models.RunOptions) models.AppError
	DenoiseTestCases(ctx context.Context, testSetID string, noiseParams []*models.NoiseParams) ([]*models.NoiseParams, error)
	DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error
	DeleteTestSet(ctx context.Context, testSetID string) error

	StoreMappings(ctx context.Context, mapping *models.Mapping) error

	// CompareHTTPResp compares HTTP responses and returns match result with detailed diffs
	CompareHTTPResp(tc *models.TestCase, actualResponse *models.HTTPResp, testSetID string, emitFailureLogs bool) (bool, *models.Result)
	// CompareGRPCResp compares gRPC responses and returns match result with detailed diffs
	CompareGRPCResp(tc *models.TestCase, actualResp *models.GrpcResp, testSetID string, emitFailureLogs bool) (bool, *models.Result)
}

type TestDB interface {
	GetAllTestSetIDs(ctx context.Context) ([]string, error)
	GetTestCases(ctx context.Context, testSetID string) ([]*models.TestCase, error)
	UpdateTestCase(ctx context.Context, testCase *models.TestCase, testSetID string, enableLog bool) error
	DeleteTests(ctx context.Context, testSetID string, testCaseIDs []string) error
	DeleteTestSet(ctx context.Context, testSetID string) error
}

type MockDB interface {
	GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error)
	GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error)
	UpdateMocks(ctx context.Context, testSetID string, mockNames map[string]models.MockState, pruneBefore time.Time, startupCutoffTime time.Time) error
}

type ReportDB interface {
	GetAllTestRunIDs(ctx context.Context) ([]string, error)
	GetTestCaseResults(ctx context.Context, testRunID string, testSetID string) ([]models.TestResult, error)
	GetReport(ctx context.Context, testRunID string, testSetID string) (*models.TestReport, error)
	ClearTestCaseResults(_ context.Context, testRunID string, testSetID string)
	InsertTestCaseResult(ctx context.Context, testRunID string, testSetID string, result *models.TestResult) error // 1
	InsertReport(ctx context.Context, testRunID string, testSetID string, testReport *models.TestReport) error     // 2
	UpdateReport(ctx context.Context, testRunID string, testCoverage any) error
}

type TestSetConfig interface {
	Read(ctx context.Context, testSetID string) (*models.TestSet, error)
	Write(ctx context.Context, testSetID string, testSet *models.TestSet) error
	ReadSecret(ctx context.Context, testSetID string) (map[string]interface{}, error)
}

type Telemetry interface {
	TestSetRun(success int, failure int, testSet string, runStatus string)
	TestRun(success int, failure int, testSets int, mocksConsumed int, runStatus string, metadata map[string]interface{})
	// TestRunAborted records a graceful replay stop that happened before the
	// TestRun summary was emitted (e.g. setup/instrumentation failed before
	// any test set ran), carrying a categorized stop_reason for the funnel.
	TestRunAborted(stopReason string)
	MockTestRun(utilizedMocks int)
}

type TestHooks interface {
	SimulateRequest(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error)
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
	// GetNoisyTestCaseNames returns test case names that were reclassified as noisy
	// for the provided test set during BeforeTestResult processing.
	GetNoisyTestCaseNames(testSetID string) []string
	BeforeTestRun(ctx context.Context, testRunID string) error
	BeforeTestSetCompose(ctx context.Context, testRunID string, testSetID string, firstRun bool) error
	BeforeTestSetRun(ctx context.Context, testSetID string) error
	BeforeTestSetReplay(ctx context.Context, testSetID string) error
	BeforeTestResult(ctx context.Context, testRunID string, testSetID string, testCaseResults []models.TestResult) error
	AfterTestSetRun(ctx context.Context, testSetID string, status bool) error
	AfterTestRun(ctx context.Context, testRunID string, testSetIDs []string, coverage models.TestCoverage) error // hook executed after running all the test-sets
}

// TestCaseMutator is an optional extension to TestHooks.
// RunTestSet detects it via type assertion; implementations that do not need
// per-test-case mutations can safely omit it without breaking existing code.
type TestCaseMutator interface {
	// BeforeTestCaseRun is invoked once per test case, before SimulateRequest.
	// Implementations may mutate tc in-place (e.g. decrypt protected fields,
	// inject headers). It is called at most once per test case even when
	// RetryPassing is enabled.
	BeforeTestCaseRun(ctx context.Context, tc *models.TestCase, testSetID string) error
}

// MockMutator is an optional extension to TestHooks.
// RunTestSet detects it via type assertion after GetMocks returns and before
// StoreMocks pushes mocks to the proxy. Implementations may mutate mocks
// in-place (e.g. decrypt ENC: fields) so the proxy always receives plaintext
// values without ever writing plaintext to disk.
type MockMutator interface {
	// AfterGetMocks is invoked once per test set, after all mocks are loaded
	// from disk and before they are sent to the proxy. Both slices may be
	// mutated in-place; nil slices are a no-op.
	AfterGetMocks(ctx context.Context, filtered []*models.Mock, unfiltered []*models.Mock) error
}

// TestSetOrderer is an optional extension to TestHooks.
// Start and GetSelectedTestSets detect it via type assertion after the
// natural-sort of test-set IDs; implementations that do not need a custom
// run order can safely omit it and keep the sorted order.
type TestSetOrderer interface {
	// OrderTestSets receives the natural-sorted test-set IDs and returns
	// them in the order they should run. The result must contain exactly
	// the same IDs; a result of a different length is ignored.
	OrderTestSets(testSets []string) []string
}

type Storage interface {
	Upload(ctx context.Context, file io.Reader, mockName string, appName string, jwtToken string) error // 3
	Download(ctx context.Context, mockName string, appName string, userName string, jwtToken string) (io.Reader, error)
}

type InstrumentState struct {
	HookCancel context.CancelFunc
}

type MappingDB interface {
	Insert(ctx context.Context, mapping *models.Mapping) error
	Get(ctx context.Context, testSetID string) (map[string][]models.MockEntry, bool, error)
	// Exists reports whether the mappings.yaml file is present on disk
	// for the given test-set. Distinct from Get's second return (which
	// reports "file present AND contains at least one test case with
	// mocks") because the create-if-not-present write path needs to
	// distinguish "no file at all" from "file exists but has empty
	// entries" — only the former should trigger an automatic create.
	Exists(ctx context.Context, testSetID string) (bool, error)
}
