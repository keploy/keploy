package replay

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The set's end read of the misses with the proven switch off. A parser may
// hold a call past its own window for a later test's recording (a consumer's
// poll answered with no records until the test whose recording answers it),
// and files its miss when no test of the set is left to answer it: at the
// set's end read (integrations.SetEndFiler), or at its connection's end
// (models.UnmatchedCall.Held). With the switch off keploy test made no such
// read, so the agent's parsers took no registration there
// (models.OutgoingOptions.ReadsSetEnds), and such a call was filed only when
// its connection ended: during a later test, whose read kept it out of that
// test as the set's (before_set_calls.go) and nothing reported it, or after
// the set, when no read takes it. A consumer whose connection outlives the
// set (it is stopped after keploy test's last read, or not at all) left no
// trace of it but the agent's log, and the set passed as if every call it
// made had been answered from the recording.
//
// The switch-off run now reads the set's end of the misses too, after its
// read of what was consumed after the set's last test: the agent's parsers
// file each call they hold for a later test of the set first, and the read
// takes it. Each held call, filed there or taken by an earlier test's read,
// is reported as the set's, naming no test, the way the switch off reports a
// miss: in the run's mismatch summary and at WARN, changing no verdict.
// Under the switch the same calls fail the set (unclaimedCalls).

// readsSetEnds reports whether keploy test reads each of its sets' end of
// the misses with a set's end read (models.WithSetEndRead), the word its
// session gives the agent (models.OutgoingOptions.ReadsSetEnds): always under
// the proven switch (unclaimedCalls), and with it off wherever it reads the
// set's end (readsSetEndOff) from an agent (setEndOff).
func (r *Replayer) readsSetEnds() bool {
	return provenswitch.On() || (r.readsSetEndOff() && !r.noAgent)
}

// readsSetEndOff reports whether, with the proven switch off, keploy test
// reads the set's end: where the set's mocks are served, it reads what was
// consumed after the set's last test and then the misses (setEndOff).
func (r *Replayer) readsSetEndOff() bool {
	return !provenswitch.On() && r.instrument && r.mocksDependencies()
}

// setEndOff is the switch-off run's set's end read of the misses
// (models.WithSetEndRead), its last read of the set: the agent's parsers
// file each call they hold for a later test of the set that none answered,
// and the read takes it. It reports the set's held calls, those this read
// took and those an earlier test's read kept as the set's (setCalls), as
// the set's, naming no test, and returns them for the set's report
// (models.TestReport.SetHeldCalls). It changes no verdict: with the switch
// off a miss fails nothing. A call the application made after the set's last test
// that no mock answered, no held one, is no test's: it is said at WARN, as
// the agent said it when the next set's first test began. A read that could
// not take every such call marks the set's misses unattributed
// (noteUnattributed). The read is bounded as the read of what was consumed
// is (setEndDrainBudget).
func (r *Replayer) setEndOff(ctx context.Context, testSetID string) (held []models.UnmatchedCall) {
	// A miss is the agent's to report. A base path has none, and no mocks.
	if r.noAgent {
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, setEndDrainBudget)
	defer cancel()
	read, err := r.instrumentation.GetMockErrors(models.WithSetEndRead(readCtx))
	after := dataPlaneCalls(r.setCalls.take(read))
	// With the switch off the set's calls are held ones only: the agent
	// marks a call made before the set's first test only under the switch.
	held = r.setCalls.failing()
	for _, c := range held {
		r.mockMismatchFailures.AddUnmatchedCallForTest(testSetID, "", c)
	}
	r.logHeldOff(testSetID, held)
	if len(after) > 0 {
		r.logger.Warn("the application made calls after the test set's last test that no mock answered; they are no test's, and with the proven switch off they change no verdict",
			zap.String("test-set", testSetID), zap.Int("count", len(after)), zap.Strings("calls", describeAll(after)))
	}
	if err != nil {
		r.noteUnattributed(testSetID)
		r.logger.Warn("could not read every call the application made after the test set's last test, or that keploy held for a later test of the set, that no mock answered; a held call may be missing from the set's mismatch summary",
			zap.String("test-set", testSetID), zap.Int("callsRead", len(read)), zap.Error(err))
	}
	return held
}

// logHeldOff says, at WARN, each of the set's held calls (setEndOff): with
// the proven switch off it is reported, and fails nothing.
func (r *Replayer) logHeldOff(testSetID string, calls []models.UnmatchedCall) {
	for _, c := range calls {
		r.logger.Warn("keploy held a call of the application for a later test's recording, and no test of the set answered it",
			zap.String("test-set", testSetID),
			zap.String("protocol", c.Protocol),
			zap.String("call", c.Named()),
			zap.String("destination", c.Destination),
			zap.String("diff", c.Diff),
			zap.String("next_step", "a call held for a later test (a consumer's poll whose recorded answer came in a later test) found no recording there: the application's calls changed since the recording; fix the application, or re-record if the change is intended"))
	}
}

// describeAll names each call (describeUnclaimed), in order.
func describeAll(calls []models.UnmatchedCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, describeUnclaimed(c))
	}
	return out
}
