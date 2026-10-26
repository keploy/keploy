package http

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// A cleanup that signs an admin session for each call (a group's afterAll
// deleting what it seeded) was recorded with tokens signed in one second. F
// ran on the moved clock and signed them in that same second: each call is
// exactly its recording, and nothing drifted. F follows each token all the
// same, as the value its harness gave before that call (the latest install
// so far), so a run that signs others (CI, a later round) is answered, each
// call with its own install's token. Before, F learned nothing there, and
// such a run was refused at every call.
func TestLearnMatchAGivenTokenFSentUnchangedIsFollowed(t *testing.T) {
	recTok := jwtFor("u-admin", 1790000000, "c2lnbmF0dXJlLXJlY29yZGVk")
	ciA, ciB := jwtFor("u-admin", 1790000009, "c2lnbmF0dXJlLWNpLWFhYWFh"), jwtFor("u-admin", 1790000010, "c2lnbmF0dXJlLWNpLWJiYmJi")
	del := func(id string) string {
		return `{"operationName":"deleteRoleBinding","variables":{"id":"` + id + `"}}`
	}
	recs := []*models.Mock{
		learnRecorded("mock-0", 0, "POST", "/query", del("rb-1"), sessionCookie(recTok), `{"data":{"deleteRoleBinding":true}}`),
		learnRecorded("mock-1", 10*time.Millisecond, "POST", "/query", del("rb-2"), sessionCookie(recTok), `{"data":{"deleteRoleBinding":true}}`),
	}
	calls := func(a, b string) []*req {
		return []*req{
			live("POST", "/query", del("rb-1"), sessionCookie(a)),
			live("POST", "/query", del("rb-2"), sessionCookie(b)),
		}
	}
	// The harness gives each call's session right before the call.
	give := func(table *variables.Table, i int, tok string) {
		name := []string{"cookie-jwt_token_staging-1", "cookie-jwt_token_staging-2"}[i]
		if _, ok := table.Bind(variables.Binding{Name: name, Value: tok, Recorded: recTok, Mock: "the test's harness", Place: "given", Given: true}); !ok {
			t.Fatalf("bind %s", name)
		}
	}

	// F: both tokens are the recorded one.
	h, n, opts := learnOn(t)
	ctx, tr := inTest()
	db := &givenDb{learnDb: &learnDb{perTest: copies(recs), observe: tr.ServedMock}}
	db.table.Start(nil)
	for i, c := range calls(recTok, recTok) {
		give(&db.table, i, recTok)
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

	// The replays after F: each call with its own install's token.
	replay := func(mocks []*models.Mock, a, b string) (bool, *matchDiag) {
		ci := &varDb{perTest: append([]*models.Mock(nil), mocks...)}
		ci.table.Start(nil)
		for i, c := range calls(a, b) {
			give(&ci.table, i, []string{ciA, ciB}[i])
			if ok, _, diag := matchVar(t, h, ci, c); !ok {
				return false, diag
			}
		}
		return true, nil
	}
	if ok, diag := replay(learnedOnto(recs, db.consumed), ciA, ciB); !ok {
		t.Fatalf("CI: %+v", diag)
	}
	// The other install's token is not this call's.
	if ok, _ := replay(learnedOnto(recs, db.consumed), ciB, ciB); ok {
		t.Fatal("CI: the first call was answered with the second install's token")
	}
	// Control: the recording as it was, with nothing learned, refuses CI's.
	if ok, _ := replay(copies(recs), ciA, ciB); ok {
		t.Fatal("control: CI's tokens were answered by the recording with nothing learned")
	}
}
