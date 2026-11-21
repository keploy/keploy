package http

import (
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// A token the test gave keploy, sent as the token of "Authorization:
// Bearer <token>", is the given's use at that token (|bearer), with no
// bind, and an answer that holds it whole is answered with this run's
// value.
func TestLearnMatchFollowsAGivenIntoABearerToken(t *testing.T) {
	auth := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
	m, miss := learnGivenCall(t, `{"name":"a"}`, auth(givenJWTRec), `{"token":"`+givenJWTRec+`"}`, `{"name":"a"}`, auth(givenJWTFull), "given:session", givenJWTRec, givenJWTFull)
	if miss != nil {
		t.Fatalf("refused: %s", miss.res.Why)
	}
	v, ok := m.Spec.Learned()["given:session"]
	if !ok || len(m.Spec.Learned()) != 1 || v.Recorded != givenJWTRec {
		t.Fatalf("variables %+v, want given:session alone, recorded as the token", m.Spec.Learned())
	}
	if !reflect.DeepEqual(v.Use, []string{"header.authorization|bearer"}) || !reflect.DeepEqual(v.Answer, []string{"body.token"}) {
		t.Fatalf("given:session used at %v, answered at %v; want header.authorization|bearer and body.token", v.Use, v.Answer)
	}
}

// A given is used at no Bearer token where it is no whole token there in
// both runs with the rest of the header as recorded: inside the token, a
// scheme that changed, spaces that changed, another scheme, a token with
// text after it, not the value the test gave, or a Bearer text that is no
// Authorization header. A call made from the given (why) is refused naming
// it; one whose drift is no given's is learned as it is with no given at
// all, never as the given's use.
func TestLearnMatchFollowsNoGivenNoBearerTokenHoldsWhole(t *testing.T) {
	const rec, full = "tok-1761609600123", "tok-1761700000456"
	auth := func(v string) map[string]string { return map[string]string{"Authorization": v} }
	derived := func(path string) string { return learn.GivenDerivedWhy(path, "given:session", "apps_test.go:20") }
	for _, c := range []struct {
		name               string
		recBody, liveBody  string
		recHeader, liveHdr map[string]string
		gRec               string
		why                string
	}{
		{name: "inside the token", recBody: `{}`, liveBody: `{}`,
			recHeader: auth("Bearer x" + rec), liveHdr: auth("Bearer x" + full), why: derived("header.authorization")},
		{name: "the scheme changed", recBody: `{}`, liveBody: `{}`,
			recHeader: auth("Bearer " + rec), liveHdr: auth("Token " + full), why: derived("header.authorization")},
		{name: "the spaces changed", recBody: `{}`, liveBody: `{}`,
			recHeader: auth("Bearer  " + rec), liveHdr: auth("Bearer " + full), why: derived("header.authorization")},
		{name: "another scheme", recBody: `{}`, liveBody: `{}`,
			recHeader: auth("Basic " + rec), liveHdr: auth("Basic " + full), why: derived("header.authorization")},
		{name: "text after the token", recBody: `{}`, liveBody: `{}`,
			recHeader: auth("Bearer " + rec + ", realm=a"), liveHdr: auth("Bearer " + full + ", realm=a"), why: derived("header.authorization")},
		{name: "not the value the test gave", recBody: `{}`, liveBody: `{}`,
			recHeader: auth("Bearer " + rec), liveHdr: auth("Bearer tok-1761700000999")},
		{name: "an empty recorded given in a header recorded empty", recBody: `{}`, liveBody: `{}`,
			recHeader: auth(""), liveHdr: auth("Bearer " + full), gRec: "-"},
		{name: "a Bearer text in a body field", recBody: `{"name":"Bearer ` + rec + `"}`, liveBody: `{"name":"Bearer ` + full + `"}`,
			why: derived("body.name")},
	} {
		t.Run(c.name, func(t *testing.T) {
			gRec := rec
			if c.gRec == "-" {
				gRec = ""
			}
			m, miss := learnGivenCall(t, c.recBody, c.recHeader, `{"id":7}`, c.liveBody, c.liveHdr, "given:session", gRec, full)
			if m != nil {
				if _, used := m.Spec.Learned()["given:session"]; used || c.why != "" {
					t.Fatalf("served, learning %+v; want no use of given:session", m.Spec.Learned())
				}
				return
			}
			if c.why == "" || !strings.Contains(miss.res.Why, c.why) {
				t.Fatalf("refused: %s; want %q", miss.res.Why, c.why)
			}
		})
	}
}

// A replay holds a given used at a Bearer token: the call that carries the
// value the test gave there is answered; one that carries another value is
// refused naming the given, and one that carries it under another scheme
// is refused too.
func TestVariables_AGivenInABearerTokenIsPassedOnOrTheCallIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := at(varMock("mock-1", "POST", "http://store/apps", `{"name":"a"}`, 201, `{"id":7}`,
		models.MockVariables{"given:session": useV("tok-1", []string{"header.authorization|bearer"})}), 1)
	m.Spec.HTTPReq.Header["Authorization"] = "Bearer tok-1"
	db := &varDb{perTest: []*models.Mock{m}}
	db.table.Start(nil)
	if _, ok := db.table.Give(variables.Binding{Name: "given:session", Value: "tok-9", Recorded: "tok-1", Mock: "test o::TestApps", Place: "apps_test.go:20"}); !ok {
		t.Fatal("the test's given did not bind")
	}
	call := func(v string) *req {
		r := liveReq("POST", "/apps", `{"name":"a"}`)
		r.header.Set("Authorization", v)
		return r
	}
	ok, _, diag := matchVar(t, h, db, call("Bearer tok-8"))
	wantProblem(t, ok, diag, `the test gave given:session = "tok-9" (apps_test.go:20)`)
	if ok, stub, _ := matchVar(t, h, db, call("Basic tok-9")); ok || stub != nil {
		t.Fatal("the call carrying the given value under another scheme was answered")
	}
	ok, stub, diag := matchVar(t, h, db, call("Bearer tok-9"))
	if !ok || stub == nil {
		t.Fatalf("the call carrying the given value as its Bearer token was refused: %+v", diag)
	}
}
