package record

import (
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// What a revoked test case lost (record.go's revokedWhy), as its
// models.CaptureLossMockDropped says under the proven switch.
const (
	// mockDroppedAfterStreamed: the agent dropped a mock of the test case's
	// on its way to keploy record after it had streamed the test case (its
	// queue was full or closed), and told keploy record so (a RevokedTests
	// control frame).
	mockDroppedAfterStreamed = "a mock of its own was dropped on its way to keploy record, its queue full or closed, after the test case was streamed"
	// mockNotSaved: keploy record could not save a mock of the test case's
	// own (one that does not encode, or a write that failed).
	mockNotSaved = "keploy record could not save a mock of its own"
)

// keepRevoked is, under the proven switch, what becomes of the test cases
// revoked for a mock of theirs that never reached the mock file (names, each
// saved in test set testSetID): each is kept, charged with what it lacks
// (models.CaptureLossMockDropped, why[name] saying which), so the replay that
// judges it keeps it if it replays and quarantines it with the loss in its
// reason if it does not. With the switch off such a test case is deleted, and
// the recording only counted it: Start calls this under the switch only. It
// returns the test cases still to delete: none.
func (r *Recorder) keepRevoked(w *captureWatch, testSetID string, names []string, why map[string]string) []string {
	for _, n := range names {
		what := why[n]
		if what == "" {
			what = mockNotSaved
		}
		// The agent revokes every test case a mock of which it dropped on
		// its way here, the ones it streamed with that loss already
		// (models.TestCase.CaptureLost) among them: one drop, charged once.
		if what == mockDroppedAfterStreamed && w.chargedWith(n, models.CaptureLossMockDropped) {
			continue
		}
		w.charge(n, models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: 1, What: what}})
	}
	r.logger.Warn("some test cases lack a mock of their own that never reached the mock file: they are kept, with that as their evidence, for the replay that judges them ("+provenswitch.Env+"=1)",
		zap.String("testSetID", testSetID),
		zap.Strings("testCases", names))
	return nil
}

// lateLossTaker is an Instrumentation that says what test cases the agent
// streamed with nothing lost had lost by the stream's end
// (models.StreamEndSummary.LateLosses): the keploy agent client.
type lateLossTaker interface {
	TakeLateTestCaseLosses() map[string]models.CaptureLosses
}

// chargeLateLosses charges, under the proven switch, each test case keploy
// record saved (inserted) with what the agent found at the stream's end it
// had lost after all: a span a parser reported behind the traffic overlaps
// it, after it was streamed with nothing lost. Without this it was saved
// without that as its evidence. Called once the frame streams drained.
func (r *Recorder) chargeLateLosses(w *captureWatch, inserted func(string) bool) {
	t, ok := r.instrumentation.(lateLossTaker)
	if !ok || !provenswitch.On() {
		return
	}
	for name, lost := range t.TakeLateTestCaseLosses() {
		if inserted(name) {
			w.charge(name, lost)
		}
	}
}

// unencodableMockMsg is the line keploy record says a mock it cannot encode
// with: with the proven switch off the test case that owns it is revoked
// (deleted at the recording's end); under it, kept with the loss as its
// evidence (keepRevoked).
func unencodableMockMsg() string {
	if provenswitch.On() {
		return "dropping ONE unencodable mock and CONTINUING to record; the test that owns it is kept, with the mock it lacks as its evidence, for the replay that judges it"
	}
	return "dropping ONE unencodable mock and CONTINUING to record; the test that owns it is revoked, not recorded short"
}

// mocksDroppedMsg is the line keploy record ends a recording that dropped
// mocks with (unencodableMockMsg).
func mocksDroppedMsg() string {
	if provenswitch.On() {
		return "recording finished with mocks dropped; the tests that owned them are kept, with what they lack as their evidence, for the replay that judges them"
	}
	return "recording finished with mocks dropped; the tests that owned them were revoked rather than recorded with a short mock pool"
}
