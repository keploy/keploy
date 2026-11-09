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
// never a value.
type BackgroundBinding struct {
	Recording  string `json:"recording" yaml:"recording"`
	Place      string `json:"place" yaml:"place"`
	Variable   string `json:"variable" yaml:"variable"`
	BoundMock  string `json:"bound_mock" yaml:"bound_mock"`
	BoundPlace string `json:"bound_place" yaml:"bound_place"`
}
