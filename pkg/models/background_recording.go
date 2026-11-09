package models

import "time"

// BackgroundRecording is one of the staged set's background recordings
// (MockFilterParams.BackgroundMocks: its mappings.yaml worker: and startup:
// mocks) as the agent hands it to the proxy: the recording, served or not,
// and the span of the recording's app clock in which the application can
// have read its clock for the call. From is the request time of the set's
// data-plane recording made before it (the latest one sent before it, not
// itself, not answered after it was sent, as learn.PreviousCall reads it),
// zero for the set's first call; the span ends at the recording's own
// request time (Mock.Spec.ReqTimestampMock). Another goroutine's call
// recorded between a read and the call that carries it narrows the span
// past the read: a check of the read against it then fails, and the miss
// stays the test's.
//
// Mock is shared and read-only: it is never served, never consumed, and a
// reader must not change it.
type BackgroundRecording struct {
	Mock *Mock
	From time.Time
}

// BackgroundBinding names one place at which a background miss
// (MockMismatchReport.Background) differs from the background recording it
// is the same request as: the recording holds the recorded value of a
// variable the call's own test bound before the call, and the call holds the
// value the test bound it to (variables.Table.Own). Recording is that
// background recording, Place the call's place (body.<path>, as learned
// noise names it), Variable the binding's name, and BoundMock and BoundPlace
// where the test bound it (VariableBinding.Mock and Place). Names only,
// never a value; evidence keeps it Sanitized.
type BackgroundBinding struct {
	Recording  string `json:"recording" yaml:"recording"`
	Place      string `json:"place" yaml:"place"`
	Variable   string `json:"variable,omitempty" yaml:"variable,omitempty"`
	BoundMock  string `json:"bound_mock" yaml:"bound_mock"`
	BoundPlace string `json:"bound_place" yaml:"bound_place"`
}

// Sanitized is b as evidence keeps it (UnmatchedCall.Sanitized): the names
// of the two recordings, and each place in its shape (FieldShape), since a
// key on the way to it can be a value the app sent (a map keyed by an
// email). A learned variable's name stays out, as a variable miss's does:
// it can be such a key. A given's (ValidGivenVariable) is the test's own
// name for a value it chose, and is kept.
func (b BackgroundBinding) Sanitized() BackgroundBinding {
	out := BackgroundBinding{Recording: b.Recording, Place: FieldShape(b.Place), BoundMock: b.BoundMock, BoundPlace: FieldShape(b.BoundPlace)}
	if ValidGivenVariable(b.Variable) {
		out.Variable = b.Variable
	}
	return out
}

// sanitizedBindings is each of bs Sanitized, in a new slice; nil for none.
func sanitizedBindings(bs []BackgroundBinding) []BackgroundBinding {
	if len(bs) == 0 {
		return nil
	}
	out := make([]BackgroundBinding, len(bs))
	for i, b := range bs {
		out[i] = b.Sanitized()
	}
	return out
}
