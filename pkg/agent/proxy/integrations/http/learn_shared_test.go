package http

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A test that names a role and a case after one Date.now() (atg-features'
// Smart Test Set and RBAC report-list specs: "e2e-role-<ts>", then
// "e2e-case-<ts>"), with F's time out of the lead's reach (a slow call before
// the read moved it 62.7 s), so no clock read explains it.
const (
	sharedRec = "1792678689546"
	sharedF   = "1792678752275"
	sharedCI  = "1792679999001"
)

func roleBody(ts string) string {
	return `{"operationName":"SeedRole","variables":{"input":{"name":"e2e-role-` + ts + `"}}}`
}

func caseBody(ts string) string {
	return `{"operationName":"DeleteCase","variables":{"name":"e2e-case-` + ts + `"}}`
}

func sharedRecording() []*models.Mock {
	return []*models.Mock{
		gql("mock-0", 0, roleBody(sharedRec), `{"data":{"createRole":{"id":"r-1","name":"e2e-role-`+sharedRec+`"}}}`),
		gql("mock-1", 1, caseBody(sharedRec), `{"data":{"deleteCase":true}}`),
	}
}

// The case name carries a word of the role name, not the role name: F
// follows the word (the time) as a variable of its own, bound inside the role
// name where it first stands, and used inside the case name. The replays
// after F answer the case only when it sends that text with the time the
// role was named with, exactly.
func TestLearnMatchTwoNamesOfOneTimeShareTheTimeAsAWord(t *testing.T) {
	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	recs := sharedRecording()
	db := &learnDb{perTest: copies(recs), observe: tr.ServedMock}
	for i, c := range []*req{gqlLive(roleBody(sharedF)), gqlLive(caseBody(sharedF))} {
		out, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || miss != nil || out == nil {
			t.Fatalf("F call %d: served %v miss %+v err %v", i, out, miss, err)
		}
	}
	del := db.consumed[1].Spec
	if want := (models.MockVariables{"name-part": {Recorded: sharedRec, Use: []string{"body.variables.name"}}}); !reflect.DeepEqual(del.Learned(), want) {
		t.Fatalf("the case learned %+v, want %+v", del.Learned(), want)
	}
	if want := (map[string]models.MockVariables{"mock-0": {"name-part": {Recorded: sharedRec, Bind: "body.variables.input.name"}}}); !reflect.DeepEqual(del.LearnedBinds(), want) {
		t.Fatalf("the case's binds %+v, want %+v", del.LearnedBinds(), want)
	}

	// The replays after F: the role's place holds the role name whole (its
	// echo) and the time inside it.
	mocks := learnedOnto(recs, db.consumed)
	if v := mocks[0].Spec.Variables["name-part"]; v.Recorded != sharedRec || v.Bind != "body.variables.input.name" {
		t.Fatalf("the role carries %+v", mocks[0].Spec.Variables)
	}
	ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	for i, c := range []*req{gqlLive(roleBody(sharedCI)), gqlLive(caseBody(sharedCI))} {
		if ok, _, diag := matchVar(t, h, ci, c); !ok {
			t.Fatalf("CI call %d: %+v", i, diag)
		}
	}

	// A case named with another time than the role's is a miss whose
	// evidence is the text the role's time makes, never answered.
	ci = &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	if ok, _, diag := matchVar(t, h, ci, gqlLive(roleBody(sharedCI))); !ok {
		t.Fatalf("the role in CI: %+v", diag)
	}
	ok, _, diag := matchVar(t, h, ci, gqlLive(caseBody(sharedRec)))
	if p := wantProblem(t, ok, diag, "so this call sends"); p.expected != "e2e-case-"+sharedCI || p.actual != "e2e-case-"+sharedRec {
		t.Fatalf("evidence expected %q actual %q", p.expected, p.actual)
	}
}

// A role named with more changed than the time (another prefix) binds no
// word of it: the bind reads the time inside the recorded text only where
// the call sends that text with one value in its place.
func TestVariables_AWordBoundInsideALongerValueNeedsTheRestOfTheText(t *testing.T) {
	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	recs := sharedRecording()
	db := &learnDb{perTest: copies(recs), observe: tr.ServedMock}
	for i, c := range []*req{gqlLive(roleBody(sharedF)), gqlLive(caseBody(sharedF))} {
		if out, miss, err := h.learnMatch(ctx, c, db, n, opts); err != nil || miss != nil || out == nil {
			t.Fatalf("F call %d: served %v miss %+v err %v", i, out, miss, err)
		}
	}
	mocks := learnedOnto(recs, db.consumed)
	ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	other := `{"operationName":"SeedRole","variables":{"input":{"name":"e2e-admin-` + sharedCI + `"}}}`
	if ok, _, _ := matchVar(t, h, ci, gqlLive(other)); ok {
		t.Fatal("a role of another name was answered by the role's recording")
	}
}
