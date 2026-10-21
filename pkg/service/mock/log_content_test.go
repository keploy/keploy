package mock

import (
	"context"
	"errors"
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

// unreadOutcomeInstr is an agent whose reads of what it served and missed
// fail with its reply's text.
type unreadOutcomeInstr struct {
	deadlineInstr
	err error
}

func (i *unreadOutcomeInstr) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return nil, i.err
}

func (i *unreadOutcomeInstr) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return nil, i.err
}

// Why the agent's account of a replay could not be read is its client's
// error, whose text the agent's reply fills: the lines that say the reads
// failed name it by its kind, unless KEPLOY_LOG_CONTENT is set.
func TestReportOutcome_AnUnreadAccountIsLoggedByItsKind(t *testing.T) {
	const canary = "kp-canary-mock-unread-3b9d"
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		inst := &unreadOutcomeInstr{err: errors.New("agent returned 500: " + canary)}
		m := &mockService{logger: logger, instrumentation: inst, config: &config.Config{}}
		m.reportOutcome(context.Background(), 0, replayDetail{}, func() bool { return false })
		logtest.Check(t, logs, canary, on, "failed to read consumed mocks", "failed to read mock misses")
	})
}

// A miss's next step can name the call it was made for (an HTTP session
// call's path): the line that tells it carries it only as logcontent allows.
func TestReportOutcome_AMissNextStepIsLoggedWithoutThePathItNames(t *testing.T) {
	const canary = "kp-canary-mock-miss-next-4b1c"
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		inst := &missesInstr{misses: []models.UnmatchedCall{{Protocol: "HTTP", Destination: "api.test:443", ActualSummary: "GET /reset/{}",
			NextSteps: `Each test is served the HTTP calls it recorded. (sessionCalls: [{host: "api.test", method: "GET", pathPrefix: "/reset/` + canary + `"}])`}}}
		m := &mockService{logger: logger, instrumentation: inst, config: &config.Config{}}
		m.reportOutcome(context.Background(), 0, replayDetail{}, func() bool { return false })
		logtest.Check(t, logs, canary, on, "no recorded mock matched an outgoing call")
	})
}
