package http

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// gql is a recorded GraphQL call of the test (POST /query), at seconds into
// the recording.
func gql(name string, at int, body, answer string) *models.Mock {
	return learnRecorded(name, time.Duration(at)*time.Second, "POST", "/query", body, nil, answer)
}

func gqlLive(body string) *req { return live("POST", "/query", body, nil) }

// inTest is the learn step's context with a test open, and its tracker.
func inTest() (context.Context, *learn.Tracker) {
	tr := learn.NewTracker()
	tr.Begin()
	return learn.WithTracker(context.Background(), tr), tr
}

const (
	recordedName = "e2e-review-changes-1792226022975-yh0x34"
	fName        = "e2e-review-changes-1792226273167-fprvrx"
	ciName       = "e2e-review-changes-1792229999999-qq77aa"
)

func branchCalls(name string) []*req {
	return []*req{
		gqlLive(`{"operationName":"createBranch","variables":{"input":{"name":"` + name + `"}}}`),
		gqlLive(`{"operationName":"GetBranches","variables":{}}`),
		gqlLive(`{"operationName":"deleteBranch","variables":{"name":"` + name + `"}}`),
	}
}

func branchRecording() []*models.Mock {
	return []*models.Mock{
		gql("mock-0", 0, `{"operationName":"createBranch","variables":{"input":{"name":"`+recordedName+`"}}}`, `{"data":{"createBranch":{"ok":true}}}`),
		gql("mock-1", 1, `{"operationName":"GetBranches","variables":{}}`,
			`{"data":{"branches":[{"name":"main"},{"name":"`+recordedName+`","title":"`+recordedName+` (draft)"}]}}`),
		gql("mock-2", 2, `{"operationName":"deleteBranch","variables":{"name":"`+recordedName+`"}}`, `{"data":{"deleteBranch":true}}`),
	}
}

