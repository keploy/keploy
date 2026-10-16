package replay

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// markUnmatchedCallsUnread records on a test's result that its unmatched calls
// could not all be read.
func markUnmatchedCallsUnread(result *models.TestResult) {
	if result != nil {
		result.FailureInfo.UnmatchedCallsUnread = true
	}
}

// testsWithUnreadMisses names the tests of a set whose unmatched calls could
// not all be read (FailureInfo.UnmatchedCallsUnread), in result order. Under
// the proven switch a miss fails its test, so such a test has no verdict and
// neither has its set: RunTestSet reports the set INTERNAL_ERR, which is
// neither PASSED nor FAILED, and nothing reading the report can take it for
// either. With the switch off it is always nil.
func testsWithUnreadMisses(results []models.TestResult) []string {
	if !provenswitch.On() {
		return nil
	}
	var out []string
	for _, r := range results {
		if r.FailureInfo.UnmatchedCallsUnread {
			out = append(out, r.TestCaseID)
		}
	}
	return out
}

// unreadMissesReason is the test-set report's reason for a set left unjudged
// by unread misses.
func unreadMissesReason(tests []string) string {
	return fmt.Sprintf("not judged: the calls that %d test(s) made with no recorded answer could not all be read from keploy's agent (%s); check the agent's log for why, then run the test-set again",
		len(tests), strings.Join(tests, ", "))
}

// markNotJudged records that testSetID ended not judged: its INTERNAL_ERR
// fails the set and the run, and does not end the round (abortAfterTestSet).
func (r *Replayer) markNotJudged(testSetID string) {
	r.notJudgedMu.Lock()
	defer r.notJudgedMu.Unlock()
	if r.notJudgedSets == nil {
		r.notJudgedSets = map[string]bool{}
	}
	r.notJudgedSets[testSetID] = true
}

// notJudged reports whether testSetID ended not judged in this run.
func (r *Replayer) notJudged(testSetID string) bool {
	r.notJudgedMu.Lock()
	defer r.notJudgedMu.Unlock()
	return r.notJudgedSets[testSetID]
}

// markNeverReady records that testSetID was not judged because its
// application never got ready (proven switch): it ran no test.
func (r *Replayer) markNeverReady(testSetID string) {
	r.notJudgedMu.Lock()
	defer r.notJudgedMu.Unlock()
	if r.neverReadySets == nil {
		r.neverReadySets = map[string]bool{}
	}
	r.neverReadySets[testSetID] = true
}

// neverReady reports whether testSetID was not judged in this run because
// its application never got ready.
func (r *Replayer) neverReady(testSetID string) bool {
	r.notJudgedMu.Lock()
	defer r.notJudgedMu.Unlock()
	return r.neverReadySets[testSetID]
}

// saysResultsUnread says whether a set's results that could not be read are
// an error to say: not for a set with no tests to run, nor for one whose
// application never got ready (it said why already, and ran no test); for
// any other, one not judged for another reason too.
func (r *Replayer) saysResultsUnread(status models.TestSetStatus, testSetID string) bool {
	return status != models.TestSetStatusNoTestsToRun && !r.neverReady(testSetID)
}

// forgetNotJudged clears testSetID's marks, as a run of the set starts.
func (r *Replayer) forgetNotJudged(testSetID string) {
	r.notJudgedMu.Lock()
	defer r.notJudgedMu.Unlock()
	delete(r.notJudgedSets, testSetID)
	delete(r.neverReadySets, testSetID)
}

// testsWithUnreadCapture names the tests of a set whose capture could not be
// told (FailureInfo.CaptureUnread: keploy could not read its agent's capture-
// loss counts as the test ended, or at its set's mark), in result order. Under
// the proven switch such a test has no verdict and neither has its set. With
// the switch off it is always nil.
func testsWithUnreadCapture(results []models.TestResult) []string {
	if !provenswitch.On() {
		return nil
	}
	var out []string
	for _, r := range results {
		if r.FailureInfo.CaptureUnread {
			out = append(out, r.TestCaseID)
		}
	}
	return out
}

