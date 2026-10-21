package record

import (
	"slices"

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
	TakeLateTestCaseLosses() []models.LateLoss
}

// streamedWindow is a test case's window as the agent streamed it
// (models.LateLoss.Request and Response), as keploy record received it.
type streamedWindow struct{ req, resp int64 }

// windowOf is tc's window as the agent streamed it; the zero window for a
// test case with no request time (one the agent checks nothing for).
func windowOf(tc *models.TestCase) streamedWindow {
	if tc == nil || tc.HTTPReq.Timestamp.IsZero() {
		return streamedWindow{}
	}
	return streamedWindow{req: tc.HTTPReq.Timestamp.UnixNano(), resp: tc.HTTPResp.Timestamp.UnixNano()}
}

// savedTestCases is, under the proven switch, the test cases keploy record
// saved, by the window the agent streamed each with: the agent streams most
// test cases unnamed, and the name each is saved under is keploy record's own
// (InsertTestCase names it). A late loss finds its test case here
// (savedFor). Not safe for concurrent use: record.go guards it with
// insertedMu.
type savedTestCases map[streamedWindow][]string

// add notes test case name saved, streamed with window win.
func (s savedTestCases) add(win streamedWindow, name string) {
	if win == (streamedWindow{}) || name == "" {
		return
	}
	s[win] = append(s[win], name)
}

// savedFor is the names of the test cases keploy record saved that late loss
// l names: the ones streamed with its window, and the one by its name, when the
// agent named it and that name was saved (inserted). Two test cases streamed
// with the very same window are both charged: one of them is the one that
// lost, and a charge is evidence, which the replay that judges each decides.
func (s savedTestCases) savedFor(l models.LateLoss, inserted func(string) bool) []string {
	var names []string
	if !l.Request.IsZero() {
		names = append(names, s[streamedWindow{req: l.Request.UnixNano(), resp: l.Response.UnixNano()}]...)
	}
	if l.Name != "" && inserted(l.Name) && !slices.Contains(names, l.Name) {
		names = append(names, l.Name)
	}
	return names
}

// chargeLateLosses charges, under the proven switch, each test case keploy
// record saved with what the agent found at the stream's end it had lost after
// all: a span a parser reported behind the traffic overlaps it, after it was
// streamed with nothing lost. saved names the test cases saved that a late
// loss names (savedTestCases.savedFor); one keploy record did not save is
// charged nothing. Without this such a test case was saved without that as
// its evidence. Called once the frame streams drained.
func (r *Recorder) chargeLateLosses(w *captureWatch, saved func(models.LateLoss) []string) {
	t, ok := r.instrumentation.(lateLossTaker)
	if !ok || !provenswitch.On() {
		return
	}
	for _, l := range t.TakeLateTestCaseLosses() {
		for _, name := range saved(l) {
			w.charge(name, l.Lost)
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
