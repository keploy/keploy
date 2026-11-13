package http

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

// A session token a test signs itself, as recorded and in record's full
// round: its claims hold the time it was made.
const (
	givenJWTRec  = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSIsImlhdCI6MTc2MTYwOTYwMH0.T1K3nRj2mJx8yQ9vA4bC6dE0fG2hI4jK6lM8nO0pQ2s"
	givenJWTFull = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1MSIsImlhdCI6MTc2MTcwMDAwMH0.Zb7Xw1Vu3Ts5Rq7Po9Nm1Lk3Ji5Hg7Fe9Dc1Ba3Zy5x"
)

// learnGivenCall runs record's full round on one POST /v1/apps recorded
// with recBody and recHeader while the open test gave name (rec as
// recorded, full now), and sent liveBody and liveHeader: the mock it
// served, or the miss.
func learnGivenCall(t *testing.T, recBody string, recHeader map[string]string, respBody, liveBody string, liveHeader map[string]string, name, rec, full string) (*models.Mock, *learnMiss) {
	t.Helper()
	h, n, opts := learnOn(t)
	m := learnRecorded("mock-0", -time.Hour+300*time.Millisecond, "POST", "/v1/apps", recBody, recHeader, respBody)
	db := &givenDb{learnDb: &learnDb{perTest: []*models.Mock{m}}}
	db.table.Start(nil)
	if _, ok := db.table.Give(variables.Binding{Name: name, Value: full, Recorded: rec, Mock: "test o::TestApps", Place: "apps_test.go:20", Given: true}); !ok {
		t.Fatal("the test's given did not bind")
	}
	tr := learn.NewTracker()
	tr.Begin()
	tr.GiveValue(learn.TestGiven("test o::TestApps", name, rec, full))
	served, miss, err := h.learnMatch(learn.WithTracker(context.Background(), tr), live("POST", "/v1/apps", liveBody, liveHeader), db, n, opts)
	if err != nil {
		t.Fatal(err)
	}
	if served == nil {
		return nil, miss
	}
	return &db.consumed[0], nil
}

// A token the test gave keploy, sent as one pair of the Cookie header beside
// the jar's other pairs, is the given's use at that pair (|cookie.<name>),
// with no bind, and an answer that holds it whole is answered with this
// run's value.
func TestLearnMatchFollowsAGivenIntoACookiePair(t *testing.T) {
	jar := func(tok string) map[string]string {
		return map[string]string{"Cookie": "theme=dark; session=" + tok + "; lang=en"}
	}
	m, miss := learnGivenCall(t, `{"name":"a"}`, jar(givenJWTRec), `{"token":"`+givenJWTRec+`"}`, `{"name":"a"}`, jar(givenJWTFull), "given:session", givenJWTRec, givenJWTFull)
	if miss != nil {
		t.Fatalf("refused: %s", miss.res.Why)
	}
	v, ok := m.Spec.Learned()["given:session"]
	if !ok || len(m.Spec.Learned()) != 1 || v.Recorded != givenJWTRec {
		t.Fatalf("variables %+v, want given:session alone, recorded as the token", m.Spec.Learned())
	}
	if !reflect.DeepEqual(v.Use, []string{"header.cookie|cookie.session"}) || !reflect.DeepEqual(v.Answer, []string{"body.token"}) {
		t.Fatalf("given:session used at %v, answered at %v; want header.cookie|cookie.session and body.token", v.Use, v.Answer)
	}
}

// A given is used at no pair of the Cookie header where it is no whole
// pair's value there in both runs with the rest of the header as recorded:
// inside a pair's value, beside another pair that changed, not the value
// the test gave, a pair with no name, an empty recorded given, or a text
// like a jar that is no Cookie header. A call made from the given (why) is
// refused naming it; one whose drift is no given's is learned as it is with
// no given at all, never as the given's use.
func TestLearnMatchFollowsNoGivenNoCookiePairHoldsWhole(t *testing.T) {
	const rec, full = "tok-1761609600123", "tok-1761700000456"
	cookie := func(v string) map[string]string { return map[string]string{"Cookie": v} }
	for _, c := range []struct {
		name               string
		recBody, liveBody  string
		recHeader, liveHdr map[string]string
		gRec, gFull        string
		why                string
	}{
		{name: "inside a pair's value", recBody: `{}`, liveBody: `{}`,
			recHeader: cookie("theme=dark; session=v1." + rec), liveHdr: cookie("theme=dark; session=v1." + full),
			gRec: rec, gFull: full, why: learn.GivenDerivedWhy("header.cookie (pair session holds another value)", "given:session", "apps_test.go:20")},
		{name: "another pair changed too", recBody: `{}`, liveBody: `{}`,
			recHeader: cookie("theme=dark; session=" + rec), liveHdr: cookie("theme=light; session=" + full),
			gRec: rec, gFull: full, why: learn.GivenDerivedWhy("header.cookie (pair theme holds another value; pair session holds another value)", "given:session", "apps_test.go:20")},
		{name: "not the value the test gave", recBody: `{}`, liveBody: `{}`,
			recHeader: cookie("theme=dark; session=" + rec), liveHdr: cookie("theme=dark; session=tok-1761700000999"),
			gRec: rec, gFull: full},
		{name: "a pair with no name", recBody: `{}`, liveBody: `{}`,
			recHeader: cookie("=" + rec + "; theme=dark"), liveHdr: cookie("=" + full + "; theme=dark"),
			gRec: rec, gFull: full, why: learn.GivenDerivedWhy("header.cookie (pair \"\" holds another value)", "given:session", "apps_test.go:20")},
		{name: "an empty recorded given", recBody: `{}`, liveBody: `{}`,
			recHeader: cookie("session=; theme=dark"), liveHdr: cookie("session=" + full + "; theme=dark"),
			gRec: "", gFull: full},
		{name: "a jar's text in a body field", recBody: `{"name":"session=` + rec + `; theme=dark"}`, liveBody: `{"name":"session=` + full + `; theme=dark"}`,
			gRec: rec, gFull: full, why: learn.GivenDerivedWhy("body.name", "given:session", "apps_test.go:20")},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, miss := learnGivenCall(t, c.recBody, c.recHeader, `{"id":7}`, c.liveBody, c.liveHdr, "given:session", c.gRec, c.gFull)
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

// A replay holds a given used at a pair of the Cookie header: the call that
// carries the value the test gave there is answered; one that carries
// another value is refused naming the given.
func TestVariables_AGivenInACookiePairIsPassedOnOrTheCallIsRefused(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := at(varMock("mock-1", "POST", "http://store/apps", `{"name":"a"}`, 201, `{"id":7}`,
		models.MockVariables{"given:session": useV("tok-1", []string{"header.cookie|cookie.session"})}), 1)
	m.Spec.HTTPReq.Header["Cookie"] = "theme=dark; session=tok-1"
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
	ok, _, diag := matchVar(t, h, db, call("theme=dark; session=tok-8"))
	wantProblem(t, ok, diag, `the test gave given:session = "tok-9" (apps_test.go:20)`)
	ok, stub, diag := matchVar(t, h, db, call("theme=dark; session=tok-9"))
	if !ok || stub == nil {
		t.Fatalf("the call carrying the given value in its cookie was refused: %+v", diag)
	}
}
