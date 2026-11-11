package replay

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The calls of an earlier test set that no mock answered, which keploy
// decided only after that set had ended (under the proven switch). keploy
// test serves each test case by name, so the agent knows the test a call was
// made in however late it decides the call. When that is after the set's end
// was read, no read of the set can take it any more: the agent keeps it for
// the next set's first read, naming its test
// (models.UnmatchedCall.EarlierSetTest) and the set
// (models.UnmatchedCall.EarlierSet), and so it keeps a call the set made
// while no test's view named one (between two of its tests, or after its
// last) that its reads would have returned at zero latency, naming the set
// alone. It is no call of the set that read it, nor of that set's tests, and
// the set it was made in has its verdict already: the run fails with it,
// naming the call, the set it was made in and its test, where it names one,
// and no set's verdict counts it. A call of no test is placed among that
// set's tests, in the attempt that made it, by its time (placeByTime):
// before its first test, between two of its tests, or after its last, as a
// set's own call is said.

// earlierSetCalls are the calls of earlier sets the run's reads returned
// (models.UnmatchedCall.EarlierSet, EarlierSetTest), with the set each was
// made in: the set the agent names, counted back from the one whose read
// returned it.
type earlierSetCalls struct {
	mu sync.Mutex
	// sets are the sets RunTestSet began, in order; running, the last, is
	// the one whose reads it takes now.
	sets    []string
	running string
	// spans are, by the index of the set in sets (an attempt of it), when
	// the run sent each of its tests' requests and read their answers
	// (noteSpan): what an earlier set's call of no test is placed by.
	spans []map[string]testSpan
	calls []earlierSetCall
}

// testSpan is when the run sent a test's request (began) and read the
// application's answer to it (ended), in an attempt of its set: the agent
// serves a test only from just before its request to its end read, so a call
// made while no test's view served falls outside every test's span.
type testSpan struct {
	began, ended time.Time
}

// earlierSetCall is a call of an earlier set, with the set it was made in
// ("" when the run began no such set).
type earlierSetCall struct {
	set  string
	call models.UnmatchedCall
}

// begin notes that the run now reads set's calls, at the set's staging
// (RunTestSet, once the agent took the staging of its mocks), where the agent
// counts the set too (its set start): an earlier set's call read from here
// was made in a set the run began before it (an earlier attempt of the same
// set, for a set run again), counted back as the agent counts, so a set that
// ends after its staging and before its first test is counted on both sides.
// A staging the agent did not take ends the run (Start), so no later set's
// read follows a count the two sides could disagree on.
func (e *earlierSetCalls) begin(set string) {
	e.mu.Lock()
	e.sets = append(e.sets, set)
	e.spans = append(e.spans, map[string]testSpan{})
	e.running = set
	e.mu.Unlock()
}

// noteSpan keeps span as test's in the attempt of set the run began last: a
// test of a set the run has not begun, or of another set than the one it
// began last, is noted nowhere, so no set's call is placed by another set's
// tests.
func (e *earlierSetCalls) noteSpan(set, test string, span testSpan) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i := len(e.sets) - 1; i >= 0 && e.sets[i] == set {
		e.spans[i][test] = span
	}
}

// madeIn is the attempt an earlier set's call c was made in, by its index in
// sets, and its set: the agent's count back from the running set
// (models.UnmatchedCall.EarlierSet; 1 from an agent that names only the
// test), -1 and "" when the run began no such set. Called under mu.
func (e *earlierSetCalls) madeIn(c models.UnmatchedCall) (int, string) {
	back := c.EarlierSet
	if back < 1 {
		back = 1
	}
	if i := len(e.sets) - 1 - back; i >= 0 {
		return i, e.sets[i]
	}
	return -1, ""
}

// take keeps the calls of read that an earlier set made
// (models.UnmatchedCall.EarlierSet, EarlierSetTest), and returns the others,
// and those it kept. One of no test is placed among the tests of the attempt
// that made it, by its time (placeByTime): MadeAfter names the test that
// ended last before it, MadeBefore the first that began after it, each ""
// where there is none, and both "" when keploy cannot place it.
func (e *earlierSetCalls) take(read []models.UnmatchedCall) (rest []models.UnmatchedCall, taken []earlierSetCall) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range read {
		if c.EarlierSet < 1 && c.EarlierSetTest == "" {
			rest = append(rest, c)
			continue
		}
		i, set := e.madeIn(c)
		if c.EarlierSetTest == "" {
			var spans map[string]testSpan
			if i >= 0 {
				spans = e.spans[i]
			}
			c.MadeAfter, c.MadeBefore = placeByTime(c.At, spans)
		}
		taken = append(taken, earlierSetCall{set: set, call: c})
	}
	if len(taken) == 0 {
		return read, nil
	}
	e.calls = append(e.calls, taken...)
	return rest, taken
}

