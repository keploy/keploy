package record

import (
	"context"
	"errors"
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/realclient"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// errRealRunSilent is why the read of what keploy could not capture of
// mock.real was given up: keploy had the read's whole bound (stall.HangBudget
// of its own runnable time) to run, and the agent did not answer.
var errRealRunSilent = fmt.Errorf("the keploy agent did not say which mock.real connections it could not capture: keploy had %s of its own time to run, and it did not answer", stall.HangBudget)

// errRealRunStopped is why that read, ended by the user's Ctrl+C at an offer
// of the stop's (utils.StopNow), is not read.
var errRealRunStopped = errors.New("a Ctrl+C stopped keploy's wait for its agent to say which mock.real connections it could not capture")

// reportRealRun says, as a recording ends under the proven switch with a
// mock.real list, what keploy could neither capture nor relay as real
// (capture_incomplete, design v14 §3.2): no test case holds what those
// connections carried, and capture is never silent. The recording is
// incomplete and the run fails, as for a capture loss (realUncaptured). A
// read that fails, or that the agent does not answer (readRealRun), is
// said, and fails the run the same way (realUnread): whether the recording
// is whole cannot be told. Where a later pass judges each test case
// (JudgeLossesLater), each is a WARN, and handed over.
func (r *Recorder) reportRealRun(ctx context.Context) {
	run, err := r.readRealRun(ctx)
	if err != nil {
		why := "keploy could not read from its agent which mock.real connections it could not capture: " + err.Error()
		r.realUnread.Store(&why)
		if LossesJudgedLater() {
			r.logger.Warn("keploy could not read from its agent which mock.real connections it could not capture: the recording is kept, and the auto-replay that follows judges each of its test cases",
				zap.Error(err), zap.String("next_step", "check the agent's log for capture_incomplete lines"))
			return
		}
		utils.LogError(r.logger, err, "the recording may be incomplete: keploy could not read from its agent which mock.real connections it could not capture",
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
				zap.String("destination", k.dest), zap.Int("connections", counts[k]), zap.String("next_step", k.why+"; then re-record this test set to capture them"))
			continue
		}
		r.logger.Error("the recording is incomplete: keploy could neither capture nor relay as real connections of it (capture_incomplete), and no test case holds what they carried",
			zap.String("destination", k.dest), zap.Int("connections", counts[k]), zap.String("next_step", k.why+"; then re-record this test set"))
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

// readRealRun reads what the run saw of mock.real (realclient.Run) on the
// stop's goroutine, and waits for it as the capture watch's reads are waited
// for (countReads.readNow): the stop does not end it (the read is made once
// the stop has ended record's context), the interrupt that gives the stop's
// waits up (utils.StopNow) does, at once, and it is given up once keploy had
// stall.HangBudget of its own runnable time to run without an answer, on
// countReadClock, so a starved keploy is not taken for a silent agent and a
// stop no one presses Ctrl+C at (CI's `sudo pkill -INT`) still ends. Once it
// has waited countReadEvery it says so at Info, with the bound; it stands no
// offer of the stop's (a press cannot be told from the copy of the stop's
// own signal that sudo hands on). Without the switch or a list it asks
// nothing.
func (r *Recorder) readRealRun(ctx context.Context) (*models.RealRun, error) {
	list := r.mockReal()
	if len(list) == 0 {
		return nil, nil
	}
	type answer struct {
		run *models.RealRun
		err error
	}
	got := make(chan answer, 1)
	go func() {
		readCtx, cancelRead := stopProof(ctx)
		defer cancelRead()
		run, err := boundedAgentRead(readCtx, countReadClock, stall.HangBudget, errRealRunSilent, func(c context.Context) (*models.RealRun, error) {
			return realclient.Run(c, r.instrumentation, list)
		})
		if errors.Is(err, context.Canceled) {
			// Its context is stop-proof: only utils.StopNow ends it.
			err = errRealRunStopped
		}
		got <- answer{run, err}
	}()
	ticks, stopTicks := countReadTicks(countReadEvery)
	defer stopTicks()
	for said := false; ; {
		select {
		case a := <-got:
			return a.run, a.err
		case <-ticks:
			if !said {
				said = true
				r.logger.Info(fmt.Sprintf("Still waiting for the keploy agent to say which mock.real connections it could not capture; Ctrl+C does not cut this short: the read is given up once keploy has had %s of its own time to run without an answer", stall.HangBudget))
			}
		}
	}
}

// mockReal is keploy.yml mock.real as the recorder reads it: only under the
// proven switch, and none without a config.
func (r *Recorder) mockReal() []string {
	if !provenswitch.On() || r.config == nil {
		return nil
	}
	return r.config.Mock.Real
}
