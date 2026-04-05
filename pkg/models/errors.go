package models

import "fmt"

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

// MockMismatchReport describes what didn't match when a mock lookup fails.
// It is populated by protocol-specific matching logic and surfaced to the user
// in the CLI mismatch table.
type MockMismatchReport struct {
	Protocol      string // "HTTP", "MySQL", "PostgreSQL", "MongoDB", "gRPC", "HTTP/2", "Generic", "DNS"
	ActualSummary string // Brief description of the actual request
	ClosestMock   string // Name of the closest mock (empty if none)
	Diff          string // Human-readable diff (protocol-specific)
	NextSteps     string // Actionable suggestion for the user
}

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
