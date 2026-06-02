// Package recorder is a fixture for analysistest: a buggy V2 parser that
// calls time.Now() to stamp a mock, the exact I5-invariant violation the
// analyzer must flag.
package recorder

import "time"

// Mock is a minimal stand-in for models.Mock's timestamp fields.
type Mock struct {
	ReqTimestampMock time.Time
	ResTimestampMock time.Time
}

// Record reaches for time.Now() at record time — the bug the analyzer is
// designed to catch. Expect exactly one diagnostic, anchored to the time.Now
// selector.
func Record() Mock {
	m := Mock{}
	m.ReqTimestampMock = time.Now() // want `time.Now is forbidden in parser record-path files`
	return m
}
