package mock

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// idPairsInstr is an agent that answers the epilogue reads at once, and says
// which ids the run made in place of recorded ones.
type idPairsInstr struct {
	deadlineInstr
	pairs map[string]string
}

func (i *idPairsInstr) GetIDPairs(context.Context) (map[string]string, error) { return i.pairs, nil }

// The ids a run made in place of recorded ones, values of the app's, are
// logged by their count, unless KEPLOY_LOG_CONTENT is set.
func TestReportOutcome_TheRunsIDsAreLoggedByTheirCount(t *testing.T) {
	const canary = "kp-canary-mock-ids-8f4b"
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		inst := &idPairsInstr{pairs: map[string]string{canary + "-recorded": canary + "-live"}}
		m := &mockService{logger: logger, instrumentation: inst, config: &config.Config{}}
		m.reportOutcome(context.Background(), 0, replayDetail{}, func() bool { return false })
		logtest.Check(t, logs, canary, on, "ids this run made in place of recorded ones")
	})
}
