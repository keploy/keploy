package http

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
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

// jwtFor is a signed-session-shaped token (header.claims.signature) whose
// claims say who and when, as a test that signs its own session makes one.
func jwtFor(user string, iat int, sig string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc([]byte(fmt.Sprintf(`{"userId":%q,"iat":%d,"exp":%d}`, user, iat, iat+7200))) + "." + sig
}

func sessionCookie(tok string) map[string]string {
	return map[string]string{"Cookie": "jwt_token_staging=" + tok + "; refresh_token_staging=" + tok + "; integration_testing_subscription_staging=enterprise"}
}

// A session token the test signs and installs itself (a reviewer's, minted
// for each call that acts as the reviewer) is new in every run: no recorded
// answer gave it. Its harness gives it as the test installs it, named by the
// order of the install (agent.Agent.Give), and keploy binds it as the test's
// given value. F follows it inside the Cookie header, where it stands with
// the run's other cookies: the call is the recording with this run's token in
// the recorded token's place, exactly. Two tokens the record minted in one
// second (one value) and the replay in two (two values) are two given values,
// each followed where its own install put it. The replays after F answer a
// call only when it carries the token its harness gave, exactly.
func TestLearnMatchATokenTheTestInstalledIsItsGivenValue(t *testing.T) {
	recTok := jwtFor("u-reviewer", 1790000000, "c2lnbmF0dXJlLXJlY29yZGVk")
	fA, fB := jwtFor("u-reviewer", 1790000061, "c2lnbmF0dXJlLWYtYWFhYWFh"), jwtFor("u-reviewer", 1790000062, "c2lnbmF0dXJlLWYtYmJiYmJi")
	ciA, ciB := jwtFor("u-reviewer", 1790000001, "c2lnbmF0dXJlLWNpLWFhYWFh"), jwtFor("u-reviewer", 1790000002, "c2lnbmF0dXJlLWNpLWJiYmJi")
	recs := []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/query", `{"operationName":"approve","variables":{"id":"b-1"}}`, sessionCookie(recTok), `{"data":{"approve":true}}`),
		learnRecorded("mock-1", time.Second, "POST", "/query", `{"operationName":"requestChanges","variables":{"id":"b-1"}}`, sessionCookie(recTok), `{"data":{"requestChanges":true}}`),
	}
	calls := func(a, b string) []*req {
		return []*req{
			live("POST", "/query", `{"operationName":"approve","variables":{"id":"b-1"}}`, sessionCookie(a)),
			live("POST", "/query", `{"operationName":"requestChanges","variables":{"id":"b-1"}}`, sessionCookie(b)),
		}
	}
	given := func(a, b string) []variables.Binding {
		return []variables.Binding{
			{Name: "cookie-jwt_token_staging-1", Value: a, Recorded: recTok, Mock: "the test's harness", Place: "given", Given: true},
			{Name: "cookie-jwt_token_staging-2", Value: b, Recorded: recTok, Mock: "the test's harness", Place: "given", Given: true},
		}
	}

	// Without the harness's given values the second call is refused: the
	// recording had one token where the run sent two.
	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	db := &givenDb{learnDb: &learnDb{perTest: copies(recs), observe: tr.ServedMock}}
	db.table.Start(nil)
	if out, miss, _ := h.learnMatch(ctx, calls(fA, fB)[0], db, n, opts); out == nil || miss != nil {
		t.Fatalf("control, first call: %+v", miss)
	}
	if out, miss, _ := h.learnMatch(ctx, calls(fA, fB)[1], db, n, opts); out != nil || miss == nil {
		t.Fatalf("control: the second token was learned without the harness's given value: %v", out)
	}

	// With them, F follows each token to its install.
	ctx, tr = inTest()
	db = &givenDb{learnDb: &learnDb{perTest: copies(recs), observe: tr.ServedMock}}
	db.table.Start(nil)
	for _, g := range given(fA, fB) {
		if _, ok := db.table.Bind(g); !ok {
			t.Fatalf("bind %s", g.Name)
		}
	}
	for i, c := range calls(fA, fB) {
		if out, miss, err := h.learnMatch(ctx, c, db, n, opts); err != nil || miss != nil || out == nil {
			t.Fatalf("F call %d: served %v miss %+v err %v", i, out, miss, err)
		}
	}
	for i, name := range []string{"cookie-jwt_token_staging-1", "cookie-jwt_token_staging-2"} {
		want := models.MockVariables{name: {Recorded: recTok, Use: []string{"header.cookie"}}}
		if got := db.consumed[i].Spec.Learned(); !reflect.DeepEqual(got, want) {
			t.Fatalf("mock-%d learned %+v, want %+v", i, got, want)
		}
		if len(db.consumed[i].Spec.LearnedBinds()) != 0 {
			t.Fatalf("mock-%d binds %+v: a given value is bound by its harness, at no call", i, db.consumed[i].Spec.LearnedBinds())
		}
	}

	// The replays after F.
	mocks := learnedOnto(recs, db.consumed)
	ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	ci.table.Start(nil)
	for _, g := range given(ciA, ciB) {
		ci.table.Bind(g)
	}
	for i, c := range calls(ciA, ciB) {
		if ok, _, diag := matchVar(t, h, ci, c); !ok {
			t.Fatalf("CI call %d: %+v", i, diag)
		}
	}
	// The recording's token, or the other install's, is not this call's.
	for _, wrong := range []string{recTok, ciB} {
		ci = &varDb{perTest: append([]*models.Mock(nil), mocks...)}
		ci.table.Start(nil)
		for _, g := range given(ciA, ciB) {
			ci.table.Bind(g)
		}
		ok, _, diag := matchVar(t, h, ci, calls(wrong, ciB)[0])
		wantProblem(t, ok, diag, "so this call sends")
	}
}

