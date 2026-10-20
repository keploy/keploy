package record

import (
	"context"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/realclient"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
)

// reportRealRun says, as a recording ends under the proven switch with a
// mock.real list, what keploy could neither capture nor relay as real
// (capture_incomplete, design v14 §3.2): no test case holds what those
// connections carried, and capture is never silent. The recording is
// incomplete and the run fails, as for a capture loss (realUncaptured). A
// read that fails is said, and fails the run the same way (realUnread):
// whether the recording is whole cannot be told. Where a later pass judges
// each test case (JudgeLossesLater), each is a WARN, and handed over.
func (r *Recorder) reportRealRun(ctx context.Context) {
	run, err := realclient.Run(context.WithoutCancel(ctx), r.instrumentation, r.mockReal())
	if err != nil {
		why := "keploy could not read from its agent which mock.real connections it could not capture: " + err.Error()
		r.realUnread.Store(&why)
		if LossesJudgedLater() {
			r.logger.Warn("keploy could not read from its agent which mock.real connections it could not capture: the recording is kept, and the auto-replay that follows judges each of its test cases",
				logcontent.Error(err), zap.String("next_step", "check the agent's log for capture_incomplete lines"))
			return
		}
		utils.LogContentError(r.logger, err, "the recording may be incomplete: keploy could not read from its agent which mock.real connections it could not capture",
			zap.String("next_step", "check the agent's log for capture_incomplete lines, then re-record this test set"))
		return
	}
	if run == nil {
		return
	}
	type key struct{ dest, why string }
	counts := map[key]int{}
	var order []key
	for _, c := range run.Incomplete {
		k := key{c.Destination, c.Why}
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	if len(order) > 0 {
		r.realUncaptured.Store(true)
	}
	for _, k := range order {
		if LossesJudgedLater() {
			// The recording is kept: the pass that follows judges each of
			// its test cases (finish hands it over).
			r.logger.Warn("keploy could neither capture nor relay as real connections of it (capture_incomplete), and no test case holds what they carried: the recording is kept, and the auto-replay that follows judges each of its test cases",
				zap.String("destination", k.dest), zap.Int("connections", counts[k]), logcontent.String("next_step", k.why+"; then re-record this test set to capture them"))
			continue
		}
		r.logger.Error("the recording is incomplete: keploy could neither capture nor relay as real connections of it (capture_incomplete), and no test case holds what they carried",
			zap.String("destination", k.dest), zap.Int("connections", counts[k]), logcontent.String("next_step", k.why+"; then re-record this test set"))
	}
}

// finishCapture ends the capture watch's account of test set testSetID
// (finishRun) with what reportRealRun found of mock.real: whether the run
// fails, and whether the recording is incomplete.
func (r *Recorder) finishCapture(w *captureWatch, testSetID string) (fail, incomplete bool) {
	unread := ""
	if why := r.realUnread.Load(); why != nil {
		unread = *why
	}
	return w.finishRun(testSetID, r.realUncaptured.Load(), unread)
}

// sessionEnd is the session's status and reason, as its telemetry says them:
// aborted for a stop with a reason; incomplete when the agent's captures
// from before the stop were not all handed over or written (agentHeldBack,
// storeWrote, agentLost), or the capture watch found what fails a recording
// judged on its own (captureLossy), even where a later pass judges it and
// the run does not fail; completed otherwise.
func sessionEnd(stopReason string, agentHeldBack, captureLossy, storeWrote, agentLost bool) (status, reason string) {
	switch {
	case stopReason != "":
		return "aborted", stopReason
	case agentHeldBack:
		reason = "the agent's captures from before the stop were not all handed over"
		if !storeWrote {
			reason = "mocks the recording captured were not written to the mock file"
			if agentLost {
				reason = "the agent's captures from before the stop were not all handed over, and mocks the recording captured were not written to the mock file"
			}
		}
		return "incomplete", reason
	case captureLossy:
		return "incomplete", "keploy's agent could not capture every call while it recorded, or could not be checked"
	}
	return "completed", ""
}

// mockReal is keploy.yml mock.real as the recorder reads it: only under the
// proven switch, and none without a config.
func (r *Recorder) mockReal() []string {
	if !provenswitch.On() || r.config == nil {
		return nil
	}
	return r.config.Mock.Real
}
