package http

import (
	"errors"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// AnswerProblem names the variable and answer place a replay cannot write
// in an answer, and says why, exactly when the replay refuses to serve it:
// an answer whose body or header no longer holds the place, or holds
// another value than the variable's recorded one there. A held place, a
// mock with no answer place and one with no variables have none.
func TestAnswerProblemIsTheReplaysRefusal(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	echo := func(answer string) models.MockVariables {
		return models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Answer: []string{answer}, Echo: true}}
	}
	for _, tc := range []struct {
		name       string
		vars       models.MockVariables
		header     map[string]string
		body       string
		place, why string
	}{
		{name: "the place held", vars: echo("body.idempotency_key"), body: `{"idempotency_key":"k-1","error":"card_declined"}`},
		{name: "a bind and no answer place", vars: models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Echo: true}}, body: `{"error":"card_declined"}`},
		{name: "the body no longer holds the place", vars: echo("body.idempotency_key"), body: `{"error":"card_declined"}`,
			place: "body.idempotency_key", why: "the recorded response holds no scalar there"},
		{name: "the body holds another value there", vars: echo("body.idempotency_key"), body: `{"idempotency_key":"other"}`,
			place: "body.idempotency_key", why: `the recorded response holds "other" there, not the variable's recorded value "k-1"`},
		{name: "a number place held, written as a number", vars: models.MockVariables{
			"key": {Recorded: "k-1", Bind: "body.key", Echo: true},
			"amt": {Recorded: "5", Bind: "body.amount", Answer: []string{"body.charged"}, Echo: true},
		}, body: `{"charged":5}`},
		{name: "a header place held", vars: echo("header.X-Request-Id"), header: map[string]string{"X-Request-Id": "k-1"}, body: `{}`},
		{name: "a header place gone", vars: echo("header.X-Request-Id"), body: `{}`,
			place: "header.X-Request-Id", why: `does not hold variable key's recorded value "k-1" as the one value of header.X-Request-Id`},
		{name: "a header place holding another value", vars: echo("header.X-Request-Id"), header: map[string]string{"X-Request-Id": "other"}, body: `{}`,
			place: "header.X-Request-Id", why: `does not hold variable key's recorded value "k-1"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := at(varMock("mock-1", "POST", "http://pay/charge", `{"key":"k-1","amount":5}`, 402, tc.body, tc.vars), 1)
			for k, v := range tc.header {
				m.Spec.HTTPResp.Header[k] = v
			}
			name, place, why := AnswerProblem(m)
			if tc.why == "" {
				if name != "" || place != "" || why != "" {
					t.Fatalf("AnswerProblem = %q %q %q; want none", name, place, why)
				}
			} else if name != "key" || place != tc.place || !strings.Contains(why, tc.why) {
				t.Fatalf("AnswerProblem = %q %q %q; want key %q saying %q", name, place, why, tc.place, tc.why)
			}
			// The replay of the same mock, a call with another key: it
			// serves the answer exactly when AnswerProblem names none.
			db := &varDb{session: []*models.Mock{m}}
			ok, _, diag := matchVar(t, newHTTP(), db, liveReq("POST", "/charge", `{"key":"k-9","amount":5}`))
			if tc.why == "" && !ok {
				t.Fatalf("the replay refused a mock AnswerProblem passes: %+v", diag)
			}
			if tc.why != "" {
				p := wantProblem(t, ok, diag, tc.why)
				if p.name != name || p.place != place || p.why != why {
					t.Fatalf("the replay refused %q %q %q; AnswerProblem said %q %q %q", p.name, p.place, p.why, name, place, why)
				}
			}
		})
	}
}

// What AnswerProblem cannot read it says, naming no variable: a variables
// block the replay cannot read, and an answer kept apart from the mock that
// does not load. It reads an answer kept apart as the replay serves it,
// loaded; no mock and a mock with no variables have no problem.
func TestAnswerProblemReadsWhatTheReplayReads(t *testing.T) {
	charge := func(vars models.MockVariables, body string) *models.Mock {
		return varMock("mock-1", "POST", "http://pay/charge", `{"key":"k-1"}`, 200, body, vars)
	}
	echo := models.MockVariables{"key": {Recorded: "k-1", Bind: "body.key", Answer: []string{"body.idempotency_key"}, Echo: true}}
	none := func(t *testing.T, m *models.Mock) {
		t.Helper()
		if name, place, why := AnswerProblem(m); name != "" || place != "" || why != "" {
			t.Fatalf("AnswerProblem = %q %q %q; want none", name, place, why)
		}
	}
	t.Run("no mock", func(t *testing.T) { none(t, nil) })
	t.Run("no variables", func(t *testing.T) {
		// Nothing to write: its answer is not even loaded.
		m := charge(nil, `{"error":"x"}`)
		m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) { return nil, nil, errors.New("gone") })
		none(t, m)
	})
	t.Run("an unreadable block", func(t *testing.T) {
		name, place, why := AnswerProblem(charge(models.MockVariables{"key": {Bind: "body.key", Answer: []string{"body.idempotency_key"}}}, `{}`))
		if name != "" || place != "" || why != "variable key has no recorded value" {
			t.Fatalf("AnswerProblem = %q %q %q; want the block's own problem", name, place, why)
		}
	})
	t.Run("an answer kept apart, loaded", func(t *testing.T) {
		m := charge(echo, "")
		m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
			return &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"idempotency_key":"k-1"}`}, nil, nil
		})
		none(t, m)
	})
	t.Run("an answer kept apart that does not load", func(t *testing.T) {
		m := charge(echo, `{"idempotency_key":"k-1"}`)
		m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) { return nil, nil, errors.New("gone") })
		name, _, why := AnswerProblem(m)
		if name != "" || !strings.Contains(why, "gone") {
			t.Fatalf("AnswerProblem = %q %q; want the load's error", name, why)
		}
	})
}