// A session a group's beforeAll signs (outside every test) and bakes into the
// API context its tests seed with is the claim key's given value: its harness
// gave it while no test was open, and keploy bound it in the key's scope. A
// test's own call that carries it uses it as the key's (Outside), and the
// replays after F read it in the key's scope; the key's calls outside its
// tests (the beforeAll's own seeding) carry it too, and no other call does.
func TestLearnMatchASessionTheBeforeAllSignedIsTheKeysGivenValue(t *testing.T) {
	recTok := jwtFor("u-admin", 1790000000, "c2lnbmF0dXJlLWJlZm9yZWFsbA")
	fTok, ciTok := jwtFor("u-admin", 1790000061, "c2lnbmF0dXJlLWYtYmVmb3JlYWw"), jwtFor("u-admin", 1790000003, "c2lnbmF0dXJlLWNpLWJlZm9yZWE")
	cookie := func(tok string) map[string]string {
		return map[string]string{"Cookie": "refresh_token_staging=r-stable-refresh; jwt_token_staging=" + tok}
	}
	seed := `{"query":"mutation($name: String!) { createATGApp(name: $name) }","variables":{"name":"e2e-app"}}`
	recs := []*models.Mock{learnRecorded("mock-0", 0, "POST", "/query", seed, cookie(recTok), `{"data":{"createATGApp":"app-1"}}`)}
	keyGiven := func(live string) variables.Binding {
		return variables.Binding{Name: "outside-cookie-jwt_token_staging-1", Value: live, Recorded: recTok, Mock: "the claim key's harness", Place: "given", Given: true, Outside: true}
	}
	bindKey := func(t *testing.T, table *variables.Table, live string) {
		t.Helper()
		table.Start(nil)
		if _, _, _, ok := table.BindAll(table.Gen(), []variables.Binding{keyGiven(live)}); !ok {
			t.Fatal("bind the key's given value")
		}
	}

	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	db := &givenDb{learnDb: &learnDb{perTest: copies(recs), observe: tr.ServedMock}}
	bindKey(t, &db.table, fTok)
	if out, miss, err := h.learnMatch(ctx, live("POST", "/query", seed, cookie(fTok)), db, n, opts); err != nil || miss != nil || out == nil {
		t.Fatalf("F: served %v miss %+v err %v", out, miss, err)
	}
	want := models.MockVariables{"outside-cookie-jwt_token_staging-1": {Recorded: recTok, Use: []string{"header.cookie"}, Outside: true}}
	if got := db.consumed[0].Spec.Learned(); !reflect.DeepEqual(got, want) {
		t.Fatalf("learned %+v, want %+v", got, want)
	}

	mocks := learnedOnto(recs, db.consumed)
	ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	bindKey(t, &ci.table, ciTok)
	if ok, _, diag := matchVar(t, h, ci, live("POST", "/query", seed, cookie(ciTok))); !ok {
		t.Fatalf("CI: %+v", diag)
	}
	ci = &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	bindKey(t, &ci.table, ciTok)
	ok, _, diag := matchVar(t, h, ci, live("POST", "/query", seed, cookie(fTok)))
	wantProblem(t, ok, diag, "so this call sends")

	// The key's call outside its tests (the beforeAll's own seeding) carries
	// it; a call that is neither the test's own nor the key's does not.
	learned := []learn.Learned{{Kind: learn.KindNoise,
		Recorded: learn.Leaf{Path: "header.cookie", Type: learn.String, Value: cookie(recTok)["Cookie"]},
		Live:     learn.Leaf{Path: "header.cookie", Type: learn.String, Value: cookie(fTok)["Cookie"]}}}
	given := []variables.Binding{keyGiven(fTok)}
	p, why := learnPlan(nil, 0, recs[0], learn.Shape{}, false, true, learned, nil, given)
	if why != "" || !reflect.DeepEqual(p.vars, want) {
		t.Fatalf("the key's call outside its tests: why %q vars %+v", why, p.vars)
	}
	if _, why := learnPlan(nil, 0, recs[0], learn.Shape{}, false, false, learned, nil, given); why == "" {
		t.Fatal("a call of no test and no claim key carried the key's given value")
	}
}

