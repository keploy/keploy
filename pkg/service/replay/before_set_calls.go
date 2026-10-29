package replay

import (
	"context"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The calls a test set's application made outside every test's window that
// no mock answered (under the proven switch; founder 2026-10-29, item b).
//
// Before the set's first test the application is starting, and none of its
// calls is a test's: the agent hands the misses made then to the set's first
// test's read marked as the set's (models.UnmatchedCall.BeforeSet). A call
// made between two tests, after the agent read the earlier one's misses and
// before the next test's window opened, or after the set's last test was
// read, the agent marks (models.UnmatchedCall.Background). A call made inside
// a test's window is marked so only by its parser, as a call of a series
// judging marked as the application's background work that no recording of
// the series answered (models.UnmatchedCall.MadeInTest, keploy enterprise).
// Each is the application's background work (a timer, a start-up job, a
// write it starts once its last answer has left): the replay keeps it out of
// every test's verdict, fails nothing with it, and lists it once for the
// set, saying where it was made (TestReport.BackgroundCalls), as keploy mock
// does (founder D1).
//
// The dataflow guard: a call made before the set's first test, or between two
// tests, whose recorded answer reaches the test read next (flowGuard: by the
// word rule keploy learn reads a value's source by) is that test's miss,
// charged to it with the word that reached it named
// (models.UnmatchedCall.ReachesTest): the test read what the application got
// before it, and that answer is gone. A call made after the set's last test
// reaches no test.

// setCalls are the calls of the set RunTestSet is running that are the set's,
// no test's: a read hands them over with a test's own, and they are kept
// here, out of that test's verdict. Reset as each set begins.
type setCalls struct {
	mu sync.Mutex
	// calls are the set's calls made before its first test that count
	// against no test (an answered call, a DNS lookup): its answers
	// (answers) are read from them.
	calls []models.UnmatchedCall
	// background are the calls the application made outside every test's
	// window that no mock answered: its background work, charged to no test
	// and failing nothing, listed for the set (TestReport.BackgroundCalls).
	background []models.UnmatchedCall
	// guard tells whether a call made before the set's first test reached
	// it (flowGuard); nil reads none as reaching it.
	guard *flowGuard
	// last is the test the latest read was a test's of, for the line of a
	// call made between two tests.
	last string
}

// setsCall reports whether c is the set's, no test's: made before the set's
// first test (models.UnmatchedCall.BeforeSet).
func setsCall(c models.UnmatchedCall) bool { return c.BeforeSet }

// reset forgets the previous set's calls; guard reads the new set's.
func (s *setCalls) reset(guard *flowGuard) {
	s.mu.Lock()
	s.calls = nil
	s.background = nil
	s.guard = guard
	s.last = ""
	s.mu.Unlock()
}

// Where a background call was made (models.UnmatchedCall.BackgroundWhy).
const (
	madeBeforeFirst = "made before the set's first test began, as the application started, and its recorded answer reaches no test"
	madeAfterLast   = "made after the set's last test was read, with no test left to run"
)

// madeInside is where a call of a marked background series made inside
// test's window was ("" for a test the read does not name).
func madeInside(test string) string {
	if test == "" {
		return "made inside a test's window, as a call of a marked background series"
	}
	return "made inside " + test + "'s window, as a call of a marked background series"
}

// madeBetween is where a call made between test before and test after was.
func madeBetween(before, after string) string {
	switch {
	case before != "" && after != "":
		return "made between two tests, after " + before + " was read and before " + after + " began"
	case after != "":
		return "made between two tests, before " + after + " began"
	}
	return "made between two tests"
}

// take keeps the calls of read that are the set's (setsCall) and those made
// outside every test's window (models.UnmatchedCall.Background), and returns
// the others: test's own, where test is the test the read is of ("" for the
// set's end) and live the response it got (nil for none). A call made before
// the set's first test, or between the previous test and test, that no mock
// answered is test's own only when the word rule says what changed in test
// this run may have come from its recorded answer (flowGuard.reaches); it is
// then charged to test, with the word and its place named. A call of a
// marked background series made inside a test's window (MadeInTest) is
// guarded the same way, against that test. Each one kept as background work
// says where it was made (BackgroundWhy): a marked series' call, inside
// test's window, or, for the set end's read, the last test's.
func (s *setCalls) take(ctx context.Context, read []models.UnmatchedCall, test string, live any) []models.UnmatchedCall {
	var own []models.UnmatchedCall
	for _, c := range read {
		if !setsCall(c) && !c.Background {
			own = append(own, c)
		}
	}
	var rest []models.UnmatchedCall
	s.mu.Lock()
	defer s.mu.Unlock()
	var drifts []drift
	drifted := false
	driftsOf := func() []drift {
		if !drifted {
			drifts, drifted = s.guard.drifts(test, live, own), true
		}
		return drifts
	}
	changed := false
	for _, c := range read {
		switch {
		case setsCall(c) && c.CountsAgainstTest():
			if why := s.guard.reaches(ctx, c, test, "made before the set's first test", driftsOf()); why != "" {
				c.ReachesTest = why
				rest = append(rest, c)
				changed = true
				continue
			}
			c.Background, c.BackgroundWhy = true, madeBeforeFirst
			s.background = append(s.background, c)
		case setsCall(c):
			s.calls = append(s.calls, c)
		case c.Background && test == "":
			c.BackgroundWhy = madeAfterLast
			if c.MadeInTest() {
				c.BackgroundWhy = madeInside(s.last)
			}
			s.background = append(s.background, c)
		case c.Background:
			where := madeBetween(s.last, test)
			if c.MadeInTest() {
				// Guarded too, on purpose: the series' mark says its
				// answers reach no test, so on a real series the guard
				// never fires. When it fires, test's response holds a
				// word of the series' recorded answer, and a call whose
				// answer reaches a test is that test's (the dataflow
				// rule): charged to test, failing closed.
				where = madeInside(test)
			}
			if why := s.guard.reaches(ctx, c, test, where, driftsOf()); why != "" {
				c.Background, c.ReachesTest = false, why
				rest = append(rest, c)
				changed = true
				continue
			}
			c.BackgroundWhy = where + ", and its recorded answer reaches no test"
			s.background = append(s.background, c)
		default:
			rest = append(rest, c)
			continue
		}
		changed = true
	}
	if test != "" {
		s.last = test
	}
	if !changed {
		return read
	}
	return rest
}

// answers is the set's calls that were answered (models.UnmatchedCall.Answer,
// proven switch), in the order they were read: the set's answers, which fail
// nothing (TestReport.SetAnswers).
func (s *setCalls) answers() []models.UnmatchedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, answers := splitAnswers(s.calls)
	return answers
}

