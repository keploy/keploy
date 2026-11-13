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

// A token the test gave keploy that the jar carries as the value of two of
// its pairs (a session cookie and a refresh cookie set to the same token),
// the rest of the header as recorded, is the given's use at each of those
// pairs, in the order the header names them, with no bind.
func TestLearnMatchFollowsAGivenIntoEveryCookiePairThatHoldsIt(t *testing.T) {
	jar := func(tok string) map[string]string {
		return map[string]string{"Cookie": "jwt=" + tok + "; theme=dark; refresh=" + tok}
	}
	m, miss := learnGivenCall(t, `{"name":"a"}`, jar(givenJWTRec), `{"token":"`+givenJWTRec+`"}`, `{"name":"a"}`, jar(givenJWTFull), "given:session", givenJWTRec, givenJWTFull)
	if miss != nil {
		t.Fatalf("refused: %s", miss.res.Why)
	}
	v, ok := m.Spec.Learned()["given:session"]
	if !ok || len(m.Spec.Learned()) != 1 || v.Recorded != givenJWTRec {
		t.Fatalf("variables %+v, want given:session alone, recorded as the token", m.Spec.Learned())
	}
	want := []string{"header.cookie|cookie.jwt", "header.cookie|cookie.refresh"}
	if !reflect.DeepEqual(v.Use, want) || !reflect.DeepEqual(v.Answer, []string{"body.token"}) {
		t.Fatalf("given:session used at %v, answered at %v; want %v and body.token", v.Use, v.Answer, want)
	}
}

// Where two pairs held the given's recorded value, the pairs that hold this
// run's value are its use. One that kept the recorded token is compared as
// recorded (the full round shows it does not follow the given); one that
// holds a third token is a pair that changed too, and the call is refused
// naming the given, as a value made from it is.
func TestLearnMatchFollowsAGivenOnlyIntoThePairsThatHoldIt(t *testing.T) {
	const rec, full = "tok-1761609600123", "tok-1761700000456"
	cookie := func(v string) map[string]string { return map[string]string{"Cookie": v} }
	m, miss := learnGivenCall(t, `{}`, cookie("jwt="+rec+"; refresh="+rec), `{"id":7}`, `{}`, cookie("jwt="+full+"; refresh="+rec), "given:session", rec, full)
	if miss != nil {
		t.Fatalf("refused: %s", miss.res.Why)
	}
	if v := m.Spec.Learned()["given:session"]; !reflect.DeepEqual(v.Use, []string{"header.cookie|cookie.jwt"}) {
		t.Fatalf("given:session used at %v; want header.cookie|cookie.jwt alone", v.Use)
	}
	m, miss = learnGivenCall(t, `{}`, cookie("jwt="+rec+"; refresh="+rec), `{"id":7}`, `{}`, cookie("jwt="+full+"; refresh=tok-1761700000999"), "given:session", rec, full)
	if m != nil {
		t.Fatalf("served, learning %+v; want the call refused", m.Spec.Learned())
	}
	// The refusal names the pairs that differ, never their values.
	if why := learn.GivenDerivedWhy("header.cookie (pair jwt holds another value; pair refresh holds another value)", "given:session", "apps_test.go:20"); !strings.Contains(miss.res.Why, why) {
		t.Fatalf("refused: %s; want %q", miss.res.Why, why)
	}
}

// A replay holds a given used at two pairs of the Cookie header: the call
// whose pairs both carry the value the test gave is answered; one where a
// pair carries another value is refused naming the given.
func TestVariables_AGivenInTwoCookiePairsIsPassedOnOrTheCallIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := at(varMock("mock-1", "POST", "http://store/apps", `{"name":"a"}`, 201, `{"id":7}`,
		models.MockVariables{"given:session": useV("tok-1", []string{"header.cookie|cookie.jwt", "header.cookie|cookie.refresh"})}), 1)
	m.Spec.HTTPReq.Header["Cookie"] = "jwt=tok-1; theme=dark; refresh=tok-1"
	db := &varDb{perTest: []*models.Mock{m}}
	db.table.Start(nil)
	if _, ok := db.table.Give(variables.Binding{Name: "given:session", Value: "tok-9", Recorded: "tok-1", Mock: "test o::TestApps", Place: "apps_test.go:20"}); !ok {
		t.Fatal("the test's given did not bind")
	}
	call := func(cookie string) *req {
		r := liveReq("POST", "/apps", `{"name":"a"}`)
		r.header.Set("Cookie", cookie)
		return r
	}
	ok, _, diag := matchVar(t, h, db, call("jwt=tok-9; theme=dark; refresh=tok-8"))
	wantProblem(t, ok, diag, `the test gave given:session = "tok-9" (apps_test.go:20)`)
	ok, stub, diag := matchVar(t, h, db, call("jwt=tok-9; theme=dark; refresh=tok-9"))
	if !ok || stub == nil {
		t.Fatalf("the call carrying the given value in both its pairs was refused: %+v", diag)
	}
}

// A pair named twice holds no value (variables.Cookie), so it is no place
// of a given, even one whose value in this run is empty: the given is
// followed into the one pair that holds it whole, and the pair named twice
// stays as recorded beside it. Were the pair named twice taken as holding
// the empty value, the given could not be written back there and the call
// would be refused.
func TestGivenCookiesSkipsAPairNamedTwice(t *testing.T) {
	g := variables.Binding{Name: "given:session", Recorded: "tok-1", Value: ""}
	steps, ok := givenCookies("jwt=tok-1; t=1; t=2", "jwt=; t=1; t=2", g)
	if !ok || !reflect.DeepEqual(steps, []variables.Step{variables.Cookie("jwt")}) {
		t.Fatalf("givenCookies = %v, %v; want [%s], true", steps, ok, variables.Cookie("jwt"))
	}
}
