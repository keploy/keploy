package replay

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The calls a test set's application made after its last test that no mock
// answered (P101, under the proven switch). A call made while no test is open
// is a pool call (design v14 §5): its miss is charged to the next test that
// begins in that instance. After the set's last test none begins, so its miss
// was charged to no test, and a set whose application made a wrong call after
// its last answer (a fire-and-forget publish or write it starts once the
// answer has left) passed, keploy test exiting 0. The set's end reads those
// misses once more, as the agent settles it (a call already made is in the
// proxy's hands and answered, or missed, before the read): a pool miss no
// later test takes fails the set, naming the call.
//
// Known limit: a call the application has not made by the time the set ends
// (it is stopped first) is not judged. Which test such a call belongs to is
// not recorded (design v14's gaps belong to no test), so nothing can wait for
// it without a clock.

// unclaimedCalls reads the calls the application made since the set's last
// test that no mock answered, data-plane only (dataPlaneCalls): those count
// against the set. answered are the calls of that read that were answered
// (models.UnmatchedCall.Answer, proven switch), which count against nothing
// and are the set's (TestReport.SetAnswers). err says the read could not
// vouch for every such call.
func (r *Replayer) unclaimedCalls(ctx context.Context) (calls, answered []models.UnmatchedCall, err error) {
	// A miss is the agent's to report. A base path has none, and no mocks.
	if r.noAgent {
		return nil, nil, nil
	}
	read, err := r.instrumentation.GetMockErrors(ctx)
	r.keptInside.add(read)
	// A set whose first test's read did not take what was missed before it
	// (the read was cut short) hands it over here: still the set's.
	calls, answered = splitAnswers(r.setCalls.take(read))
	return dataPlaneCalls(calls), answered, err
}

// splitAnswers splits a read of the calls the agent reports into the misses
// and the answered calls (models.UnmatchedCall.Answer, proven switch), each in
// read order: an answer never lands in a miss list, nor changes a status. A
// read with no answer is returned as it is.
func splitAnswers(read []models.UnmatchedCall) (misses, answers []models.UnmatchedCall) {
	if !slices.ContainsFunc(read, func(c models.UnmatchedCall) bool { return c.Answer != "" }) {
		return read, nil
	}
	for _, c := range read {
		if c.Answer != "" {
			answers = append(answers, c)
			continue
		}
		misses = append(misses, c)
	}
	return misses, answers
}

// unclaimedWhy says why the calls fail the set, naming each.
func unclaimedWhy(calls []models.UnmatchedCall) string {
	if len(calls) == 0 {
		return ""
	}
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, describeUnclaimed(c))
	}
	return fmt.Sprintf("the application made %d call(s) after the set's last test that no mock answered, which no later test could take: %s",
		len(calls), strings.Join(names, "; "))
}

// describeUnclaimed names one unclaimed call: its protocol, what it was, and
// where it went.
func describeUnclaimed(c models.UnmatchedCall) string {
	s := c.Protocol
	if c.ActualSummary != "" {
		s += " " + c.ActualSummary
	}
	if c.Destination != "" {
		s += " to " + c.Destination
	}
	return strings.TrimSpace(s)
}

// logUnclaimed says, at ERROR, each call that fails the set.
func (r *Replayer) logUnclaimed(testSetID string, calls []models.UnmatchedCall) {
	for _, c := range calls {
		r.logger.Error("test-set failed: the application made a call after its last test that no mock answered (KEPLOY_PROVEN=1)",
			zap.String("test-set", testSetID),
			zap.String("protocol", c.Protocol),
			zap.String("call", c.ActualSummary),
			zap.String("destination", c.Destination),
			zap.String("diff", c.Diff),
			zap.String("next_step", "a call the application starts after it has answered a test (a fire-and-forget publish or write) is no test's; it changed or is new since the recording: fix the application, or re-record if the change is intended"))
	}
}
