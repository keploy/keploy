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
