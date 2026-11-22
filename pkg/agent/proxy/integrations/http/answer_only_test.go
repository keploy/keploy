package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// The CI test's list of apps answers the ids earlier tests made, each the run
// variable of the test that made it, and the test never sends one. An
// earlier test this replay did not run before it leaves its id unbound: its
// answer place is refused, as before, unless the test's scope began naming
// it answered as recorded (the agent's answer-only need). Then the list is
// answered with the recorded id, and an id an earlier test of this run did
// make is answered with its run value. A call that sends the id, whole or as
// a word of a longer value, misses while it is not bound, however the scope
// began; and a call that binds it binds it, and is answered its live value.
func TestVariables_AnAnswerOnlyRunVariableIsAnsweredAsRecordedOnlyWhereTheScopeSaysSo(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const slow, made = "run-appId0-13", "run-appId0-7"
	list := func() *models.Mock {
		return at(varMock("audits", "GET", "http://store/audits", "", 200, `{"apps":[{"id":"appRec13"},{"id":"appRec7"}]}`,
			models.MockVariables{
				slow: {Recorded: "appRec13", Answer: []string{"body.apps[0].id"}},
				made: {Recorded: "appRec7", Answer: []string{"body.apps[1].id"}},
			}), 1)
	}
	type row struct {
		name     string
		mock     *models.Mock
		call     *req
		marked   []string
		bound    map[string]string
		answered string // "" = a miss
		why      string
		binds    string // the value slow is bound to after the call, "" for none
	}
	rows := []row{
		{name: "nothing marked: refused, as before", mock: list(), call: liveReq("GET", "/audits", ""),
			bound: map[string]string{made: "appLive7"}, why: "was not made before it in this replay"},
		{name: "marked: the unbound id is answered as recorded, the bound one its run value", mock: list(), call: liveReq("GET", "/audits", ""),
			marked: []string{slow}, bound: map[string]string{made: "appLive7"}, answered: `{"apps":[{"id":"appRec13"},{"id":"appLive7"}]}`},
		{name: "marked, and both earlier tests ran: both answered their run values", mock: list(), call: liveReq("GET", "/audits", ""),
			marked: []string{slow}, bound: map[string]string{made: "appLive7", slow: "appLive13"}, answered: `{"apps":[{"id":"appLive13"},{"id":"appLive7"}]}`},
		{name: "one of two unbound ids marked: the other is refused", mock: list(), call: liveReq("GET", "/audits", ""),
			marked: []string{made}, why: "was not made before it in this replay"},
		{name: "marked, and the call sends it whole: a miss",
			mock: at(varMock("read", "GET", "http://store/apps/appRec13", "", 200, `{"id":"appRec13"}`,
				models.MockVariables{slow: useV("appRec13", []string{"url.path[1]"}, "body.id")}), 1),
			call: liveReq("GET", "/apps/appRec13", ""), marked: []string{slow}, why: "was not made before it in this replay"},
		{name: "marked, and the call sends it as a word of a longer value: a miss",
			mock: at(varMock("read", "GET", "http://store/apps/app-appRec13", "", 200, `{"id":"appRec13"}`,
				models.MockVariables{slow: useV("appRec13", []string{"url.path[1]#1"}, "body.id")}), 1),
			call: liveReq("GET", "/apps/app-appRec13", ""), marked: []string{slow}, why: "was not made before it in this replay"},
		{name: "marked, and the call binds it: bound and answered its live value",
			mock: at(varMock("create", "POST", "http://store/apps", `{"id":"appRec13"}`, 201, `{"id":"appRec13"}`,
				models.MockVariables{slow: bindV("appRec13", "body.id", "body.id")}), 1),
			call: liveReq("POST", "/apps", `{"id":"appLive13"}`), marked: []string{slow}, answered: `{"id":"appLive13"}`, binds: "appLive13"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			db := &varDb{perTest: []*models.Mock{r.mock}}
			db.table.KeepRun()
			db.table.StartOpeningScope("o::TestCI", func(uint64) {}, nil, nil, nil, r.marked)
			for name, v := range r.bound {
				if _, ok := db.table.Bind(variables.Binding{Name: name, Value: v, Recorded: r.mock.Spec.Variables[name].Recorded, Mock: "made-earlier", Place: "body.id"}); !ok {
					t.Fatalf("%s did not bind", name)
				}
			}
			ok, stub, diag := matchVar(t, newHTTP(), db, r.call)
			if r.answered == "" {
				if p := wantProblem(t, ok, diag, r.why); p.name != slow {
					t.Fatalf("the miss names variable %s, want %s", p.name, slow)
				}
				return
			}
			if !ok || stub == nil {
				t.Fatalf("the call missed: %+v", diag)
			}
			if got := stub.Spec.HTTPResp.Body; got != r.answered {
				t.Fatalf("answered %s, want %s", got, r.answered)
			}
			b, bound := db.table.Lookup(slow)
			if got := map[bool]string{true: b.Value}[bound]; got != r.binds && r.bound[slow] == "" {
				t.Fatalf("%s is bound to %q after the call, want %q", slow, got, r.binds)
			}
		})
	}
}
