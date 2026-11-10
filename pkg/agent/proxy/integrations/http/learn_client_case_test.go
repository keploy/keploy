package http

import (
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
