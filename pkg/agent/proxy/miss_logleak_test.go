package proxy

import (
	"errors"
	"fmt"
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// Every parser's miss funnels through sendMockNotFoundErrorFrom, which logs
// one line for it at WARN in the agent: it names the call by its shape
// (models.MissLogShape) and withholds what the next step quotes, never the
// call's summary, its diff or its rendered requests, which hold what the call
// sent (a path's token, a statement's literal, a body) for the user's own
// report. A miss with no report is logged with its error's quoted input
// withheld. It logged the summary whole, at WARN, for every miss.
func TestTheMissLineLogsNoneOfTheCall(t *testing.T) {
	const s = logleak.Canary
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"an HTTP miss", models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{
			Protocol: "HTTP", Destination: "api.example.com", ActualSummary: "GET /v1/reset/" + s + "?access_token=" + s,
			Diff: "body.token: " + s, ReceivedReq: "Authorization: Bearer " + s, ClosestMock: "mock-0",
			NextSteps: `declare it (sessionCalls: [{host: "api.example.com", method: "GET", pathPrefix: "/v1/reset/` + s + `"}])`,
		})},
		{"a MySQL miss", models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{
			Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT name FROM users WHERE token = '" + s + "'", Diff: "actual: " + s,
		})},
		{"a learn round's miss, whose next step is a refused word's line", models.NewMockMismatchError(models.ErrNoMockMatched, &models.MockMismatchReport{
			Protocol: "Mongo", ActualSummary: "find on sessions", ClosestMock: "mock-3",
			NextSteps: "Refused: " + learn.RefusalLine(learn.Refusal{Kind: learn.RefusalConflict, Mock: "mock-3", Field: "body.filter.token", Word: -1,
				Recorded: s, Full: "a" + s, Why: "recorded " + s + " differs from round F's", Places: []string{"body.filter.token"}}),
		})},
		{"a miss with no report, quoting the call", errors.New(`no mock matched "GET /v1/me?access_token=` + s + `"`)},
		{"a miss with no report, the call written as it is", fmt.Errorf("%w for GET /v1/me?access_token=%s", models.ErrNoMockMatched, s)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, logs := logleak.Observe()
			p := &Proxy{logger: logger, errChannel: make(chan error, 1)}
			p.sendMockNotFoundErrorFrom(tc.err, 0, 0, &models.ConditionalDstCfg{Addr: "10.0.0.5:80"})
			if logs.Len() == 0 {
				t.Fatal("precondition: the miss was not logged")
			}
			logleak.Check(t, logs, s, "the miss line")
		})
	}
}
