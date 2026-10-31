package manager

import "go.uber.org/zap"

// notHandedOnWarning is what the manager says, as they come, of the requests
// an ingress served and recorded whose test cases never reached the recording
// (NoteNotHandedOn).
const notHandedOnWarning = "syncMock: a request the app served was not recorded: the ingress that served it stopped (the agent, or its forwarder for the app's port, was stopping) before the request's test case reached the recording"

// NoteNotHandedOn counts a request an ingress served and was recording whose
// test case never reached the recording's queue: the context its capture runs
// on ended first, as the ingress that served it stopped (its forwarder for the
// app's port, or the agent, was stopping) while the capture waited for a slot,
// built the test case, or waited for the queue. The app answered the request;
// the recording lacks it. The manager warns of the first and of every
// sendDropSampleRate-th as they come, and the recording's summary reports the
// session's count (routes/record.go), so a recording that lost test cases so
// says how many rather than ending short without a word. A recording's stop
// ends no such context: it waits for each test case being built from before
// it (BuildingTestCase, noted by the HTTP/1 ingress before its client can
// have the whole answer), and a stop whose wait makes no progress gives up,
// says so and fails the recording (the CLI's drain). Nil-safe.
func (m *SyncMockManager) NoteNotHandedOn() {
	if m == nil {
		return
	}
	n := m.notHandedOn.Add(1)
	if n == 1 || n%sendDropSampleRate == 0 {
		m.dropLogger().Warn(notHandedOnWarning,
			zap.Uint64("not_handed_on_so_far", n))
	}
}

// NotHandedOn returns how many requests NoteNotHandedOn has counted over the
// manager's life. The count only grows, so what it grew by over a recording
// session is that session's: routes/record.go's summary reports that.
// Nil-safe.
func (m *SyncMockManager) NotHandedOn() uint64 {
	if m == nil {
		return 0
	}
	return m.notHandedOn.Load()
}
