package manager

import (
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// fate.go is what the lines that report a span keploy could not record as a
// mock (an exchange a parser left out, a hole, a connection it stopped
// recording) say becomes of the test cases recorded over it. With the proven
// switch off, or to a client that does not charge it, such a test case is
// left out of the recording (routes/record.go); under the switch, to a
// client that charges what a test case lost (models.IncomingOptions.ChargesLosses:
// keploy record, keploy mock record), it is streamed with the span as its
// evidence. Which it is depends on the record streams open on the manager
// the span is recorded on, so a line asks (LossFate) rather than read the
// switch alone.

// OpenRecordStream notes a record stream open on m from now until the closer
// it returns is called, charging what its test cases lost or not (charges):
// LossFate reads them. The closer is idempotent. Nil-safe.
func (m *SyncMockManager) OpenRecordStream(charges bool) (closeStream func()) {
	if m == nil {
		return func() {}
	}
	n := &m.streamsPlain
	if charges {
		n = &m.streamsCharging
	}
	n.Add(1)
	var once sync.Once
	return func() { once.Do(func() { n.Add(-1) }) }
}

// LossFate is what becomes of the test cases recorded over a span recorded on
// m now, as the record streams open on it say (OpenRecordStream):
// models.LossFateKept when every one charges what its test cases lost,
// models.LossFateMixed when some do, and models.LossFateLeftOut otherwise,
// always with the proven switch off. Nil-safe: a nil manager has no stream.
func (m *SyncMockManager) LossFate() models.LossFate {
	if m == nil || !provenswitch.On() {
		return models.LossFateLeftOut
	}
	charging, plain := m.streamsCharging.Load(), m.streamsPlain.Load()
	switch {
	case charging > 0 && plain > 0:
		return models.LossFateMixed
	case charging > 0:
		return models.LossFateKept
	}
	return models.LossFateLeftOut
}
