package models

import (
	"context"
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

// ReportMockMismatchOnChannel publishes an ErrMockNotFound ParserError
// onto the proxy error channel that the caller passed in via context
// under ProxyErrChannelKey. Used by long-lived parsers (Pulsar today,
// Kafka tomorrow) that multiplex many logical streams over a single
// connection and therefore cannot signal a mock miss by returning
// from MockOutgoing — doing so would tear down every other stream
// sharing the connection. The function is a no-op when the context
// carries no channel (e.g. during unit tests that exercise the parser
// without the proxy harness) so call sites stay branch-free.
//
// The returned bool tells the caller whether the event was actually
// published. A non-blocking send is used; if the buffered channel is
// full the event is dropped (matches Proxy.SendError behavior — better
// to lose a single mismatch event than to wedge the parser goroutine
// when the consumer is slow).
func ReportMockMismatchOnChannel(ctx context.Context, baseErr error, report *MockMismatchReport) bool {
	if ctx == nil || report == nil {
		return false
	}
	ch, ok := ctx.Value(ProxyErrChannelKey).(chan<- error)
	if !ok || ch == nil {
		return false
	}
	parserErr := ParserError{
		ParserErrorType: ErrMockNotFound,
		Err:             baseErr,
		MismatchReport:  report,
	}
	select {
	case ch <- parserErr:
		return true
	default:
		return false
	}
}
