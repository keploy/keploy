package http

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// boundInOf is the miss report of the variable miss diag (the one a live
// call method rawURL got): what the replay carries of it.
func boundInOf(t *testing.T, db *varDb, diag *matchDiag, method, rawURL string) *models.MockMismatchReport {
	t.Helper()
	request, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := variableMismatchReport(request, nil, "store", diag, nil, db)
	return r
}

// A variable miss names where this run bound each variable its refusal
// read (MockMismatchReport.BoundIn), by the name the binding carries: a
// reader of the miss (keploy mock judge's roll back) tells by it which
// recordings shaped the refusal beside the closest one. A given is the
// test's own value, bound at no recording, and names none.
func TestVariables_AVariableMissNamesWhereItsVariablesWereBound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t.Run("another-value-than-the-run-bound", func(t *testing.T) {
		h := newHTTP()
		db := &varDb{perTest: createAndReadBack("ord-rec-1")}
		if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"ord-live-9","item":"pen"}`)); !ok {
			t.Fatal("the create should match")
		}
		ok, _, diag := matchVar(t, h, db, liveReq("GET", "/orders/ord-other-3", ""))
		p := wantProblem(t, ok, diag, "this call sent")
		if p.mock != "mock-2" || !slices.Equal(p.boundIn, []string{"mock-1"}) {
			t.Fatalf("refused as %s with the variables bound in %v; want mock-2, orderId bound in mock-1", p.mock, p.boundIn)
		}
		if r := boundInOf(t, db, diag, "GET", "http://store/orders/ord-other-3"); r.ClosestMock != "mock-2" || !slices.Equal(r.BoundIn, []string{"mock-1"}) {
			t.Fatalf("the miss names %s bound in %v; want mock-2, bound in mock-1", r.ClosestMock, r.BoundIn)
		}
	})
	t.Run("another-variables-value", func(t *testing.T) {
		h := newHTTP()
		db := &varDb{perTest: []*models.Mock{
			at(varMock("mock-1", "POST", "http://store/charges", `{"key":"k1-ord7"}`, 201, `{"key":"k1"}`,
				models.MockVariables{
					"chargeKey": {Recorded: "k1", Bind: "body.key#0", Answer: []string{"body.key"}, Echo: true},
					"orderId":   bindV("ord7", "body.key#1"),
				}), 1),
			at(varMock("mock-2", "POST", "http://store/refunds", `{"ref":"ref3"}`, 201, `{}`,
				models.MockVariables{"refundId": bindV("ref3", "body.ref#0")}), 2),
		}}
		if ok, _, diag := matchVar(t, h, db, liveReq("POST", "/charges", `{"key":"k9-z9"}`)); !ok {
			t.Fatalf("the charge should be answered: %+v", diag)
		}
		ok, _, diag := matchVar(t, h, db, liveReq("POST", "/refunds", `{"ref":"z9"}`))
		p := wantProblem(t, ok, diag, "they were two values when the test was recorded")
		if p.mock != "mock-2" || !slices.Equal(p.boundIn, []string{"mock-1"}) {
			t.Fatalf("refused as %s with the variables bound in %v; want mock-2, orderId bound in mock-1", p.mock, p.boundIn)
		}
		if r := boundInOf(t, db, diag, "POST", "http://store/refunds"); !slices.Equal(r.BoundIn, []string{"mock-1"}) {
			t.Fatalf("the miss names the variables bound in %v; want mock-1", r.BoundIn)
		}
	})
	t.Run("a-given", func(t *testing.T) {
		h := newHTTP()
		db := &varDb{perTest: []*models.Mock{givenAppMock()}}
		db.table.Start(nil)
		if _, ok := db.table.Give(variables.Binding{Name: "given:app", Value: "app-9", Recorded: "app-1", Mock: "test o::TestApps", Place: "apps_test.go:20"}); !ok {
			t.Fatal("the test's given did not bind")
		}
		ok, _, diag := matchVar(t, h, db, liveReq("POST", "/apps", `{"name":"app-8"}`))
		p := wantProblem(t, ok, diag, "the application did not pass on the value the test gave")
		if len(p.boundIn) != 0 {
			t.Fatalf("a given's miss names bindings %v; want none", p.boundIn)
		}
		if r := boundInOf(t, db, diag, "POST", "http://store/apps"); len(r.BoundIn) != 0 {
			t.Fatalf("a given's miss report names bindings %v; want none", r.BoundIn)
		}
	})
}

// boundAt adds each name once, and never "".
func TestVarProblemBoundAtAddsEachNameOnce(t *testing.T) {
	p := (&varProblem{}).boundAt("mock-1").boundAt("").boundAt("mock-1").boundAt("mock-2")
	if !slices.Equal(p.boundIn, []string{"mock-1", "mock-2"}) {
		t.Fatalf("boundIn %v; want mock-1, mock-2", p.boundIn)
	}
}

// A bind the table refuses beside a binding the check read (the check and
// the bind disagree, a keploy bug) names where that binding was bound.
func TestVariables_ABindRefusedBesideABindingItReadNamesWhereThatWasBound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	first := reusable(at(varMock("mock-1", "POST", "http://store/orders", `{"id":"a-rec"}`, 201, `{}`,
		models.MockVariables{"first": bindV("a-rec", "body.id")}), 1))
	second := reusable(at(varMock("mock-2", "POST", "http://store/orders", `{"id":"b-rec"}`, 201, `{}`,
		models.MockVariables{"second": bindV("b-rec", "body.id")}), 2))
	db := &varDb{session: []*models.Mock{first, second}}
	ctx := context.Background()
	x, y := liveReq("POST", "/orders", `{"id":"x"}`), liveReq("POST", "/orders", `{"id":"y"}`)
	pool, _ := splitVariableMocks([]*models.Mock{first, second})
	hitsX, _, _ := h.variablePass(ctx, x, pool, varScope{table: &db.table, gen: db.table.Gen()}, nil, nil, nil)
	hitsY, _, _ := h.variablePass(ctx, y, pool, varScope{table: &db.table, gen: db.table.Gen()}, nil, nil, nil)
	hx, _, _ := h.pickHit(hitsX, nil, db, true, nil)
	hy, _, _ := h.pickHit(hitsY, nil, db, true, nil)
	if _, p, retry, err := h.serveVariables(ctx, hx, db, unreadPlaces); err != nil || p != nil || retry {
		t.Fatalf("x: problem %+v retry %v err %v", p, retry, err)
	}
	with, ok := db.table.Lookup("first")
	if !ok || with.Mock != "mock-1" {
		t.Fatalf("x bound first as %+v (%v); want it bound at mock-1", with, ok)
	}
	// y's check read x's binding: its bind refused beside it is no race.
	hy.saw = map[string]string{with.Name: with.Value}
	_, p, retry, err := h.serveVariables(ctx, hy, db, unreadPlaces)
	if err != nil || retry || p == nil || !strings.Contains(p.why, "though the check of the call let it") {
		t.Fatalf("y: problem %+v retry %v err %v; want the check-and-bind refusal", p, retry, err)
	}
	if !slices.Equal(p.boundIn, []string{"mock-1"}) {
		t.Fatalf("the refusal names the variables bound in %v; want mock-1", p.boundIn)
	}
}
