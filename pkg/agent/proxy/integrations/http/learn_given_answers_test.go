package http

import (
	"reflect"
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

const (
	typedOther = "filter-1792226022975"
	liveOther  = "filter-1792226273167"
)

// appList is the app list a page reads after it created an app: the app the
// test named, whole and inside a longer text, and a value the test typed but
// never sent.
func appList(name, other string) string {
	return `{"data":{"apps":[{"id":"a-0","name":"main-app"},{"id":"a-1","name":"` + name + `","title":"` + name + ` (draft)"},{"id":"a-2","name":"` + other + `"}]}}`
}

// A name the test types into the page (its harness gives it before typing it:
// typed-text-1) is sent by the create call, and a later call of the test,
// which sends nothing of it, answers it: the app list, whole and inside a
// longer text. F answers that call with this run's name, as the given name's
// variable: no call binds it, the table does, as the harness gives it in
// every run. The replays after F answer it with that run's name. A value the
// harness gave that no call of the test carried (typed-text-2: typed, never
// sent) is not answered: nothing says the application holds it.
func TestLearnMatchALaterAnswerOfATypedNameHoldsThisRunsName(t *testing.T) {
	recs := []*models.Mock{
		gql("mock-0", 0, `{"operationName":"CreateATGApp","variables":{"name":"`+recordedName+`"}}`, `{"data":{"createATGApp":{"id":"a-1"}}}`),
		gql("mock-1", 1, `{"operationName":"MyApps","variables":{}}`, appList(recordedName, typedOther)),
	}
	calls := func(name string) []*req {
		return []*req{
			gqlLive(`{"operationName":"CreateATGApp","variables":{"name":"` + name + `"}}`),
			gqlLive(`{"operationName":"MyApps","variables":{}}`),
		}
	}
	given := func(name, other string) []variables.Binding {
		return []variables.Binding{
			{Name: "typed-text-1", Value: name, Recorded: recordedName, Mock: "the test's harness", Place: "given", Given: true},
			{Name: "typed-text-2", Value: other, Recorded: typedOther, Mock: "the test's harness", Place: "given", Given: true},
		}
	}

	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	db := &givenDb{learnDb: &learnDb{perTest: copies(recs), observe: tr.ServedMock}}
	db.table.Start(nil)
	for _, g := range given(fName, liveOther) {
		if _, ok := db.table.Bind(g); !ok {
			t.Fatalf("bind %s", g.Name)
		}
	}
	var served []*models.Mock
	for i, c := range calls(fName) {
		out, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || miss != nil || out == nil {
			t.Fatalf("F call %d: served %v miss %+v err %v", i, out, miss, err)
		}
		served = append(served, out)
	}
	if got, want := served[1].Spec.HTTPResp.Body, appList(fName, typedOther); got != want {
		t.Fatalf("F answered the app list with %s, want %s", got, want)
	}
	create := db.consumed[0].Spec
	if want := (models.MockVariables{"typed-text-1": {Recorded: recordedName, Use: []string{"body.variables.name"}}}); !reflect.DeepEqual(create.Learned(), want) {
		t.Fatalf("the create learned %+v, want %+v", create.Learned(), want)
	}
	list := db.consumed[1].Spec
	got := list.Learned()["typed-text-1"]
	if want := []string{"body.data.apps[1].name", "body.data.apps[1].title"}; got.Recorded != recordedName || len(got.Use) != 0 || got.Bind != "" || !reflect.DeepEqual(slices.Sorted(slices.Values(got.Answer)), want) {
		t.Fatalf("the app list learned %+v, want typed-text-1 answered at %v", got, want)
	}
	if len(list.Learned()) != 1 {
		t.Fatalf("the app list learned %+v: only typed-text-1, which a call carried", list.Learned())
	}
	for i, c := range db.consumed {
		if len(c.Spec.LearnedBinds()) != 0 {
			t.Fatalf("mock-%d binds %+v: a given value is bound by its harness, at no call", i, c.Spec.LearnedBinds())
		}
	}

	// The replays after F.
	mocks := learnedOnto(recs, db.consumed)
	ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	ci.table.Start(nil)
	for _, g := range given(ciName, "filter-1792229999999") {
		ci.table.Bind(g)
	}
	for i, c := range calls(ciName) {
		ok, stub, diag := matchVar(t, h, ci, c)
		if !ok {
			t.Fatalf("CI call %d: %+v", i, diag)
		}
		if i == 1 && stub.Spec.HTTPResp.Body != appList(ciName, typedOther) {
			t.Fatalf("the app list in CI: %s, want %s", stub.Spec.HTTPResp.Body, appList(ciName, typedOther))
		}
	}
}
