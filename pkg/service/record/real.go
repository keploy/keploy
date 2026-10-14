package record

import (
	"context"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/realclient"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// reportRealRun says, as a recording ends under the proven switch with a
// mock.real list, what keploy could neither capture nor relay as real
// (capture_incomplete, design v14 §3.2): no test case holds what those
// connections carried, and capture is never silent. The recording is
// incomplete and the run fails, as for a capture loss (realUncaptured). A
// read that fails is said, and fails the run the same way: whether the
// recording is whole cannot be told.
func (r *Recorder) reportRealRun(ctx context.Context) {
	run, err := realclient.Run(context.WithoutCancel(ctx), r.instrumentation, r.mockReal())
	if err != nil {
		r.realUncaptured.Store(true)
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
		r.logger.Error("the recording is incomplete: keploy could neither capture nor relay as real connections of it (capture_incomplete), and no test case holds what they carried",
			zap.String("destination", k.dest), zap.Int("connections", counts[k]), zap.String("next_step", k.why+"; then re-record this test set"))
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
