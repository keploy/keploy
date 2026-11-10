package replay

import (
	"fmt"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The calls a test of an earlier test set made that no mock answered, which
// keploy decided only after that set had ended (under the proven switch).
// keploy test serves each test case by name, so the agent knows the test a
// call was made in however late it decides the call. When that is after the
// set's end was read, no read of the set can take it any more: the agent
// keeps it for the next set's first read, naming its test
// (models.UnmatchedCall.EarlierSetTest). It is no call of the set that read
// it, nor of that set's tests, and the set it was made in has its verdict
// already: the run fails with it, naming the call, the test and the set it
// was made in, and no set's verdict counts it.

// earlierSetCalls are the calls of earlier sets' tests the run's reads
// returned (models.UnmatchedCall.EarlierSetTest), with the set each was made
// in: the set the run ran before the one whose read returned it.
type earlierSetCalls struct {
	mu sync.Mutex
	// running is the set whose reads RunTestSet takes now, before the one
	// the run ran before it.
	running, before string
	calls           []earlierSetCall
}

// earlierSetCall is a call of an earlier set's test, with the set it was
// made in ("" when the run ran no set before the one that read it).
type earlierSetCall struct {
	set  string
	call models.UnmatchedCall
}

// begin notes that the run now reads set's calls: an earlier set's test's
// call read from here was made in the set the run ran before it (an earlier
// attempt of the same set, for a set run again).
func (e *earlierSetCalls) begin(set string) {
	e.mu.Lock()
	e.before, e.running = e.running, set
	e.mu.Unlock()
}

// take keeps the calls of read that an earlier set's test made
// (models.UnmatchedCall.EarlierSetTest), and returns the others, and those it
// kept.
func (e *earlierSetCalls) take(read []models.UnmatchedCall) (rest []models.UnmatchedCall, taken []earlierSetCall) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range read {
		if c.EarlierSetTest == "" {
			rest = append(rest, c)
			continue
		}
		taken = append(taken, earlierSetCall{set: e.before, call: c})
	}
	if len(taken) == 0 {
		return read, nil
	}
	e.calls = append(e.calls, taken...)
	return rest, taken
}

// takeEarlierSetCalls is read without the calls an earlier set's test made
// (earlierSetCalls.take), which the run keeps to fail with. It says each one
// that fails the run, at ERROR, as it takes it, in the documented form of a
// call outside every test (outsideFields): named under the test and the set
// it was made in, with the set whose read returned it, and tells the hooks
// each (EarlierSetCallHook).
func (r *Replayer) takeEarlierSetCalls(read []models.UnmatchedCall) []models.UnmatchedCall {
	rest, taken := r.earlierSetCalls.take(read)
	readIn := r.earlierSetCalls.reading()
	h, telling := r.hookImpl.(EarlierSetCallHook)
	for _, c := range taken {
		if !c.call.CountsAgainstTest() {
			continue
		}
		r.logger.Error("test run failed: a test of an earlier test set made a call that no mock answered, which keploy decided only after that set had ended; it is charged to no test and no set, so it fails the run ("+provenswitch.Env+"=1)",
			append(r.outsideFields(c.set, c.call.EarlierSetTest, c.call),
				zap.String("read-in-test-set", readIn))...)
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

// failing is the calls of earlier sets' tests that fail the run
// (models.UnmatchedCall.CountsAgainstTest), in the order they were read.
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

// reset forgets every call, and the sets read, for a new run.
func (e *earlierSetCalls) reset() {
	e.mu.Lock()
	e.running, e.before, e.calls = "", "", nil
	e.mu.Unlock()
}

// earlierSetCallsWhy says why the calls fail the run, naming each call, the
// test it was made in and that test's set.
func earlierSetCallsWhy(calls []earlierSetCall) string {
	var named []string
	for _, c := range calls {
		set := c.set
		if set == "" {
			set = "an earlier test set"
		}
		named = append(named, fmt.Sprintf("%s (made in %s of %s)", describeUnclaimed(c.call), c.call.EarlierSetTest, set))
	}
	return fmt.Sprintf("the application made %d call(s) in a test of an earlier test set that no mock answered, which keploy decided only after that set had ended (%s): %s",
		len(calls), models.KindOutsideEveryTest, strings.Join(named, "; "))
}

// passesEarlierSetCalls is the run's verdict pass, false where a call an
// earlier set's test made fails the run (earlierSetCalls.failing), which it
// says at ERROR, naming each call, its test and that test's set.
func (r *Replayer) passesEarlierSetCalls(pass bool) bool {
	late := r.earlierSetCalls.failing()
	if len(late) == 0 {
		return pass
	}
	r.logger.Error("test run failed: "+earlierSetCallsWhy(late)+" ("+provenswitch.Env+"=1)",
		zap.String("next_step", "each call is the test's it was made in: fix the application, or re-record that test if the change is intended"))
	return false
}