// Problem A: a name the test mints (Date.now and a random suffix) is sent
// first in a request (learned there as noise: the call's answer does not
// echo it), and a later call of the same test, which sends nothing of it,
// answers it whole. F answers that later call with this run's name (a word
// of it inside a longer text stays as recorded), and learns it: the name is
// the test's variable, bound where it first appeared, answered at the later
// call, and used by a call that sends it again. The replays after F follow
// it: the later answer holds that run's name.
func TestLearnMatchALaterAnswerOfAValueTheTestMadeHoldsThisRunsValue(t *testing.T) {
	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	recs := branchRecording()
	var per []*models.Mock
	for _, r := range recs {
		cp := *r
		per = append(per, &cp)
	}
	db := &learnDb{perTest: per, observe: tr.ServedMock}
	var served []*models.Mock
	for i, c := range branchCalls(fName) {
		out, miss, err := h.learnMatch(ctx, c, db, n, opts)
		if err != nil || miss != nil || out == nil {
			t.Fatalf("F call %d: served %v miss %+v err %v", i, out, miss, err)
		}
		served = append(served, out)
	}
	if want := `{"data":{"branches":[{"name":"main"},{"name":"` + fName + `","title":"` + recordedName + ` (draft)"}]}}`; served[1].Spec.HTTPResp.Body != want {
		t.Fatalf("F answered the list with %s, want %s", served[1].Spec.HTTPResp.Body, want)
	}
	if len(db.consumed) != 3 {
		t.Fatalf("consumed %d mocks", len(db.consumed))
	}
	create, list, del := db.consumed[0], db.consumed[1], db.consumed[2]
	if !reflect.DeepEqual(create.Spec.ReqBodyNoise, map[string][]string{"body.variables.input.name": {}}) {
		t.Fatalf("the create learned %v: noise until a later call shows it flowed on", create.Spec.ReqBodyNoise)
	}
	bind := map[string]models.MockVariables{"mock-0": {"name": {Recorded: recordedName, Bind: "body.variables.input.name"}}}
	if want := (models.MockVariables{"name": {Recorded: recordedName, Answer: []string{"body.data.branches[1].name"}}}); !reflect.DeepEqual(list.Spec.Learned(), want) {
		t.Fatalf("the list learned %+v, want %+v", list.Spec.Learned(), want)
	}
	if !reflect.DeepEqual(list.Spec.LearnedBinds(), bind) {
		t.Fatalf("the list's binds %+v, want %+v", list.Spec.LearnedBinds(), bind)
	}
	if want := (models.MockVariables{"name": {Recorded: recordedName, Use: []string{"body.variables.name"}}}); !reflect.DeepEqual(del.Spec.Learned(), want) {
		t.Fatalf("the delete learned %+v, want %+v", del.Spec.Learned(), want)
	}
	if !reflect.DeepEqual(del.Spec.LearnedBinds(), bind) {
		t.Fatalf("the delete's binds %+v, want %+v", del.Spec.LearnedBinds(), bind)
	}

	// The replays after F.
	mocks := learnedOnto(recs, db.consumed)
	if want := (models.MockVariables{"name": {Recorded: recordedName, Bind: "body.variables.input.name"}}); !reflect.DeepEqual(mocks[0].Spec.Variables, want) || len(mocks[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("the create carries %+v and noise %v", mocks[0].Spec.Variables, mocks[0].Spec.ReqBodyNoise)
	}
	ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	calls := branchCalls(ciName)
	if ok, _, diag := matchVar(t, h, ci, calls[0]); !ok {
		t.Fatalf("the create in CI: %+v", diag)
	}
	ok, stub, diag := matchVar(t, h, ci, calls[1])
	if want := `{"data":{"branches":[{"name":"main"},{"name":"` + ciName + `","title":"` + recordedName + ` (draft)"}]}}`; !ok || stub.Spec.HTTPResp.Body != want {
		t.Fatalf("the list in CI: ok %v diag %+v", ok, diag)
	}
	if ok, _, diag := matchVar(t, h, ci, calls[2]); !ok {
		t.Fatalf("the delete in CI: %+v", diag)
	}

	// A list before the create has no name to answer: a miss whose evidence
	// is the variable, never the recorded name served.
	ci = &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	ok, _, diag = matchVar(t, h, ci, calls[1])
	wantProblem(t, ok, diag, "before the call that binds it")
}

// An echo of the call that first sent the value (a label the create
// answers) is one variable with a later call's answer that holds it: bound
// at the create, no longer an echo of that call alone, and answered at both.
func TestLearnMatchAnEchoALaterAnswerHoldsIsTheTestsVariable(t *testing.T) {
	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	recs := []*models.Mock{
		gql("mock-0", 0, `{"operationName":"createLabel","variables":{"name":"tag-5vaa"}}`, `{"data":{"createLabel":{"id":"l-1","name":"tag-5vaa"}}}`),
		gql("mock-1", 1, `{"operationName":"labels","variables":{}}`, `{"data":{"labels":[{"id":"l-0","name":"bug"},{"id":"l-1","name":"tag-5vaa"}]}}`),
	}
	var per []*models.Mock
	for _, r := range recs {
		cp := *r
		per = append(per, &cp)
	}
	db := &learnDb{perTest: per, observe: tr.ServedMock}
	calls := func(name string) []*req {
		return []*req{
			gqlLive(`{"operationName":"createLabel","variables":{"name":"` + name + `"}}`),
			gqlLive(`{"operationName":"labels","variables":{}}`),
		}
	}
	for i, c := range calls("tag-x9qq") {
		out, miss, _ := h.learnMatch(ctx, c, db, n, opts)
		if out == nil || miss != nil {
			t.Fatalf("F call %d: %+v", i, miss)
		}
		if i == 1 && out.Spec.HTTPResp.Body != `{"data":{"labels":[{"id":"l-0","name":"bug"},{"id":"l-1","name":"tag-x9qq"}]}}` {
			t.Fatalf("F answered the list with %s", out.Spec.HTTPResp.Body)
		}
	}
	if want := (models.MockVariables{"name": {Recorded: "tag-5vaa", Bind: "body.variables.name", Answer: []string{"body.data.createLabel.name"}, Echo: true}}); !reflect.DeepEqual(db.consumed[0].Spec.Learned(), want) {
		t.Fatalf("the create learned %+v", db.consumed[0].Spec.Learned())
	}
	if want := (map[string]models.MockVariables{"mock-0": {"name": {Recorded: "tag-5vaa", Bind: "body.variables.name"}}}); !reflect.DeepEqual(db.consumed[1].Spec.LearnedBinds(), want) {
		t.Fatalf("the list's binds %+v", db.consumed[1].Spec.LearnedBinds())
	}
	mocks := learnedOnto(recs, db.consumed)
	if v := mocks[0].Spec.Variables["name"]; v.Echo || v.Bind != "body.variables.name" {
		t.Fatalf("the create's variable %+v: a later call answers it, so it is no echo", v)
	}
	ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
	c := calls("tag-k2mm")
	ok, stub, diag := matchVar(t, h, ci, c[0])
	if !ok || stub.Spec.HTTPResp.Body != `{"data":{"createLabel":{"id":"l-1","name":"tag-k2mm"}}}` {
		t.Fatalf("the create in CI: ok %v diag %+v", ok, diag)
	}
	ok, stub, diag = matchVar(t, h, ci, c[1])
	if !ok || stub.Spec.HTTPResp.Body != `{"data":{"labels":[{"id":"l-0","name":"bug"},{"id":"l-1","name":"tag-k2mm"}]}}` {
		t.Fatalf("the list in CI: ok %v diag %+v", ok, diag)
	}
}

// What a later answer holds of a value the test made is answered only where
// the answer holds it whole, at a scalar of its JSON body of the type the
// request sent it as: not inside a longer text, in another case, at an
// object key, in a header, or as a number where it was sent as a string.
// Such an answer is served as recorded, and nothing is learned on it.
func TestLearnMatchALaterAnswerHoldsAValueTheTestMadeOnlyWhole(t *testing.T) {
	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	list := gql("mock-1", 1, `{"operationName":"items","variables":{}}`,
		`{"data":{"title":"n-1111 copy","code":"N-1111","byName":{"n-1111":{"ok":true}},"num":1111}}`)
	list.Spec.HTTPResp.Header["X-Name"] = "n-1111"
	db := &learnDb{perTest: []*models.Mock{
		gql("mock-0", 0, `{"operationName":"create","variables":{"input":{"name":"n-1111","n":"1111"}}}`, `{"data":{"ok":true}}`),
		list,
	}, observe: tr.ServedMock}
	if out, miss, _ := h.learnMatch(ctx, gqlLive(`{"operationName":"create","variables":{"input":{"name":"n-2222","n":"2222"}}}`), db, n, opts); out == nil || miss != nil {
		t.Fatalf("the create: %+v", miss)
	}
	out, miss, _ := h.learnMatch(ctx, gqlLive(`{"operationName":"items","variables":{}}`), db, n, opts)
	if out == nil || miss != nil || out.Spec.HTTPResp.Body != list.Spec.HTTPResp.Body || out.Spec.HTTPResp.Header["X-Name"] != "n-1111" {
		t.Fatalf("served %v miss %+v: the answer is served as recorded", out, miss)
	}
	if got := db.consumed[1].Spec; len(got.Learned()) != 0 || len(got.LearnedBinds()) != 0 {
		t.Fatalf("learned %+v binds %+v on an answer that holds no value whole", got.Learned(), got.LearnedBinds())
	}
}

// Only a test's own calls carry its variables: a pool recording that
// answers a value the test made is served as recorded, and nothing is
// learned onto it; nor is the next test's own recording answered with the
// previous test's value.
func TestLearnMatchAValueTheTestMadeIsNotAnsweredOutsideItsTest(t *testing.T) {
	h, n, opts := learnOn(t)
	create := func() *models.Mock {
		return gql("mock-0", 0, `{"operationName":"create","variables":{"name":"`+recordedName+`"}}`, `{"data":{"ok":true}}`)
	}
	listBody := `{"data":{"branches":[{"name":"` + recordedName + `"}]}}`

	t.Run("a pool recording", func(t *testing.T) {
		ctx, tr := inTest()
		pool := gql("pool-1", 1, `{"operationName":"list","variables":{}}`, listBody)
		pool.TestModeInfo.Lifetime = models.LifetimeSession
		db := &learnDb{mockMemDb: mockMemDb{mocks: []*models.Mock{pool}}, perTest: []*models.Mock{create()}, observe: tr.ServedMock}
		if out, miss, _ := h.learnMatch(ctx, gqlLive(`{"operationName":"create","variables":{"name":"`+fName+`"}}`), db, n, opts); out == nil || miss != nil {
			t.Fatalf("the create: %+v", miss)
		}
		out, miss, _ := h.learnMatch(ctx, gqlLive(`{"operationName":"list","variables":{}}`), db, n, opts)
		if out == nil || miss != nil || out.Spec.HTTPResp.Body != listBody {
			t.Fatalf("served %v miss %+v: the pool's answer is served as recorded", out, miss)
		}
		for _, m := range db.pooled {
			if len(m.Spec.Learned()) != 0 || len(m.Spec.LearnedBinds()) != 0 {
				t.Fatalf("learned onto the pool: %+v %+v", m.Spec.Learned(), m.Spec.LearnedBinds())
			}
		}
	})

	t.Run("the next test", func(t *testing.T) {
		ctx, tr := inTest()
		db := &learnDb{perTest: []*models.Mock{create(), gql("mock-1", 1, `{"operationName":"list","variables":{}}`, listBody)}, observe: tr.ServedMock}
		if out, miss, _ := h.learnMatch(ctx, gqlLive(`{"operationName":"create","variables":{"name":"`+fName+`"}}`), db, n, opts); out == nil || miss != nil {
			t.Fatalf("the create: %+v", miss)
		}
		tr.End()
		tr.Begin()
		out, miss, _ := h.learnMatch(ctx, gqlLive(`{"operationName":"list","variables":{}}`), db, n, opts)
		if out == nil || miss != nil || out.Spec.HTTPResp.Body != listBody {
			t.Fatalf("served %v miss %+v: another test's value is not answered", out, miss)
		}
		if got := db.consumed[1].Spec; len(got.Learned()) != 0 || len(got.LearnedBinds()) != 0 {
			t.Fatalf("learned %+v binds %+v", got.Learned(), got.LearnedBinds())
		}
	})
}