// A helper that signs a session for each call (a cleanup afterAll's
// per-request Cookie) gives a value right before each call. Signed in one
// second, they are one value in the record and in F; a replay that signs them
// in two seconds holds two. Each call carries its own install, the latest
// given before it: the learn step pairs it so, and the replays after F answer
// each call only with its own install's value.
func TestLearnMatchACallCarriesTheLatestInstallOfOneValue(t *testing.T) {
	recTok := jwtFor("u-admin", 1790000000, "c2lnbmF0dXJlLWNsZWFudXAtcmVj")
	fTok := jwtFor("u-admin", 1790000061, "c2lnbmF0dXJlLWNsZWFudXAtZmZm")
	ciA, ciB := jwtFor("u-admin", 1790000001, "c2lnbmF0dXJlLWNsZWFudXAtYWFh"), jwtFor("u-admin", 1790000002, "c2lnbmF0dXJlLWNsZWFudXAtYmJi")
	cookie := func(tok string) map[string]string { return map[string]string{"Cookie": "jwt_token_staging=" + tok} }
	del := func(id string) string {
		return `{"query":"mutation($id: String!) { deleteRole(id: $id) }","variables":{"id":"` + id + `"}}`
	}
	var recs []*models.Mock
	for i, id := range []string{"r-1", "r-2", "r-3"} {
		recs = append(recs, learnRecorded(fmt.Sprintf("mock-%d", i), time.Duration(i)*time.Second, "POST", "/query", del(id), cookie(recTok), `{"data":{"deleteRole":true}}`))
	}
	install := func(n int, live string) variables.Binding {
		return variables.Binding{Name: fmt.Sprintf("cookie-jwt_token_staging-%d", n), Value: live, Recorded: recTok, Mock: "the test's harness", Place: "given", Given: true}
	}

	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	db := &givenDb{learnDb: &learnDb{perTest: copies(recs), observe: tr.ServedMock}}
	db.table.Start(nil)
	for i, id := range []string{"r-1", "r-2", "r-3"} {
		db.table.Bind(install(i+1, fTok))
		if out, miss, err := h.learnMatch(ctx, live("POST", "/query", del(id), cookie(fTok)), db, n, opts); err != nil || miss != nil || out == nil {
			t.Fatalf("F call %d: served %v miss %+v err %v", i, out, miss, err)
		}
	}
	for i := range recs {
		name := fmt.Sprintf("cookie-jwt_token_staging-%d", i+1)
		if want := (models.MockVariables{name: {Recorded: recTok, Use: []string{"header.cookie"}}}); !reflect.DeepEqual(db.consumed[i].Spec.Learned(), want) {
			t.Fatalf("mock-%d learned %+v, want its own install %s", i, db.consumed[i].Spec.Learned(), name)
		}
	}

	mocks := learnedOnto(recs, db.consumed)
	ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	ci.table.Start(nil)
	for i, tok := range []string{ciA, ciB, ciB} {
		ci.table.Bind(install(i+1, tok))
		if ok, _, diag := matchVar(t, h, ci, live("POST", "/query", del(fmt.Sprintf("r-%d", i+1)), cookie(tok))); !ok {
			t.Fatalf("CI call %d: %+v", i, diag)
		}
	}
}
