package http

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// givenAppMock is a recording of POST /apps whose body carries the value
// the test gave as given:app (recorded app-1).
func givenAppMock() *models.Mock {
	return at(varMock("mock-1", "POST", "http://store/apps", `{"name":"app-1"}`, 201, `{"name":"app-1"}`,
		models.MockVariables{"given:app": useV("app-1", []string{"body.name"}, "body.name")}), 1)
}

// A call that carries the value the test gave is answered with it; one that
// carries another value is refused with evidence naming the given and where
// the test gave it.
func TestVariables_AGivenIsPassedOnOrTheCallIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{givenAppMock()}}
	db.table.Start(nil)
	if _, ok := db.table.Give(variables.Binding{Name: "given:app", Value: "app-9", Recorded: "app-1", Mock: "test o::TestApps", Place: "apps_test.go:20"}); !ok {
		t.Fatal("the test's given did not bind")
	}
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"name":"app-8"}`))
	p := wantProblem(t, ok, diag, `the test gave given:app = "app-9" (apps_test.go:20)`)
	if !strings.Contains(p.why, "the application did not pass on the value the test gave") || p.expected != "app-9" || p.actual != "app-8" {
		t.Fatalf("evidence %+v", p)
	}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"name":"app-9"}`))
	if !ok || stub == nil {
		t.Fatalf("the call carrying the given value was refused: %+v", diag)
	}
}

// A use of a given the run's test did not give is refused saying so, not as
// a use before its binding call: no call binds a given.
func TestVariables_AGivenTheTestDidNotGiveIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{givenAppMock()}}
	db.table.Start(nil)
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"name":"app-9"}`))
	wantProblem(t, ok, diag, "the recording's test gave given:app, which this call carries at body.name; this run's test did not give it before this call")
}

// A recording of a given with another recorded value is another test's call,
// not a declaration keploy cannot follow (as a template key's would be).
func TestVariables_AGivenRecordedWithAnotherValueIsAnotherTestsCall(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := &varDb{perTest: []*models.Mock{givenAppMock()}}
	db.table.Start(nil)
	db.table.Give(variables.Binding{Name: "given:app", Value: "app-9", Recorded: "app-0", Mock: "test o::TestOther"})
	ok, _, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"name":"app-9"}`))
	if ok {
		t.Fatal("another test's recording answered the call")
	}
	if diag != nil && diag.variable != nil {
		t.Fatalf("another test's call was refused with a declaration problem: %+v", diag.variable)
	}
}

// A given's miss names the given (MockMismatchReport.Given), and its evidence
// keeps that name and the place's shape, never a value, saying which miss it
// is: the call carried another value than the test gave, a value of another
// JSON type, or this run's test did not give it before the call. A learned
// variable's miss names no variable.
func TestVariables_AGivensMissNamesTheGivenInItsEvidence(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	gave := func(value string) *varDb {
		db := &varDb{perTest: []*models.Mock{givenAppMock()}}
		db.table.Start(nil)
		if value != "" {
			db.table.Give(variables.Binding{Name: "given:app", Value: value, Recorded: "app-1", Mock: "test o::TestApps"})
		}
		return db
	}
	evidence := func(db *varDb, path, body string) (*models.MockMismatchReport, models.UnmatchedCall) {
		t.Helper()
		ok, _, diag := matchVar(t, h, db, liveReq("POST", path, body))
		if ok || diag == nil || diag.phase != models.MatchPhaseVariable {
			t.Fatalf("want a variable miss, got answered=%v %+v", ok, diag)
		}
		rep := h.buildHTTPMismatchReport(makeReq("POST", path), []byte(body), db, nil, nil, nil, diag)
		return rep, models.UnmatchedCall{Protocol: rep.Protocol, MatchPhase: rep.MatchPhase, ClosestMock: rep.ClosestMock,
			Diff: rep.Diff, NextSteps: rep.NextSteps, FieldDiffs: rep.FieldDiffs, Given: rep.Given}.Sanitized()
	}
	for _, tc := range []struct {
		name, gave, body, next string
	}{
		{"another value", "app-9", `{"name":"app-8"}`, "the application sent another value at body than given:app, the value the test gave: fix the application"},
		{"another JSON type", "app-9", `{"name":8}`, "the application sent given:app, the value the test gave, at body as a value of another JSON type than mock-1 holds there"},
		{"not given", "", `{"name":"app-9"}`, "the test gave given:app when mock-1 was recorded, and this run's test did not give it before the application made this call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep, ev := evidence(gave(tc.gave), "/apps", tc.body)
			if rep.Given != "given:app" || ev.Given != "given:app" {
				t.Fatalf("the miss names given %q, its evidence %q", rep.Given, ev.Given)
			}
			if !strings.HasPrefix(ev.NextSteps, tc.next) {
				t.Fatalf("evidence's next step %q, want %q", ev.NextSteps, tc.next)
			}
			for _, v := range []string{"app-1", "app-8", "app-9", "name"} {
				if strings.Contains(ev.NextSteps, v) {
					t.Fatalf("evidence's next step %q holds %q", ev.NextSteps, v)
				}
			}
		})
	}
	// The three read differently: a miss of each kind says which.
	_, other := evidence(gave("app-9"), "/apps", `{"name":"app-8"}`)
	_, none := evidence(gave(""), "/apps", `{"name":"app-9"}`)
	if other.NextSteps == none.NextSteps {
		t.Fatalf("another value and no given read alike: %q", other.NextSteps)
	}
	// A learned variable's miss keeps no variable's name.
	m := at(varMock("mock-2", "POST", "http://store/orders", `{"id":"123","item":"pen"}`, 201, `{}`,
		models.MockVariables{"orderId": bindV("123", "body.id")}), 1)
	rep, ev := evidence(&varDb{perTest: []*models.Mock{m}}, "/orders", `{"id":456,"item":"pen"}`)
	if rep.Given != "" || ev.Given != "" || strings.Contains(ev.NextSteps, "orderId") {
		t.Fatalf("a learned variable's miss: given %q, evidence %+v", rep.Given, ev)
	}
}
