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
// answered (P101, under the proven switch). The set's end reads the misses
// once more, as the agent settles it (a call already made is in the proxy's
// hands and answered, or missed, before the read). One made after the last
// test's misses were read was made outside every test's window: the agent
// marks it (models.UnmatchedCall.Background), and it is the application's
// background work (a fire-and-forget publish or write it starts once its
// last answer has left): it fails nothing and is listed for the set
// (TestReport.BackgroundCalls, before_set_calls.go). One the agent cannot
// place there (made before that read and filed after it, in the last test's
// window, or at a time it does not say) is no test's that is left to judge:
// it fails the set, naming the call.
//
// Known limit: a call the application has not made by the time the set ends
// (it is stopped first) is not judged. Which test such a call belongs to is
// not recorded (design v14's gaps belong to no test), so nothing can wait for
// it without a clock.

// unclaimedCalls reads the calls the application made since the set's last
// test that no mock answered and that the agent could not place outside
// every test's window, data-plane only (dataPlaneCalls): those count against
// the set. answered are the calls of that read that were answered
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
	// (the read was cut short) hands it over here: still the set's, and no
	// test's to charge. What was made after the last test's read is
	// background work.
	calls, answered = splitAnswers(r.setCalls.take(ctx, read, "", nil))
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
	return fmt.Sprintf("the application made %d call(s) in the set's last test that no mock answered, read only at the set's end: %s",
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
		r.logger.Error("test-set failed: the application made a call in its last test that no mock answered, read only at the set's end (KEPLOY_PROVEN=1)",
			zap.String("test-set", testSetID),
			zap.String("protocol", c.Protocol),
			zap.String("call", c.ActualSummary),
			zap.String("destination", c.Destination),
			zap.String("diff", c.Diff),
			zap.String("next_step", "the call was made before keploy read the last test's misses and reached keploy only after it; it changed or is new since the recording: fix the application, or re-record if the change is intended"))
	}
}