// unreadCaptureReason is the test-set report's reason for a set left unjudged
// by tests whose capture could not be told.
func unreadCaptureReason(tests []string) string {
	return fmt.Sprintf("not judged: keploy could not read whether its agent captured every call while %d test(s) ran (%s); check the agent's log for why its count could not be read, then run the test-set again",
		len(tests), strings.Join(tests, ", "))
}

// testsWithStreamsHeldOpen names, under the proven switch, the tests of a set
// whose app kept its stream open on keepalives, past the recording or short
// of it (FailureInfo.StreamHeldOpen, row W1), and that did not fail on what
// came: they have no verdict, and the set is not judged. One whose frames
// that came differed from the recording keeps its failure.
func testsWithStreamsHeldOpen(results []models.TestResult) []string {
	if !provenswitch.On() {
		return nil
	}
	var out []string
	for _, r := range results {
		if r.FailureInfo.StreamHeldOpen != "" && len(r.VerdictUnknown()) > 0 {
			out = append(out, r.TestCaseID)
		}
	}
	return out
}

// streamsHeldOpenReason is the test-set report's reason for a set left
// unjudged by tests whose app kept its stream open on keepalives, past the
// recording or short of it (pkg.ErrStreamStalled).
func streamsHeldOpenReason(tests []string) string {
	return fmt.Sprintf("not judged: the app kept its stream open on keepalives in %d test(s) (%s), past the end of its recording, or with the rest of its recorded answer not coming, and did not end it; %s, then run the test-set again",
		len(tests), strings.Join(tests, ", "), streamsHeldOpenRemedy)
}

// streamsHeldOpenRemedy is what to do about a stream the app held open.
const streamsHeldOpenRemedy = "check why the app keeps the stream open after its recorded answer, or why the rest of its answer does not come, or record the test again"

// markAnswerStalled marks result, a test with no answer, not judged when its
// exchange ended with nothing of the answer to compare (pkg.ErrAnswerStalled,
// rule 15: the stall rule ended it, or the app's keepalives, before the
// answer or any of its stream came): nothing was compared, so its FAILED
// says only that no answer came.
func markAnswerStalled(result *models.TestResult, err error) {
	if result != nil && errors.Is(err, pkg.ErrAnswerStalled) {
		result.FailureInfo.AnswerStalled = err.Error()
	}
}

// sayNoAnswer is the console's line for a test with no answer: for one the
// stall rule or the app's keepalives ended, that it is not judged and what
// was waited for, not an ordinary failure; else what as says, with the
// error.
func sayNoAnswer(logger *zap.Logger, test string, err error, as string) {
	if errors.Is(err, pkg.ErrAnswerStalled) {
		logger.Error("not judged: "+err.Error(),
			zap.String("testcase", test),
			zap.String("next_step", stalledAnswerRemedy(err)))
		return
	}
	utils.LogError(logger, err, as)
}

// sayStartJudged is the console's line for a test whose answer began and then
// sent nothing of its body, judged on its start (startFailedRow): that it
// failed on its start, and that its body never came. The start's diff is
// printed with it.
func sayStartJudged(logger *zap.Logger, test string, err error) {
	logger.Error("failed on its answer's start, which differs from the recording; nothing whole of its body came: "+err.Error(),
		zap.String("testcase", test))
}

// stalledAnswerRemedy is what to do about a test whose answer never came:
// for one the app's keepalives ended (pkg.AnswerStallError.Keepalives), a
// bound of the user's changes nothing, so it is never suggested.
func stalledAnswerRemedy(err error) string {
	var st *pkg.AnswerStallError
	if errors.As(err, &st) && st.Keepalives {
		return keepalivesRemedy
	}
	return stalledAnswersRemedy
}

