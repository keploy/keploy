package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Count-free reads (under the proven switch). Exact serving (S12) answers each
// recorded call of a test once, and the test's end fails it for a call made
// more often than recorded and for a recorded call it never made. A browser
// page makes its reads as timing allows: a poll fires once more when a test
// waits longer, a refetch is made again when answers come in another order,
// and fewer of either when the page closes sooner. Those counts are not what
// the application does, so, for a test process whose claim says it drives a
// browser page (models.InstanceReq.Browser), the end does not count two kinds
// of them, both reads only (models.IsReadCall, UnmatchedCall.Read: the parser
// decides, from the protocol); every write, and every call of any other test
// process (a Go test's calls follow its code, call for call), is judged as
// before:
//
//   - one read more than recorded (models.MissExtraCall with Read), in a test
//     whose own checks passed: the matcher found the call identical to one the
//     test's own recordings answered, and keploy refused it as before (nothing
//     is served twice). The test's checks passed without its answer.
//   - a recorded read the test never made, when the test made the same read
//     (method, URL and body as recorded) and another of its recordings
//     answered it the same (status, body and Set-Cookie): the application got
//     exactly that answer; only how often it asked differs. A read the test
//     never made at all, or one whose answer none of its answered recordings
//     gave, still fails it.
//
// What is not counted is said on the test's window (ScopeEvidence.CountFree)
// and in the agent's log, never dropped silently.

// countFree takes out of ev, the evidence of the test id that exact serving
// has stopped serving, the reads its end does not count (countFreeReads), and
// says them on ev and in the log. passed is the test's own outcome.
func (a *Agent) countFree(id string, ev *models.ScopeEvidence, passed bool, beganAt time.Time) {
	if ev == nil || (len(ev.Misses) == 0 && len(ev.Unconsumed) == 0) {
		return
	}
	// The test's answered recordings, from the view it was served by: read
	// once it stopped being served, the read is final (ExactTestOutcome).
	var answered []string
	if es, err := a.exactServer(); err == nil {
		if test, consumed, _, ok := es.ExactTestOutcome(); ok && test == id {
			answered = consumed
		}
	}
	var mocks map[string]*models.Mock
	if len(ev.Unconsumed) > 0 && len(answered) > 0 {
		mocks = a.mocksFound(append(append([]string(nil), ev.Unconsumed...), answered...))
	}
	notes := countFreeReads(ev, passed, beganAt, mocks, answered)
	if len(notes) == 0 {
		return
	}
	ev.CountFree = append(ev.CountFree, notes...)
	a.logger.Info("keploy did not count reads the test made more or fewer times than recorded: the reads are the same, only how often the page asked differs",
		zap.String("test", id), zap.Strings("reads", notes))
}

// countFreeReads takes out of ev, a replayed test's evidence, the reads its
// end does not count, and returns them, said. passed is the test's own
// outcome; beganAt is when exact serving began serving the test (a miss made
// before it is no call of the test's, evidenceReason); answered names the
// test's recordings that answered; mocks holds, as loaded, the recordings
// ev.Unconsumed and answered name (one it does not hold is counted).
func countFreeReads(ev *models.ScopeEvidence, passed bool, beganAt time.Time, mocks map[string]*models.Mock, answered []string) []string {
	if ev == nil {
		return nil
	}
	var notes []string
	if passed {
		extra, recorded := map[string]int{}, map[string]int{}
		var calls []string
		var kept []models.UnmatchedCall
		for _, m := range ev.Misses {
			before := !beganAt.IsZero() && !m.At.IsZero() && m.At.Before(beganAt)
			if m.Kind != models.MissExtraCall || !m.Read || before {
				kept = append(kept, m)
				continue
			}
			call := missText(m)
			if _, seen := extra[call]; !seen {
				calls = append(calls, call)
			}
			extra[call]++
			recorded[call] = m.Recorded
		}
		for _, call := range calls {
			notes = append(notes, fmt.Sprintf("%s was called %d times, recorded %d: a read the page made more often, not counted",
				call, recorded[call]+extra[call], recorded[call]))
		}
		ev.Misses = kept
	}
	var twins []*models.Mock
	for _, n := range answered {
		if m := mocks[n]; models.IsReadCall(m) {
			twins = append(twins, m)
		}
	}
	var unmade []string
	for _, n := range ev.Unconsumed {
		m := mocks[n]
		twin := sameRead(m, twins)
		if twin == "" {
			unmade = append(unmade, n)
			continue
		}
		notes = append(notes, fmt.Sprintf("recorded call %s was never made, but the test made the same read and %s answered it the same: not counted",
			mockText(n, m), twin))
	}
	ev.Unconsumed = unmade
	return notes
}

// sameRead is the name of the read in twins that made the call m recorded and
// was answered as m was: "" when m is no read, or none of twins is that read.
func sameRead(m *models.Mock, twins []*models.Mock) string {
	if !models.IsReadCall(m) || m.Kind != models.HTTP || m.Spec.HTTPReq == nil {
		return ""
	}
	want, err := recordedAnswer(m)
	if err != nil {
		return ""
	}
	for _, t := range twins {
		if t == nil || t.Name == m.Name || t.Kind != models.HTTP || t.Spec.HTTPReq == nil || !sameRequest(m.Spec.HTTPReq, t.Spec.HTTPReq) {
			continue
		}
		got, err := recordedAnswer(t)
		if err == nil && sameAnswer(want, got) {
			return t.Name
		}
	}
	return ""
}

// recordedAnswer is m's recorded HTTP answer, loaded on a copy when the store
// keeps it apart from the mock (models.Mock.HasSpilledResponse).
func recordedAnswer(m *models.Mock) (*models.HTTPResp, error) {
	if m.HasSpilledResponse() {
		c := m.ShallowCopy()
		if err := c.HydrateResponse(); err != nil {
			return nil, err
		}
		m = c
	}
	if m.Spec.HTTPResp == nil {
		return nil, errors.New("no recorded answer")
	}
	return m.Spec.HTTPResp, nil
}

// sameRequest reports whether a and b recorded the same request: method, URL
// and body, each as recorded. A body kept apart from either (BodyRef) cannot
// be compared, and is not the same.
func sameRequest(a, b *models.HTTPReq) bool {
	return a.Method == b.Method && a.URL == b.URL && a.Body == b.Body && a.Binary == b.Binary &&
		a.BodyRef == (models.BodyRef{}) && b.BodyRef == (models.BodyRef{}) && len(a.Form) == 0 && len(b.Form) == 0
}

// sameAnswer reports whether a and b are the same answer: status, body and
// the cookies it set. A body that was not saved cannot be compared, and is
// not the same.
func sameAnswer(a, b *models.HTTPResp) bool {
	return a.StatusCode == b.StatusCode && a.Body == b.Body && a.Binary == b.Binary && !a.BodySkipped && !b.BodySkipped &&
		setCookie(a.Header) == setCookie(b.Header)
}

// setCookie is the Set-Cookie value of h, whatever case its name was recorded in.
func setCookie(h map[string]string) string {
	for k, v := range h {
		if strings.EqualFold(k, "Set-Cookie") {
			return v
		}
	}
	return ""
}
