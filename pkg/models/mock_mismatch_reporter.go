package models

import (
	"context"
	"errors"
	"slices"
)

// mockMismatchReporterKey is the unexported context key under which the proxy
// stashes its mock-mismatch reporter for the duration of a MockOutgoing call.
type mockMismatchReporterKey struct{}

// WithMockMismatchReporter returns a context carrying report — the callback the
// proxy uses to record a mock-mismatch (the same sink as a returned
// ErrMockNotFound). It lets an integration surface a miss to the test report
// WITHOUT returning the error that ends — and tears down — the client
// connection.
//
// Most parsers don't need this: they return the miss error and the resulting
// connection close is harmless. It exists for protocols where the close itself
// is destructive — notably Pulsar, where closing the connection on a SEND
// mismatch makes pulsar-client-go reconnect the producer and crash on a nil
// schema (grabCnx -> schemaCache.Put). Such a parser reports the miss via this
// hook, replies to the client normally, and keeps serving the connection.
//
// A nil report is ignored so callers can pass through unconditionally.
func WithMockMismatchReporter(ctx context.Context, report func(error)) context.Context {
	if report == nil {
		return ctx
	}
	return context.WithValue(ctx, mockMismatchReporterKey{}, report)
}

// ReportMockMismatch records err via the reporter installed by
// WithMockMismatchReporter, returning true when a reporter was present (so the
// caller may keep the connection alive instead of returning the error). When no
// reporter is installed — e.g. an older agent, or a non-test path — it is a
// no-op returning false, and the caller should fall back to returning err.
func ReportMockMismatch(ctx context.Context, err error) bool {
	report, ok := ctx.Value(mockMismatchReporterKey{}).(func(error))
	if !ok || report == nil {
		return false
	}
	report(err)
	return true
}

// MismatchReported is a miss its parser already reported (ReportMockMismatch)
// and returns as well: the connection handler then ends the connection, and
// logs the miss, as it does for a miss returned unreported, without reporting
// it a second time (IsMismatchReported).
type MismatchReported struct{ Err error }

func (m MismatchReported) Error() string { return m.Err.Error() }

func (m MismatchReported) Unwrap() error { return m.Err }

// IsMismatchReported reports whether err is a miss its parser already
// reported (MismatchReported).
func IsMismatchReported(err error) bool {
	var m MismatchReported
	return errors.As(err, &m)
}

// ReportAnswer reports an answered call in-band through the reporter that
// WithMockMismatchReporter installed (the sink misses take), with r.Answer
// set. The sink stamps At, Kind (MissInTest, MissOutsideTest), BeforeSet and
// Source as it does for a miss. It returns false when no reporter is
// installed; the caller answers the call either way. An empty r.Answer is a
// programming error, and panics.
//
// The report travels as ErrAnswered carrying a copy of r that shares nothing
// with it (NewMockMismatchError), so the parser keeps r, and the sink tells it
// from a miss by errors.Is(err, ErrAnswered) as well as by its Answer.
func ReportAnswer(ctx context.Context, r *MockMismatchReport) bool {
	if r == nil || r.Answer == "" {
		panic("models.ReportAnswer: a report without an Answer")
	}
	report, ok := ctx.Value(mockMismatchReporterKey{}).(func(error))
	if !ok || report == nil {
		return false
	}
	answered := *r
	answered.FieldDiffs = slices.Clone(r.FieldDiffs)
	answered.Differs = slices.Clone(r.Differs)
	answered.VariablePlaces = slices.Clone(r.VariablePlaces)
	answered.Source = r.Source.clone()
	report(NewMockMismatchError(ErrAnswered, &answered))
	return true
}