// keepalivesRemedy is what to do about a stream the app sent only
// keepalives on: no bound changes it.
const keepalivesRemedy = "find why the app sends only keepalives and nothing of its answer (a stream it waits to fill, an event that never comes at replay), or record the test again; --api-timeout does not change this"

// startFailedRow judges the start of an answer that began and then sent
// nothing of its body (pkg.AnswerBegan): its status and headers (gRPC: its
// metadata) against view, the test's expected answer as the replay renders
// it. The recording's body (gRPC: its messages and trailers) stands in for
// the one that never came, so only the start is judged. An expected header
// that holds template markup is the app's to fill, and is taken as the app
// sent it.
//
// The judgement is apart from the replay's template and capture path: what
// came feeds no capture, asserts no use, and moves no template value. When
// the start differs it returns the test's FAILED row, judged on the start:
// the comparison's result, printed, and as its answer what came (no body).
// When it matches, nil and false: the test fails closed, not judged.
func (r *Replayer) startFailedRow(run *templates.Run, sent, view *models.TestCase, resp any, testSetID string, started time.Time) (*models.TestResult, bool) {
	row := func(result *models.Result) *models.TestResult {
		tr := &models.TestResult{
			Kind:         view.Kind,
			Name:         testSetID,
			Status:       models.TestStatusFailed,
			Started:      started.Unix(),
			Completed:    time.Now().UTC().Unix(),
			TestCaseID:   view.Name,
			TestCasePath: filepath.Join(r.config.Path, testSetID),
			MockPath:     filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
			Noise:        view.Noise,
			TimeTaken:    time.Since(started).String(),
		}
		if result != nil {
			tr.Result = *result
			if result.FailureInfo.Risk != models.None {
				tr.FailureInfo.Risk, tr.FailureInfo.Category = result.FailureInfo.Risk, result.FailureInfo.Category
			}
			tr.FailureInfo.Assessment = result.FailureInfo.Assessment
		}
		return tr
	}
	switch got := resp.(type) {
	case *models.HTTPResp:
		if got == nil {
			return nil, false
		}
		exp := *view
		exp.HTTPResp.Header = r.startHeaders(run, sent, view, got)
		start := models.HTTPResp{StatusCode: got.StatusCode, StatusMessage: got.StatusMessage, Header: got.Header, Body: exp.HTTPResp.Body}
		probe := start
		if same, _ := r.compareHTTPResp(&exp, &probe, testSetID, false); same {
			return nil, false
		}
		_, result := r.compareHTTPResp(&exp, &start, testSetID, true)
		// The body was not compared: what came of it is the row's, in
		// place of the stand-in the comparison was given.
		for i := range result.BodyResult {
			result.BodyResult[i].Actual = got.Body
		}
		tr := row(result)
		tr.Req = view.HTTPReq
		tr.Res = models.HTTPResp{StatusCode: got.StatusCode, StatusMessage: got.StatusMessage, Header: got.Header, Body: got.Body}
		return tr, true
	case *models.GrpcResp:
		if got == nil {
			return nil, false
		}
		start := view.GrpcResp
		start.Headers = got.Headers
		probe := start
		if same, _ := r.compareGRPCResp(view, &probe, testSetID, false, true); same {
			return nil, false
		}
		_, result := r.compareGRPCResp(view, &start, testSetID, true, false)
		// No message came: the rows say so, not the stand-in's.
		for i := range result.BodyResult {
			result.BodyResult[i].Actual = ""
			if result.BodyResult[i].Type == models.GrpcLength {
				result.BodyResult[i].Actual = "0"
			}
		}
		tr := row(result)
		tr.GrpcReq = view.GrpcReq
		tr.GrpcRes = models.GrpcResp{Headers: got.Headers}
		return tr, true
	}
	return nil, false
}

// testsWithStalledAnswers names, under the proven switch, the tests of a set
// whose exchange ended with nothing of their answer to compare
// (FailureInfo.AnswerStalled): they have no verdict, and the set is not
// judged.
func testsWithStalledAnswers(results []models.TestResult) []string {
	if !provenswitch.On() {
		return nil
	}
	var out []string
	for _, r := range results {
		if r.FailureInfo.AnswerStalled != "" {
			out = append(out, r.TestCaseID)
		}
	}
	return out
}

