package http

import (
	"encoding/base64"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A value a client test's request sent (learn.Place.Client), which one of the
// test's own calls carries whole, is a variable of the test: bound at its
// test case's request place, and used by the call with no bind of its own,
// as a template key's is. The same value from the request keploy test sent
// stays refused (nothing binds there), as does one a call that is not the
// test's own carries.
func TestAValueAClientTestsRequestSentIsBoundAtItsTestCase(t *testing.T) {
	tr := learn.NewTracker()
	tr.Begin()
	test := tr.Epoch()
	m := &models.Mock{Name: "mock-3"}
	rec := learn.Leaf{Path: "body.filter.name", Value: "app-1794207752000000001", Type: learn.String}
	live := learn.Leaf{Path: "body.filter.name", Value: "app-1794211111000000002", Type: learn.String}
	jsonReq := learn.Shape{Fixed: map[string]string{"body": "json"}}
	from := learn.Place{Mock: "test case post-apps-1", Side: learn.Request, Path: "body.name", Var: "body.name", Live: live.Value, Whole: true, Given: true, Client: true}
	learned := func(f learn.Place) []learn.Learned {
		return []learn.Learned{{Kind: learn.KindVariable, Recorded: rec, Live: live, From: f}}
	}

	p, why := learnPlan(tr, test, m, jsonReq, true, learned(from), nil, nil)
	if why != "" || len(p.vars) != 1 || len(p.binds) != 1 {
		t.Fatalf("why %q, variables %v, binds %v; want one variable bound at the test case", why, p.vars, p.binds)
	}
	var name string
	for n, v := range p.vars {
		name = n
		if v.Recorded != rec.Value || v.Bind != "" || len(v.Use) != 1 || v.Use[0] != "body.filter.name" {
			t.Errorf("the call's variable %s = %+v; want recorded %s, used at body.filter.name, no bind", n, v, rec.Value)
		}
	}
	if b := p.binds["test case post-apps-1"][name]; b.Recorded != rec.Value || b.Bind != "body.name" || len(b.Use) != 0 {
		t.Errorf("binds %v; want %s bound at test case post-apps-1's body.name", p.binds, name)
	}
	if g := p.values[name]; g.Value != live.Value || g.Recorded != rec.Value || g.Mock != "test case post-apps-1" || g.Place != "body.name" {
		t.Errorf("the binding served = %+v; want the request's live value at its body.name", g)
	}

	keployTest := from
	keployTest.Client = false
	if p, why := learnPlan(tr, test, m, jsonReq, true, learned(keployTest), nil, nil); !strings.Contains(why, "cannot follow into a call yet") || len(p.vars) != 0 {
		t.Errorf("the request keploy test sent: why %q, variables %v; want it refused, nothing learned", why, p.vars)
	}
	if p, why := learnPlan(tr, test, m, jsonReq, false, learned(from), nil, nil); !strings.Contains(why, "only a test's own calls carry its variables") || len(p.vars) != 0 {
		t.Errorf("a call not the test's own: why %q, variables %v; want it refused, nothing learned", why, p.vars)
	}
	inside := from
	inside.Whole = false
	if p, why := learnPlan(tr, test, m, jsonReq, true, learned(inside), nil, nil); !strings.Contains(why, "inside a longer value") || len(p.vars) != 0 {
		t.Errorf("a part of the request's value: why %q, variables %v; want it refused, nothing learned", why, p.vars)
	}
	other := from
	other.Live = "app-1794211111000000003"
	if p, why := learnPlan(tr, test, m, jsonReq, true, learned(other), nil, nil); !strings.Contains(why, "does not hold the value the test sent") || len(p.vars) != 0 {
		t.Errorf("a call that does not carry the request's live value: why %q, variables %v; want it refused, nothing learned", why, p.vars)
	}
}

// A value a client test's request sent inside its token, in a JWT claim
// (learn.Place.Claim), binds at the claim's word of its test case's request
// place, with that word as the value the call must carry: each claim of one
// token is a variable of its own, and a claim's clock evidence is read from
// its word. A claim the live token does not open stays refused as a value
// inside a longer one.
func TestAValueAClientTestsTokenSentInAClaimIsBoundAtTheClaim(t *testing.T) {
	enc := base64.RawURLEncoding
	token := func(payload string) string {
		return "Bearer " + enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(payload)) + ".c2ln"
	}
	liveTok := token(`{"cid":"e2e-company-2","cluster_id":"6af47b1e0c22d4e8a91b03f6","login_at":"2026-11-11T10:05Z"}`)
	jsonReq := learn.Shape{Fixed: map[string]string{"body": "json"}}
	from := func(claim, live string) learn.Place {
		return learn.Place{Mock: "test case post-cluster-status-3", Side: learn.Request, Path: "header.Authorization", Var: "header.Authorization", Live: live,
			Given: true, Client: true, Claim: claim}
	}
	variable := func(path, rec, live string, f learn.Place) learn.Learned {
		return learn.Learned{Kind: learn.KindVariable, Recorded: learn.Leaf{Path: path, Value: rec, Type: learn.String}, Live: learn.Leaf{Path: path, Value: live, Type: learn.String}, From: f}
	}

	tr := learn.NewTracker()
	tr.Begin()
	test := tr.Epoch()
	p, why := learnPlan(tr, test, &models.Mock{Name: "mock-3"}, jsonReq, true, []learn.Learned{
		variable("body.filter._id", "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6", from("header.Authorization|jwt.cluster_id#0", liveTok)),
		variable("body.filter.company", "e2e-company-1", "e2e-company-2", from("header.Authorization|jwt.cid", liveTok)),
	}, nil, nil)
	if why != "" || len(p.vars) != 2 || len(p.binds["test case post-cluster-status-3"]) != 2 {
		t.Fatalf("why %q, variables %v, binds %v; want two variables, each bound at its claim of the test case's token", why, p.vars, p.binds)
	}
	for name, b := range p.binds["test case post-cluster-status-3"] {
		v, g := p.vars[name], p.values[name]
		want := map[string]string{"header.Authorization|jwt.cluster_id#0": "6af47b1e0c22d4e8a91b03f6", "header.Authorization|jwt.cid": "e2e-company-2"}[b.Bind]
		if want == "" || v.Recorded != b.Recorded || g.Place != b.Bind || g.Value != want || g.Mock != "test case post-cluster-status-3" {
			t.Errorf("%s: bind %+v, binding %+v; want it bound at a claim of the token, its live value the claim's word", name, b, g)
		}
	}

	// login_at, the minute the application minted the token at: a read of its
	// clock that moved by the lead, carried by the call as the claim holds it.
	tr = learn.NewTracker()
	tr.Begin()
	p, why = learnPlan(tr, tr.Epoch(), &models.Mock{Name: "mock-3"}, jsonReq, true, []learn.Learned{
		variable("body.since", "2026-11-11T10:04Z", "2026-11-11T10:05Z", from("header.Authorization|jwt.login_at", liveTok)),
	}, nil, nil)
	if b := p.binds["test case post-cluster-status-3"]; why != "" || len(b) != 1 {
		t.Fatalf("why %q, binds %v; want login_at bound at its claim", why, p.binds)
	}
	for _, b := range p.binds["test case post-cluster-status-3"] {
		if len(b.Clock) == 0 {
			t.Errorf("bind %+v; want the clock evidence of the claim's moments", b)
		}
	}

	tr = learn.NewTracker()
	tr.Begin()
	if p, why := learnPlan(tr, tr.Epoch(), &models.Mock{Name: "mock-3"}, jsonReq, true, []learn.Learned{
		variable("body.filter._id", "6af47add8a9351dfdb7c457a", "6af47b1e0c22d4e8a91b03f6", from("header.Authorization|jwt.cluster_id#0", "Bearer opaque-token")),
	}, nil, nil); !strings.Contains(why, "inside a longer value or at a place keploy cannot bind it") || len(p.vars) != 0 {
		t.Errorf("a claim the live token does not open: why %q, variables %v; want it refused, nothing learned", why, p.vars)
	}
}