// backgroundCalls is the calls the application made outside every test's
// window that no mock answered, in the order they were read: charged to no
// test, and failing nothing (TestReport.BackgroundCalls).
func (s *setCalls) backgroundCalls() []models.UnmatchedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]models.UnmatchedCall(nil), s.background...)
}

// logBackgroundCalls says, at WARN, once for the set, each call the
// application made outside every test's window, or inside one as a call of a
// marked background series (MadeInTest), that no mock answered: its
// background work (a timer, a start-up job, a late write), which may fail
// inside the application but fails no test and not the set (founder D1). The
// line says which of the two it was, and where names the place.
func (r *Replayer) logBackgroundCalls(testSetID string, calls []models.UnmatchedCall) {
	for _, c := range calls {
		next := c.NextSteps
		if next == "" {
			next = "nothing fails for it; if a test reads what this call does, re-record the test set"
		}
		r.logger.Warn(c.BackgroundLine()+" ("+provenswitch.Env+"=1)",
			zap.String("test-set", testSetID),
			zap.String("protocol", c.Protocol),
			zap.String("call", c.ActualSummary),
			zap.String("destination", c.Destination),
			zap.Time("at", c.At),
			zap.String("where", c.BackgroundWhy),
			zap.String("why", c.UnansweredWhy()),
			zap.String("next_step", next))
	}
}
