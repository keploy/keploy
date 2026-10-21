package http

import (
	"reflect"
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// suiteRecording is a test that creates a suite under a name it mints, then
// renames it to that name with a suffix, then reads a diff whose answer
// holds the name whole, the new name whole, and the new name inside a path.
func suiteRecording(name string) []*models.Mock {
	return []*models.Mock{
		gql("mock-0", 0, `{"operationName":"CreateTestSuite","variables":{"input":{"name":"`+name+`"}}}`, `{"data":{"createTestSuite":{"id":"s-1"}}}`),
		gql("mock-1", 1, `{"operationName":"UpdateTestSuite","variables":{"id":"s-1","in":{"name":"`+name+`-renamed"}}}`, `{"data":{"updateTestSuite":{"ok":true}}}`),
		gql("mock-2", 2, `{"operationName":"GetBranchDiff","variables":{}}`, diffAnswer(name)),
	}
}

func diffAnswer(name string) string {
	return `{"data":{"diff":[{"entityName":"` + name + `-renamed","mainValue":"` + name + `","branchValue":"` + name + `-renamed","path":"Test Suites/` + name + `-renamed"}]}}`
}

func suiteCalls(name, renamed string) []*req {
	return []*req{
		gqlLive(`{"operationName":"CreateTestSuite","variables":{"input":{"name":"` + name + `"}}}`),
		gqlLive(`{"operationName":"UpdateTestSuite","variables":{"id":"s-1","in":{"name":"` + renamed + `"}}}`),
		gqlLive(`{"operationName":"GetBranchDiff","variables":{}}`),
	}
}

func copies(recs []*models.Mock) []*models.Mock {
	var out []*models.Mock
	for _, r := range recs {
		cp := *r
		out = append(out, &cp)
	}
	return out
}

// A suite the test creates under a name it mints and then renames to
// "<name>-renamed" (atg-features' branch and diff specs): the rename sends
// the name inside a longer value. F follows it as the name's use, bound at
// the create where it was sent whole, and answers the later diff with this
// run's name wherever its answer holds it, whole or as a token of a longer
// text. The replays after F answer the rename only when it sends that text
// with the name the test created, exactly.
func TestLearnMatchARenameCarriesTheNameItWasCreatedWith(t *testing.T) {
	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	recs := suiteRecording(recordedName)
	db := &learnDb{perTest: copies(recs), observe: tr.ServedMock}
	var served []*models.Mock
	for i, c := range suiteCalls(fName, fName+"-renamed") {
		out, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || miss != nil || out == nil {
			t.Fatalf("F call %d: served %v miss %+v err %v", i, out, miss, err)
		}
		served = append(served, out)
	}
	if got, want := served[2].Spec.HTTPResp.Body, diffAnswer(fName); got != want {
		t.Fatalf("F answered the diff with %s, want %s", got, want)
	}
	bind := map[string]models.MockVariables{"mock-0": {"name": {Recorded: recordedName, Bind: "body.variables.input.name"}}}
	rename := db.consumed[1].Spec
	if want := (models.MockVariables{"name": {Recorded: recordedName, Use: []string{"body.variables.in.name"}}}); !reflect.DeepEqual(rename.Learned(), want) || len(rename.ReqBodyNoise) != 0 {
		t.Fatalf("the rename learned %+v and noise %v, want %+v", rename.Learned(), rename.ReqBodyNoise, want)
	}
	if !reflect.DeepEqual(rename.LearnedBinds(), bind) {
		t.Fatalf("the rename's binds %+v, want %+v", rename.LearnedBinds(), bind)
	}
	diff := db.consumed[2].Spec.Learned()["name"]
	answers := slices.Sorted(slices.Values(diff.Answer))
	if want := []string{"body.data.diff[0].branchValue", "body.data.diff[0].entityName", "body.data.diff[0].mainValue", "body.data.diff[0].path"}; diff.Recorded != recordedName || !reflect.DeepEqual(answers, want) {
		t.Fatalf("the diff learned %+v, want the name answered at %v", diff, want)
	}

	// The replays after F.
	mocks := learnedOnto(recs, db.consumed)
	if want := (models.MockVariables{"name": {Recorded: recordedName, Bind: "body.variables.input.name"}}); !reflect.DeepEqual(mocks[0].Spec.Variables, want) {
		t.Fatalf("the create carries %+v", mocks[0].Spec.Variables)
	}
	ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	calls := suiteCalls(ciName, ciName+"-renamed")
	for i, c := range calls {
		ok, stub, diag := matchVar(t, h, ci, c)
		if !ok {
			t.Fatalf("CI call %d: %+v", i, diag)
		}
		if i == 2 && stub.Spec.HTTPResp.Body != diffAnswer(ciName) {
			t.Fatalf("the diff in CI: %s, want %s", stub.Spec.HTTPResp.Body, diffAnswer(ciName))
		}
	}

	// A rename to another suite's name (the recording's) is a miss whose
	// evidence is the text the name makes, never answered.
	ci = &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	calls = suiteCalls(ciName, recordedName+"-renamed")
	if ok, _, diag := matchVar(t, h, ci, calls[0]); !ok {
		t.Fatalf("the create in CI: %+v", diag)
	}
	ok, _, diag := matchVar(t, h, ci, calls[1])
	if p := wantProblem(t, ok, diag, "so this call sends"); p.expected != ciName+"-renamed" || p.actual != recordedName+"-renamed" {
		t.Fatalf("evidence expected %q actual %q", p.expected, p.actual)
	}
}

// A later call that sends the renamed text again (a read by the new name)
// carries the name the rename carried: the rename's place holds the name
// inside a longer value, no value of its own to bind (withinPlaces), so the
// read uses the name too, bound at the create. No second variable is bound
// at the rename, which would declare its place twice and leave it no
// replay could answer.
func TestLearnMatchATextSentAgainAfterARenameUsesTheName(t *testing.T) {
	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	read := func(name string) string {
		return `{"operationName":"GetTestSuite","variables":{"name":"` + name + `-renamed"}}`
	}
	recs := []*models.Mock{
		suiteRecording(recordedName)[0],
		suiteRecording(recordedName)[1],
		gql("mock-2", 2, read(recordedName), `{"data":{"testSuite":{"name":"`+recordedName+`-renamed"}}}`),
	}
	db := &learnDb{perTest: copies(recs), observe: tr.ServedMock}
	calls := func(name string) []*req {
		return append(suiteCalls(name, name+"-renamed")[:2], gqlLive(read(name)))
	}
	for i, c := range calls(fName) {
		out, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || miss != nil || out == nil {
			t.Fatalf("F call %d: served %v miss %+v err %v", i, out, miss, err)
		}
		if i == 2 && out.Spec.HTTPResp.Body != `{"data":{"testSuite":{"name":"`+fName+`-renamed"}}}` {
			t.Fatalf("F answered the read with %s", out.Spec.HTTPResp.Body)
		}
	}
	get := db.consumed[2].Spec
	if want := (models.MockVariables{"name": {Recorded: recordedName, Use: []string{"body.variables.name"}, Answer: []string{"body.data.testSuite.name"}}}); !reflect.DeepEqual(get.Learned(), want) {
		t.Fatalf("the read learned %+v, want %+v", get.Learned(), want)
	}
	if want := (map[string]models.MockVariables{"mock-0": {"name": {Recorded: recordedName, Bind: "body.variables.input.name"}}}); !reflect.DeepEqual(get.LearnedBinds(), want) {
		t.Fatalf("the read's binds %+v, want the create's alone", get.LearnedBinds())
	}

	mocks := learnedOnto(recs, db.consumed)
	if _, why := readDecls(mocks[1]); why != "" {
		t.Fatalf("the rename's variables cannot be followed: %s", why)
	}
	ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	for i, c := range calls(ciName) {
		ok, stub, diag := matchVar(t, h, ci, c)
		if !ok {
			t.Fatalf("CI call %d: %+v", i, diag)
		}
		if i == 2 && stub.Spec.HTTPResp.Body != `{"data":{"testSuite":{"name":"`+ciName+`-renamed"}}}` {
			t.Fatalf("the read in CI: %s", stub.Spec.HTTPResp.Body)
		}
	}
}

// One answer text that holds two variables as tokens ("Test Suites/<suite>/
// <case>") holds both values in their places, put in one reading; a text
// that does not hold a variable's recorded value is not answered.
func TestVariables_TwoVariablesInOneAnswerText(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	mocks := func(path string) []*models.Mock {
		return []*models.Mock{
			at(varMock("mock-1", "POST", "http://store/cases", `{"suite":"suite-aaaaaa","case":"case-bbbbbb"}`, 201, `{"path":"`+path+`"}`,
				models.MockVariables{
					"suite": bindV("suite-aaaaaa", "body.suite", "body.path"),
					"case":  bindV("case-bbbbbb", "body.case", "body.path"),
				}), 1),
		}
	}
	db := &varDb{perTest: mocks("Test Suites/suite-aaaaaa/case-bbbbbb")}
	ok, stub, diag := matchVar(t, h, db, liveReq("POST", "/cases", `{"suite":"suite-cccccc","case":"case-dddddd"}`))
	if !ok || stub.Spec.HTTPResp.Body != `{"path":"Test Suites/suite-cccccc/case-dddddd"}` {
		t.Fatalf("ok %v diag %+v", ok, diag)
	}

	db = &varDb{perTest: mocks("Test Suites/suite-aaaaaa")}
	ok, _, diag = matchVar(t, h, db, liveReq("POST", "/cases", `{"suite":"suite-cccccc","case":"case-dddddd"}`))
	wantProblem(t, ok, diag, "does not hold variable case's recorded value")
}
