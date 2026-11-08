package replay

import (
	"fmt"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The calls a test set's application made before its first test that no mock
// answered (F2, under the proven switch). A call made while no test is open is
// a pool call (design v14 §5). Before the set's first test the application is
// starting, and none of its calls is a test's: the agent dropped the misses
// made then with a warning, so a set whose application made, as it started, a
// call no mock answers (a write it makes at boot that changed since the
// recording) passed. The agent now hands them to the set's first test's read
// marked as the set's (models.UnmatchedCall.BeforeSet, an outside_test miss);
// the replay keeps them out of that test's verdict and fails the set with
// them at its end, naming each, as it does a call made after the set's last
// test (unclaimed_calls.go).
//
// A call a parser held past its own window for a later test's recording that
// none answered (models.UnmatchedCall.Held) is the set's too: its miss is
// filed at the set's end read, or at its connection's end, which may fall in
// any test's window. Whichever read takes it, it charges no test (the test
// open when its connection ended did not make it): under the proven switch
// it fails the set, named as held; with the switch off the set's end reports
// it as the set's and changes no verdict (set_end_off.go).

// setCalls are the calls of the set RunTestSet is running that no mock
// answered and that are the set's, no test's (setsCall): a read hands them
// over with a test's own, and they are kept here, out of that test's verdict,
// for the set's end to fail the set with. Reset as each set begins.
type setCalls struct {
	mu    sync.Mutex
	calls []models.UnmatchedCall
}

// setsCall reports whether c is the set's, no test's: made before the set's
// first test (models.UnmatchedCall.BeforeSet), or held by a parser for a
// later test's recording that none answered (models.UnmatchedCall.Held).
func setsCall(c models.UnmatchedCall) bool { return c.BeforeSet || c.Held }

// reset forgets the previous set's calls.
func (s *setCalls) reset() {
	s.mu.Lock()
	s.calls = nil
	s.mu.Unlock()
}

// take keeps the calls of read that are the set's (setsCall), and returns the
// others: the test's own.
func (s *setCalls) take(read []models.UnmatchedCall) []models.UnmatchedCall {
	var rest []models.UnmatchedCall
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range read {
		if setsCall(c) {
			s.calls = append(s.calls, c)
			continue
		}
		rest = append(rest, c)
	}
	if len(rest) == len(read) {
		return read
	}
	return rest
}

// failing is the set's calls that count against it (dataPlaneCalls), in the
// order they were read.
func (s *setCalls) failing() []models.UnmatchedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return dataPlaneCalls(s.calls)
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

// setCallsWhy says why the set's calls fail it, naming each: those made
// before the set's first test, and those a parser held for a later test's
// recording that none answered (models.UnmatchedCall.Held), wherever they
// were made.
func setCallsWhy(calls []models.UnmatchedCall) string {
	var before, held []string
	for _, c := range calls {
		if c.Held {
			held = append(held, describeUnclaimed(c))
			continue
		}
		before = append(before, describeUnclaimed(c))
	}
	var why []string
	if len(before) > 0 {
		why = append(why, fmt.Sprintf("the application made %d call(s) before the set's first test that no mock answered: %s",
			len(before), strings.Join(before, "; ")))
	}
	if len(held) > 0 {
		why = append(why, fmt.Sprintf("the application made %d call(s) that keploy held for a later test's recording, which no test of the set answered: %s",
			len(held), strings.Join(held, "; ")))
	}
	return strings.Join(why, "; ")
}

// logSetCalls says, at ERROR, each of the set's calls that fails it.
func (r *Replayer) logSetCalls(testSetID string, calls []models.UnmatchedCall) {
	for _, c := range calls {
		msg := "test-set failed: the application made a call before its first test that no mock answered (KEPLOY_PROVEN=1)"
		next := "a call the application makes before the set's first test (as it starts, or while it waits for the set) is no test's; it changed or is new since the recording: fix the application, or re-record if the change is intended"
		if c.Held {
			msg = "test-set failed: keploy held a call of the application for a later test's recording, and no test of the set answered it (KEPLOY_PROVEN=1)"
			next = "a call held for a later test (a consumer's poll whose recorded answer came in a later test) found no recording there: the application's calls changed since the recording; fix the application, or re-record if the change is intended"
		}
		r.logger.Error(msg,
			zap.String("test-set", testSetID),
			zap.String("protocol", c.Protocol),
			zap.String("call", c.Named()),
			zap.String("destination", c.Destination),
			zap.String("diff", c.Diff),
			zap.String("next_step", next))
	}
}
