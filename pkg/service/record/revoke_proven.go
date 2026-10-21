package record

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

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
	// mockNotSavedWhileItRan: keploy record could not save a mock recorded
	// while the test case ran, in a recording whose mocks no mapping names
	// the owner of (one made without --sync): each test case that ran then
	// is charged (unsavedMocks).
	mockNotSavedWhileItRan = "keploy record could not save a mock recorded while it ran"
	// mocksUncorrelated: mocks the test case's mapping names never reached
	// keploy record (a short mock pool: chargeShortPools).
	mocksUncorrelated = "mocks its mapping names never reached keploy record"
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
	r.logger.Warn("some test cases lack a mock of their own that never reached the mock file: each is kept with that as its evidence: "+judgedOrFailed,
		zap.String("testSetID", testSetID),
		zap.Strings("testCases", names))
	return nil
}

// streamedWindow is a test case's window as the agent streamed it
// (models.LateLoss.Request and Response), as keploy record received it.
type streamedWindow struct{ req, resp int64 }

// windowOf is tc's window as the agent streamed it: its request's and its
// response's times, a gRPC call's for one (the agent's testCaseWindow), an
// HTTP one's otherwise; the zero window for a test case with no request time
// (one the agent checks nothing for).
func windowOf(tc *models.TestCase) streamedWindow {
	if tc == nil {
		return streamedWindow{}
	}
	req, resp := tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp
	if tc.Kind == models.GRPC_EXPORT {
		req, resp = tc.GrpcReq.Timestamp, tc.GrpcResp.Timestamp
	}
	if req.IsZero() {
		return streamedWindow{}
	}
	return streamedWindow{req: req.UnixNano(), resp: resp.UnixNano()}
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
// record saved with what the agent found it had lost after all, by the
// stream's end (late, takeStreamEnds): a span a parser reported behind the
// traffic overlaps it, after it was streamed with nothing lost. saved names
// the test cases saved that a late loss names (savedTestCases.savedFor); one
// keploy record did not save is charged nothing. Without this such a test
// case was saved without that as its evidence. Called once the frame streams
// drained.
func chargeLateLosses(w *captureWatch, late []models.LateLoss, saved func(models.LateLoss) []string) {
	if !provenswitch.On() {
		return
	}
	for _, l := range late {
		for _, name := range saved(l) {
			w.charge(name, l.Lost)
		}
	}
}

// unencodableMockMsg is the line keploy record says a mock it cannot encode
// with: with the proven switch off the test case that owns it is revoked
// (deleted at the recording's end); under it, kept with the loss as its
// evidence: the test case its mapping names (keepRevoked), or, in a recording
// whose mocks no mapping names the owner of, each test case recorded while
// the mock was (unsavedMocks).
func unencodableMockMsg() string {
	if provenswitch.On() {
		return "dropping ONE unencodable mock and CONTINUING to record; the test that owns it (each test recorded while it was, where no mapping names its owner) is kept with the mock it lacks as its evidence: " + judgedOrFailed
	}
	return "dropping ONE unencodable mock and CONTINUING to record; the test that owns it is revoked, not recorded short"
}

// mocksDroppedMsg is the line keploy record ends a recording that dropped
// mocks with (unencodableMockMsg).
func mocksDroppedMsg() string {
	if provenswitch.On() {
		return "recording finished with mocks dropped; the tests that owned them (each test recorded while one was, where no mapping names its owner) are kept with what they lack as their evidence: " + judgedOrFailed
	}
	return "recording finished with mocks dropped; the tests that owned them were revoked rather than recorded with a short mock pool"
}

// judgedOrFailed is what becomes of a test case kept with a loss as its
// evidence, under the proven switch: the auto-replay judges it where one runs
// (enterprise's), and without one the recording fails naming it
// (models.KeptToBeJudged).
const judgedOrFailed = "the auto-replay judges it where one follows the record (kept if it replays, quarantined with that in its reason if it does not), and without one the recording fails, naming it (" + provenswitch.Env + "=1)"

// maxUnsavedMockSpans bounds how many unsaved mocks' spans a recording keeps
// apart (unsavedMocks): past it, the rest are kept as one span over them all,
// which only charges more test cases, never fewer.
const maxUnsavedMockSpans = 1 << 16

// unsavedMocks is, under the proven switch, the span of each mock keploy
// record could not save ([its request, its response]): in a recording whose
// mocks no mapping names the owner of (one made without --sync, with no agent
// that maps it itself), every test case saved whose window overlaps one is
// charged with it (chargeUnsaved), as no mapping can say which test case made
// the call. A recording whose mappings name the owners charges the owner by
// its mapping instead (keepRevoked). Nil with the switch off, which collects
// nothing. Safe for concurrent use.
type unsavedMocks struct {
	mu    sync.Mutex
	spans []streamedWindow
	// widest is the widest span kept: no span that starts more than that
	// before a window does overlaps it.
	widest int64
	// over is the one span over every mock past maxUnsavedMockSpans; zero
	// while there is none.
	over streamedWindow
}

// newUnsavedMocks is a recording's collector of the mocks it could not save:
// nil with the proven switch off.
func newUnsavedMocks() *unsavedMocks {
	if !provenswitch.On() {
		return nil
	}
	return &unsavedMocks{}
}

// add notes mock m unsaved. A mock with no request time is in no window.
func (u *unsavedMocks) add(m *models.Mock) {
	if u == nil || m == nil || m.Spec.ReqTimestampMock.IsZero() {
		return
	}
	s := streamedWindow{req: m.Spec.ReqTimestampMock.UnixNano(), resp: m.Spec.ResTimestampMock.UnixNano()}
	if s.resp < s.req {
		s.resp = s.req
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.spans) >= maxUnsavedMockSpans {
		if u.over == (streamedWindow{}) {
			u.over = s
		}
		u.over.req, u.over.resp = min(u.over.req, s.req), max(u.over.resp, s.resp)
		return
	}
	u.spans = append(u.spans, s)
	u.widest = max(u.widest, s.resp-s.req)
}

// overlapping is how many unsaved mocks' spans overlap window w ([a, b] and
// [c, d] overlap iff a <= d and c <= b). spans must be sorted by start.
func (u *unsavedMocks) overlapping(spans []streamedWindow, w streamedWindow) int {
	end := max(w.resp, w.req)
	n := 0
	hi := sort.Search(len(spans), func(k int) bool { return spans[k].req > end })
	for i := hi - 1; i >= 0 && spans[i].req >= w.req-u.widest; i-- {
		if spans[i].resp >= w.req {
			n++
		}
	}
	if u.over != (streamedWindow{}) && u.over.req <= end && w.req <= u.over.resp {
		n++
	}
	return n
}

// chargeUnsaved charges, under the proven switch, every test case keploy
// record saved (saved: the saved test cases by the window each was streamed
// with) whose window overlaps a mock it could not save, with that mock
// (models.CaptureLossMockDropped, mockNotSavedWhileItRan), once the frame
// streams drained. Only for a recording whose mocks no mapping named the
// owner of (mapped false): one whose mappings did charges the owner by its
// mapping (keepRevoked). Without this a recording made without --sync saved
// such a test case with nothing said of the mock it lacks.
func (r *Recorder) chargeUnsaved(w *captureWatch, u *unsavedMocks, saved savedTestCases, mapped bool) {
	if u == nil || mapped || !provenswitch.On() {
		return
	}
	u.mu.Lock()
	spans := slices.Clone(u.spans)
	u.mu.Unlock()
	if len(spans) == 0 && u.over == (streamedWindow{}) {
		return
	}
	slices.SortFunc(spans, func(a, b streamedWindow) int {
		switch {
		case a.req < b.req:
			return -1
		case a.req > b.req:
			return 1
		}
		return 0
	})
	var charged []string
	for win, names := range saved {
		n := u.overlapping(spans, win)
		if n == 0 {
			continue
		}
		for _, name := range names {
			w.charge(name, models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: uint64(n), What: mockNotSavedWhileItRan}})
			charged = append(charged, name)
		}
	}
	if len(charged) > 0 {
		slices.Sort(charged)
		r.logger.Warn("keploy record could not save some mocks recorded while these test cases ran, and no mapping names which test case each was of: each is kept with that as its evidence: "+judgedOrFailed,
			zap.Strings("testCases", charged))
	}
}

