package replay

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
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
// and no set's verdict counts it.

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
	calls   []earlierSetCall
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
	e.running = set
	e.mu.Unlock()
}

// madeIn is the set an earlier set's call c was made in: the agent's count
// back from the running set (models.UnmatchedCall.EarlierSet; 1 from an
// agent that names only the test), "" when the run began no such set. Called
// under mu.
func (e *earlierSetCalls) madeIn(c models.UnmatchedCall) string {
	back := c.EarlierSet
	if back < 1 {
		back = 1
	}
	if i := len(e.sets) - 1 - back; i >= 0 {
		return e.sets[i]
	}
	return ""
}

// take keeps the calls of read that an earlier set made
// (models.UnmatchedCall.EarlierSet, EarlierSetTest), and returns the others,
// and those it kept.
func (e *earlierSetCalls) take(read []models.UnmatchedCall) (rest []models.UnmatchedCall, taken []earlierSetCall) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range read {
		if c.EarlierSet < 1 && c.EarlierSetTest == "" {
			rest = append(rest, c)
			continue
		}
		taken = append(taken, earlierSetCall{set: e.madeIn(c), call: c})
	}
	if len(taken) == 0 {
		return read, nil
	}
	e.calls = append(e.calls, taken...)
	return rest, taken
}

// takeEarlierSetCalls is read without the calls an earlier set made
// (earlierSetCalls.take), which the run keeps to fail with. It says each one
// that fails the run, at ERROR, as it takes it, naming the set it was made in
// and its test (none for a call made while no test's view named one), and the
// set whose read returned it. The line names the call by its shape and the
// path tag it carries (the agent's); the console names it whole beside that
// tag (sayEarlierSetCall), as no report does: no set's report holds it.
func (r *Replayer) takeEarlierSetCalls(read []models.UnmatchedCall) []models.UnmatchedCall {
	rest, taken := r.earlierSetCalls.take(read)
	for _, c := range taken {
		if !c.call.CountsAgainstTest() {
			continue
		}
		sayEarlierSetCall(r.console, c)
		r.logger.Error("test run failed: an earlier test set made a call that no mock answered, which keploy decided only after that set had ended ("+provenswitch.Env+"=1)",
			zap.String("made-in-test", c.call.EarlierSetTest),
			zap.String("made-in-test-set", c.set),
			zap.String("read-in-test-set", r.earlierSetCalls.reading()),
			zap.String("protocol", c.call.Protocol),
			zap.String("call", models.UnmatchedCallLogShape(c.call)),
			utils.PathTagField(c.call.PathTag),
			zap.String("destination", models.HostShape(c.call.Destination)), utils.HostTagField(c.call.HostTag),
			zap.Strings("differs", models.FieldShapes(c.call.Differs)),
			zap.String("next_step", "the call is that set's (and that test's, where one is named), not the set whose read returned it: it changed or is new since the recording; fix the application, or re-record that test if the change is intended"))
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

// reset forgets every call, and the sets read, for a new run.
func (e *earlierSetCalls) reset() {
	e.mu.Lock()
	e.sets, e.running, e.calls = nil, "", nil
	e.mu.Unlock()
}

// madeWhere is where c was made, as a line says it: "test-2 of test-set-a",
// or that it was made in no test (between two of the set's tests, or after
// its last).
func (c earlierSetCall) madeWhere() string {
	set := c.set
	if set == "" {
		set = "an earlier test set"
	}
	test := c.call.EarlierSetTest
	if test == "" {
		test = "no test (between two of its tests, or after its last)"
	}
	return test + " of " + set
}

// earlierSetCallsWhy says why the calls fail the run, as the run's log line
// says it: naming each call by its shape and the path tag it carries
// (describeUnclaimedForLog), never its summary, and the set it was made in and
// the test, where it names one, or that it was made in no test. The console
// names each call whole beside its tag (sayEarlierSetCall).
func earlierSetCallsWhy(calls []earlierSetCall) string {
	var named []string
	for _, c := range calls {
		named = append(named, fmt.Sprintf("%s (made in %s)", describeUnclaimedForLog(c.call), c.madeWhere()))
	}
	return fmt.Sprintf("the application made %d call(s) in an earlier test set that no mock answered, which keploy decided only after that set had ended: %s",
		len(calls), strings.Join(named, "; "))
}

// sayEarlierSetCall writes, on keploy's console (w), c, a call of an earlier
// set that fails the run: whole, beside the path tag the lines that log it
// name it by, its destination and where it was made. The console only, never
// the log: the summary holds what the call sent (a path's token, a
// statement's literal). It is the one place the run names the call whole: no
// set's report holds it. Written as printMismatchReport writes a test's
// misses. Nothing with no console (a nil w).
func sayEarlierSetCall(w io.Writer, c earlierSetCall) {
	if w == nil {
		return
	}
	line := fmt.Sprintf("  Mock mismatch of an earlier test set: [%s] %s", c.call.Protocol, models.WithPathTag(c.call.Named(), c.call.PathTag))
	line += models.DestinationNamed(c.call.Destination, c.call.HostTag)
	_, _ = fmt.Fprintf(w, "%s   (made in %s)\n", line, c.madeWhere())
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
