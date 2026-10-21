package replay

import (
	"fmt"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent"
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

// setCalls are the calls of the set RunTestSet is running that no mock
// answered and that are the set's, no test's (setsCall): a read hands them
// over with a test's own, and they are kept here, out of that test's verdict,
// for the set's end to fail the set with. Reset as each set begins.
type setCalls struct {
	mu    sync.Mutex
	calls []models.UnmatchedCall
}

// setsCall reports whether c is the set's, no test's: made before the set's
// first test (models.UnmatchedCall.BeforeSet).
func setsCall(c models.UnmatchedCall) bool { return c.BeforeSet }

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

// setCallsWhy says why the set's calls fail it, naming each: today every one
// was made before the set's first test (setsCall).
func setCallsWhy(calls []models.UnmatchedCall) string {
	if len(calls) == 0 {
		return ""
	}
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, describeUnclaimed(c))
	}
	return fmt.Sprintf("the application made %d call(s) before the set's first test that no mock answered: %s",
		len(calls), strings.Join(names, "; "))
}

// logSetCalls says, at ERROR, each of the set's calls that fails it.
func (r *Replayer) logSetCalls(testSetID string, calls []models.UnmatchedCall) {
	for _, c := range calls {
		r.logger.Error("test-set failed: the application made a call before its first test that no mock answered (KEPLOY_PROVEN=1)",
			zap.String("test-set", testSetID),
			zap.String("protocol", c.Protocol),
			zap.String("call_shape", models.MissShape(c)),
			logcontent.StringLen("call", c.ActualSummary),
			zap.String("destination", c.Destination),
			logcontent.StringLen("diff", c.Diff),
			zap.String("next_step", "a call the application makes before the set's first test (as it starts, or while it waits for the set) is no test's; it changed or is new since the recording: fix the application, or re-record if the change is intended"))
	}
}
