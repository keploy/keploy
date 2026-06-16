package models

import (
	"errors"
	"fmt"
)

type AppError struct {
	AppErrorType AppErrorType
	Err          error
	AppLogs      string
}

type AppErrorType string

func (e AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.AppErrorType, e.Err)
	}
	return string(e.AppErrorType)
}

// AppErrorType is a type of error that can be returned by the application
const (
	ErrCommandError   AppErrorType = "exited due to command error"
	ErrUnExpected     AppErrorType = "an unexpected error occurred"
	ErrInternal       AppErrorType = "an internal error occurred"
	ErrAppStopped     AppErrorType = "app stopped"
	ErrCtxCanceled    AppErrorType = "context canceled"
	ErrTestBinStopped AppErrorType = "test binary stopped"
)

// MockFieldDiffKind classifies a single field-level divergence between a live
// outgoing request and the closest recorded mock.
type MockFieldDiffKind string

const (
	DiffKindValueChanged  MockFieldDiffKind = "value_changed"   // same field, different value
	DiffKindTypeChanged   MockFieldDiffKind = "type_changed"    // same field, different JSON type
	DiffKindMissingInLive MockFieldDiffKind = "missing_in_live" // recorded mock has it, live request doesn't
	DiffKindMissingInMock MockFieldDiffKind = "missing_in_mock" // live request has it, recorded mock doesn't
)

// MockFieldDiff is one field-level difference between the live request and the
// closest candidate mock. Path uses the same vocabulary as the noise
// configuration (pkg/matcher): "body.<dotted.json.path>", "header.<name>",
// "query.<name>", plus the pseudo-fields "method" and "path". This shared
// grammar is deliberate: a user can copy Path straight into
// test.globalNoise / spec.assertions.noise.
type MockFieldDiff struct {
	Path     string            `json:"path" yaml:"path"`
	Kind     MockFieldDiffKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	Expected string            `json:"expected,omitempty" yaml:"expected,omitempty"` // recorded (mock) value
	Actual   string            `json:"actual,omitempty" yaml:"actual,omitempty"`     // live request value
}

// Match-cascade phases recorded on MockMismatchReport.MatchPhase. They tell
// the user how far the matcher got before giving up, which determines the
// right remediation (re-record vs add noise vs fix candidate selection).
const (
	MatchPhaseNoMocks    = "no_mocks"             // mock pool for this protocol is empty
	MatchPhaseSchema     = "no_schema_candidates" // nothing matched method/path/header-keys/query-keys
	MatchPhaseBody       = "body_mismatch"        // schema candidates existed, request body ruled them all out
	MatchPhaseStrict     = "strict_noise_reject"  // candidates rejected by strict req-body-noise enforcement
	MatchPhaseExhausted  = "no_match"             // full cascade ran and nothing matched
	MatchPhaseProtoError = "protocol_error"       // matching aborted on a protocol/decode error
)

// MockMismatchReport describes what didn't match when a mock lookup fails.
// It is populated by protocol-specific matching logic and surfaced to the user
// in the CLI mismatch table, the test-report yaml (FailureInfo.UnmatchedCalls)
// and the platform/UI APIs. Protocol parsers should build it via
// pkg/agent/proxy/integrations/mismatch so vocabulary stays uniform.
type MockMismatchReport struct {
	Protocol       string          // "HTTP", "MySQL", "PostgreSQL", "MongoDB", "gRPC", "HTTP/2", "Generic", "DNS"
	ActualSummary  string          // Brief description of the actual request
	ClosestMock    string          // Name of the closest mock (empty if none)
	Diff           string          // Human-readable diff (protocol-specific)
	NextSteps      string          // Actionable suggestion for the user
	MatchPhase     string          // how far the match cascade got (MatchPhase* constants)
	CandidateCount int             // protocol mocks considered before giving up
	FieldDiffs     []MockFieldDiff // field-level diffs vs the closest mock, noise-vocabulary paths
}

// ErrNoMockMatched is the sentinel for a genuine mock miss — an outgoing call
// for which no recorded mock matched. Protocol parsers wrap it (errors.Is)
// when they report a miss, so the proxy can distinguish real misses from
// infrastructure/decode failures when building UnmatchedCalls for reports.
var ErrNoMockMatched = errors.New("no matching mock found")

type ParserError struct {
	ParserErrorType ParserErrorType
	Err             error
	MismatchReport  *MockMismatchReport // nil when no diff is available
}

type ParserErrorType string

func (e ParserError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.ParserErrorType, e.Err)
	}
	return string(e.ParserErrorType)
}

const (
	ErrMockNotFound ParserErrorType = "mock not found"
)

// mockMismatchError wraps an error with a MockMismatchReport so protocol
// decode layers can carry diff information through the error chain to proxy.go.
type mockMismatchError struct {
	err    error
	report *MockMismatchReport
}

func (e *mockMismatchError) Error() string { return e.err.Error() }
func (e *mockMismatchError) Unwrap() error { return e.err }

// MismatchReport returns the attached diff report.
func (e *mockMismatchError) MismatchReport() *MockMismatchReport { return e.report }

// NewMockMismatchError wraps err with a MockMismatchReport for propagation.
func NewMockMismatchError(err error, report *MockMismatchReport) error {
	if report == nil {
		return err
	}
	return &mockMismatchError{err: err, report: report}
}