// startHeaders is the expected answer's headers to judge a start against:
// view's, the replay's no-answer view, with each header that holds a
// placeholder in the recording resolved as the main path resolves it
// (templates.Run.ExpectedStart), against the headers that came, on a copy of
// this run, so the run itself moves no value: its captures take the app's
// text where the literal text and uses around them match (a cookie by its
// parts, its Expires read as the template package reads it), then its uses
// take each key's value after them. A header that uses a key the body
// captures is taken as the app sent it, its value being in the body that
// never came. One the answer does not satisfy keeps its markup, and the
// comparison calls it a difference. In a set with no template, headers are
// compared as written.
func (r *Replayer) startHeaders(run *templates.Run, sent, view *models.TestCase, got *models.HTTPResp) map[string]string {
	out := make(map[string]string, len(view.HTTPResp.Header))
	for k, v := range view.HTTPResp.Header {
		out[k] = v
	}
	if run == nil || sent == nil {
		return out
	}
	for k, v := range run.ExpectedStart(sent.HTTPResp, models.HTTPResp{StatusCode: got.StatusCode, Header: got.Header}, templates.ExpectOptions{IgnoreOrdering: r.responseIgnoreOrdering()}) {
		if _, ok := out[k]; ok {
			out[k] = v
		}
	}
	return out
}

// stalledAnswersReason is the test-set report's reason for a set left
// unjudged by tests whose answer never came.
func stalledAnswersReason(tests []string) string {
	return fmt.Sprintf("not judged: the app never answered %d test(s) (%s), sent nothing of the answer past its headers, or never took the whole request, before keploy's stall bound of runnable time passed or its keepalives showed it stalled; %s (a stream it sent only keepalives on: --api-timeout does not change that), then run the test-set again",
		len(tests), strings.Join(tests, ", "), stalledAnswersRemedy)
}

// stalledAnswersRemedy is what to do about a test whose answer never came,
// by keploy's stall bound.
const stalledAnswersRemedy = "find why the app does not answer, or does not read the request; if it takes longer than that by design, give the tests a bound of your own with --api-timeout"

// testsKeployCannotCapture names, under the proven switch, the tests of a set
// during or after which the application called what keploy cannot capture at
// all (FailureInfo.CannotCapture, decision 20: a Unix socket), or during which
// a client abandoned its TLS handshake without a word, and says what: they
// have no verdict, and the set is not judged. Never a failure.
func testsKeployCannotCapture(results []models.TestResult) ([]string, models.CaptureLosses) {
	if !provenswitch.On() {
		return nil, nil
	}
	var out []string
	var standing, own models.CaptureLosses
	for _, r := range results {
		if len(r.FailureInfo.CannotCapture) == 0 {
			continue
		}
		out = append(out, r.TestCaseID)
		var latest models.CaptureLosses
		for _, c := range r.FailureInfo.CannotCapture {
			if c.Kind.PerTest() {
				// A test's own (an abandoned TLS handshake): each
				// test's, so the set's is their sum.
				own = own.Plus(models.CaptureLosses{c})
			} else {
				latest = append(latest, c)
			}
		}
		if len(latest) > 0 {
			standing = latest // the latest test's: these counts only grow
		}
	}
	return out, standing.Plus(own)
}

// notCapturableReason is the test-set report's reason for a set left unjudged
// by tests whose calls keploy cannot capture, or cannot tell it captured (an
// abandoned TLS handshake), or cannot tell which container they reached.
func notCapturableReason(tests []string, what string) string {
	return fmt.Sprintf("not judged: %d test(s) ran while keploy could not capture, or could not tell it captured, every call the application made (%s): %s",
		len(tests), strings.Join(tests, ", "), strings.TrimPrefix(what, "not judged: "))
}
