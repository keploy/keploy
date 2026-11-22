package record

import (
	"context"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A RECORDING WHOSE AGENT DROPPED MOCKS SAYS SO IN ITS TEST-SET.
//
// The agent can drop a mock after it captured it: its memory guard lets go of
// the mocks of the requests made while it paused recording, and a full or
// closed mock output loses what is sent to it. A per-test mock's test case is
// left out with it, but a mock no test case owns (one the application made as
// it started, before the first test; a mock a session keeps for every test)
// costs no test case: the set's tests are kept, and a replay of them finds no
// mock for the call, in any test that needs it, or as the application starts.
// The agent's summary of the session (StreamEndSummary.MocksDropped) counts
// them, but its WARN is a log line of the recording, gone by the time a
// replay fails on what it dropped. So the test-set's config keeps the summary
// (models.TestSet.RecordMocksDropped), which a replay of it can name.

// incomingEndReader is an instrumentation that reads what the agent's end
// marker of the recording's incoming stream said the session lost
// (AgentClient.IncomingEndSummary).
type incomingEndReader interface {
	IncomingEndSummary() (models.StreamEndSummary, bool)
}

// markMocksDropped writes into testSetID's config the mocks the agent's
// summary of the recording says it dropped (RecordMocksDropped), and says so.
// Nothing when the agent dropped none, or sent no summary.
func (r *Recorder) markMocksDropped(ctx context.Context, testSetID string) {
	er, ok := r.instrumentation.(incomingEndReader)
	if !ok {
		return
	}
	sum, ok := er.IncomingEndSummary()
	if !ok || !sum.MocksDropped.Dropped() {
		return
	}
	dropped := sum.MocksDropped
	r.logger.Warn("the test-set's recording does not hold every mock the agent captured: the agent dropped some after capturing them, so a replay of it may find no mock for a call that needs one; its config.yaml names them (recordMocksDropped)",
		zap.String("testSet", testSetID),
		zap.Int64("by_memory_pressure", dropped.ByPressure), zap.Uint64("for_the_output", dropped.Capacity),
		zap.Strings("first_dropped", dropped.Named),
		zap.String("next_step", "give the keploy agent more memory (--memory-limit), or record with fewer concurrent requests, then record the test-set again"))
	if err := r.updateTestSetConf(ctx, testSetID, func(conf *models.TestSet) { conf.RecordMocksDropped = &dropped }); err != nil {
		utils.LogError(r.logger, err, "keploy could not mark the test-set's recording as missing the mocks the agent dropped; a replay that needs one fails without naming the drop",
			zap.String("testSet", testSetID))
	}
}
