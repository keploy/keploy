package mock

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
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

// missesInstr is an agent that answers the epilogue reads at once, with the
// calls no mock answered.
type missesInstr struct {
	deadlineInstr
	misses []models.UnmatchedCall
}

func (i *missesInstr) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return i.misses, nil
}

// A call no mock answered is named by its shape, not the values it sent,
// unless KEPLOY_LOG_CONTENT is set.
func TestReportOutcome_AMissIsLoggedWithoutTheValuesItSent(t *testing.T) {
	const canary = "kp-canary-mock-miss-2e7a"
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		inst := &missesInstr{misses: []models.UnmatchedCall{{Protocol: "MySQL", Destination: "db:3306",
			ActualSummary: "COM_QUERY SELECT body FROM notes WHERE tag = '" + canary + "'"}}}
		m := &mockService{logger: logger, instrumentation: inst, config: &config.Config{}}
		m.reportOutcome(context.Background(), 0, replayDetail{}, func() bool { return false })
		logtest.Check(t, logs, canary, on, "no recorded mock matched an outgoing call")
		e := logs.FilterMessage("no recorded mock matched an outgoing call").All()
		if got, want := e[0].ContextMap()["call_shape"], "COM_QUERY SELECT body FROM notes WHERE tag = ?"; got != want {
			t.Fatalf("the line names the call %v, want its shape %q", got, want)
		}
	})
}