// takeEarlierSetCalls is read without the calls an earlier set made
// (earlierSetCalls.take), which the run keeps to fail with. It says each one
// that fails the run, at ERROR, as it takes it, in the documented form of a
// call outside every test (outsideFields): named under the set it was made in
// and its test (none for a call made while no test's view named one), with
// the set whose read returned it, and tells the hooks each
// (EarlierSetCallHook). Hooks that say calls outside every test themselves
// (OutsideCallsLogger) are handed each, under the set it was made in, and
// the replay does not say it when the hooks said it.
func (r *Replayer) takeEarlierSetCalls(read []models.UnmatchedCall) []models.UnmatchedCall {
	rest, taken := r.earlierSetCalls.take(read)
	readIn := r.earlierSetCalls.reading()
	h, telling := r.hookImpl.(EarlierSetCallHook)
	sayer, saying := r.hookImpl.(OutsideCallsLogger)
	for _, c := range taken {
		if !c.call.CountsAgainstTest() {
			continue
		}
		said := false
		if saying {
			said = sayer.LogOutsideCalls(c.set, []models.UnmatchedCall{c.call}, r.recordAgainCommand(c.set))
		}
		if !said {
			r.logger.Error("test run failed: an earlier test set made a call that no mock answered, which keploy decided only after that set had ended; it is charged to no test and no set, so it fails the run ("+provenswitch.Env+"=1)",
				append(r.outsideFields(c.set, c.call.EarlierSetTest, c.call),
					zap.String("read-in-test-set", readIn))...)
		}
		if telling {
			h.EarlierSetCall(c.set, readIn, c.call)
		}
	}
	return rest
}

// reading is the set whose reads the run takes now.
func (e *earlierSetCalls) reading() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

// failing is the calls of earlier sets that fail the run
// (models.UnmatchedCall.CountsAgainstTest), each made in one of that set's
// tests or while no test's view named one, in the order they were read.
func (e *earlierSetCalls) failing() []earlierSetCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []earlierSetCall
	for _, c := range e.calls {
		if c.call.CountsAgainstTest() {
			out = append(out, c)
		}
	}
	return out
}

// reset forgets every call, the sets read and their tests' spans, for a new
// run.
func (e *earlierSetCalls) reset() {
	e.mu.Lock()
	e.sets, e.running, e.spans, e.calls = nil, "", nil, nil
	e.mu.Unlock()
}

// placeByTime places a call made at at among spans, the tests' spans of the
// attempt of a set that made it, by its time alone: after the test that ended
// last at or before it (after), before the first test that began after it
// (before), or both; "" names no test there. Equal times go to the first name
// in order. It cannot place the call (both "") when at is zero, spans is
// empty, or a test's span was open at at (it began at or before it and had
// not ended before it): keploy would guess the test the call was made in.
func placeByTime(at time.Time, spans map[string]testSpan) (after, before string) {
	if at.IsZero() {
		return "", ""
	}
	var ended, began time.Time
	for _, n := range slices.Sorted(maps.Keys(spans)) {
		s := spans[n]
		switch {
		case s.began.After(at):
			if before == "" || s.began.Before(began) {
				before, began = n, s.began
			}
		case !at.Before(s.ended):
			// ended starts zero, before every test's end.
			if s.ended.After(ended) {
				after, ended = n, s.ended
			}
		default:
			return "", ""
		}
	}
	return after, before
}

// earlierWhere says where c, a call of an earlier set, was made: in its test,
// where it names one, or where placeByTime placed it among that set's tests,
// or that keploy could not place it.
func earlierWhere(c earlierSetCall) string {
	set := c.set
	if set == "" {
		set = "an earlier test set"
	}
	m := c.call
	switch {
	case m.EarlierSetTest != "":
		return "made in " + m.EarlierSetTest + " of " + set
	case m.MadeAfter != "" && m.MadeBefore != "":
		return "made in no test of " + set + ", after test " + m.MadeAfter + " ended and before test " + m.MadeBefore + " began"
	case m.MadeBefore != "":
		return "made in no test of " + set + ", before its first test " + m.MadeBefore + " began"
	case m.MadeAfter != "":
		return "made in no test of " + set + ", after its last test " + m.MadeAfter + " ended"
	}
	return "made in no test of " + set + ", at a time keploy cannot place among its tests"
}

// earlierSetCallsWhy says why the calls fail the run, naming each call, the
// set it was made in and the test, where it names one, or where among that
// set's tests it was made (earlierWhere).
func earlierSetCallsWhy(calls []earlierSetCall) string {
	var named []string
	for _, c := range calls {
		named = append(named, fmt.Sprintf("%s (%s)", describeUnclaimed(c.call), earlierWhere(c)))
	}
	return fmt.Sprintf("the application made %d call(s) in an earlier test set that no mock answered, which keploy decided only after that set had ended (%s): %s",
		len(calls), models.KindOutsideEveryTest, strings.Join(named, "; "))
}

// simulate sends tc's request (the hooks' SimulateRequest; tc is never nil)
// and notes when it sent it and read the answer as tc's span in the running
// set's attempt (earlierSetCalls.noteSpan), the latest send's for a request
// sent again.
func (r *Replayer) simulate(ctx context.Context, tc *models.TestCase, testSetID string) (interface{}, error) {
	began := time.Now()
	resp, err := r.hookImpl.SimulateRequest(ctx, tc, testSetID)
	r.earlierSetCalls.noteSpan(testSetID, tc.Name, testSpan{began: began, ended: time.Now()})
	return resp, err
}

// passesEarlierSetCalls is the run's verdict pass, false where a call an
// earlier set made fails the run (earlierSetCalls.failing), which it says at
// ERROR, naming each call, its set and its test, where it names one.
func (r *Replayer) passesEarlierSetCalls(pass bool) bool {
	late := r.earlierSetCalls.failing()
	if len(late) == 0 {
		return pass
	}
	r.logger.Error("test run failed: "+earlierSetCallsWhy(late)+" ("+provenswitch.Env+"=1)",
		zap.String("next_step", "each call is the set's it was made in, and the test's, where one is named: fix the application, or re-record that test (that set, for a call of no test) if the change is intended"))
	return false
}