// chargeShortPools charges, under the proven switch, each test case still in
// the set whose mapping named mocks that never reached keploy record (a short
// mock pool: shortPool, by test case, how many), with them
// (models.CaptureLossMockDropped, mocksUncorrelated), once the mappings
// drained. With the switch off they are only counted, at WARN.
func chargeShortPools(w *captureWatch, shortPool *sync.Map, deleted map[string]struct{}) {
	if !provenswitch.On() {
		return
	}
	shortPool.Range(func(k, v any) bool {
		name, _ := k.(string)
		n, _ := v.(int64)
		if _, gone := deleted[name]; name != "" && n > 0 && !gone {
			w.charge(name, models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: uint64(n), What: mocksUncorrelated}})
		}
		return true
	})
}

// streamEndTaker is an Instrumentation that says what its incoming streams'
// end markers said (models.StreamEndSummary), added up over the streams: the
// keploy agent client.
type streamEndTaker interface {
	TakeStreamEnds() models.StreamEndSummary
}

// leftOutUnsaid is, under the proven switch, what the agent's incoming
// streams' end markers (sum) said it left out of the recording: test cases it
// suppressed (an agent that does not stream them with their loss to keploy
// record, one older than keploy record, or one keploy record could not ask:
// tcs_suppressed_total), and test cases left out because each stayed in
// flight while mocks it may have made had to be let go
// (tcs_left_out_in_flight). Neither has a name keploy record could charge:
// what the recording lost of the set, said in a sentence; "" for none.
func leftOutUnsaid(sum models.StreamEndSummary) string {
	var parts []string
	if sum.TestCasesSuppressed > 0 {
		parts = append(parts, fmt.Sprintf("the keploy agent left %d test case(s) out of the recording, unnamed, for a call of theirs it could not record (tcs_suppressed_total: an agent that does not stream such a test case with its loss)", sum.TestCasesSuppressed))
	}
	if sum.LeftOutInFlight > 0 {
		parts = append(parts, fmt.Sprintf("the keploy agent left %d test case(s) out of the recording, unnamed: each stayed in flight while mocks it may have made had to be let go (tcs_left_out_in_flight)", sum.LeftOutInFlight))
	}
	return strings.Join(parts, "; ")
}

// takeStreamEnds notes on w, under the proven switch, what the agent's
// incoming streams' end markers said it left out of the recording unnamed
// (leftOutUnsaid), and is the late losses they carried, for
// chargeLateLosses. Called once the frame streams drained.
func (r *Recorder) takeStreamEnds(w *captureWatch) []models.LateLoss {
	t, ok := r.instrumentation.(streamEndTaker)
	if !ok || !provenswitch.On() {
		return nil
	}
	sum := t.TakeStreamEnds()
	w.leftOutOfSet(leftOutUnsaid(sum))
	return sum.LateLosses
}
