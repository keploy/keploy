package proxy

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A value the open test gave binds in its scope, and a recording that uses it
// matches only a call carrying that value, answered with it: the replay's
// test gives a new one, and the application must pass it on. A given for
// another test than the one served, or under a name already bound, is
// refused.
func TestAGivenIsCheckedInTheCallsThatCarryIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own := varMockOf("own-1", "/apps", "app-1761609600123456789", true, models.LifetimePerTest)
	v := own.Spec.Variables["key"]
	own.Spec.Variables = models.MockVariables{"given:app": v}
	mm := stagedManager(t, []*models.Mock{own}, nil)
	mm.NewVariableSet()
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.OpenExactTest("test-1", []*models.Mock{own}, nil)

	given := models.VariableBinding{Name: "given:app", Value: "app-1761700000000000001", Recorded: "app-1761609600123456789", Mock: "test test-1", Place: "apps_test.go:20", Given: true}
	if err := mm.GiveExactTest("test-2", given); err == nil || !strings.Contains(err.Error(), "not the test keploy serves now") {
		t.Fatalf("a given for a test not served: %v", err)
	}
	if err := mm.GiveExactTest("test-1", given); err != nil {
		t.Fatalf("the open test's given: %v", err)
	}
	if err := mm.GiveExactTest("test-1", given); err == nil || !strings.Contains(err.Error(), "bound already") {
		t.Fatalf("a second given of one name: %v", err)
	}
	if got := postKey(t, mm, "/apps", "app-1761609600123456789"); strings.HasPrefix(got, "200 ") {
		t.Fatalf("a call carrying the recorded value, not the one the test gave, was answered %s", got)
	}
	if got := postKey(t, mm, "/apps", "app-1761700000000000001"); got != `200 {"echo":"app-1761700000000000001"}` {
		t.Fatalf("the call carrying the given value was answered %s", got)
	}
}

// Under the proven switch the proxy hands the learn step each value the open
// test gave (learn.Tracker.GiveValue) once its scope binds it: an earlier
// place of the test's calls, so a value made from it is refused naming the
// given. A given the scope refuses is no place, and with the switch off
// none is.
func TestTheProxyHandsTheLearnStepTheTestsGivens(t *testing.T) {
	given := models.VariableBinding{Name: "given:app", Value: "app-2", Recorded: "app-1", Mock: "test test-1", Place: "apps_test.go:20", Given: true}
	open := func(t *testing.T) *Proxy {
		own := varMockOf("own-1", "/apps", "app-1", true, models.LifetimePerTest)
		mm := stagedManager(t, []*models.Mock{own}, nil)
		mm.NewVariableSet()
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.OpenExactTest("test-1", []*models.Mock{own}, nil)
		p := &Proxy{mockManager: mm, learnTracker: learn.NewTracker()}
		p.learnTracker.Begin()
		return p
	}
	gave := func(p *Proxy) []learn.Earlier {
		var out []learn.Earlier
		for _, e := range p.learnTracker.Placed().Earlier {
			if e.Gave {
				out = append(out, e)
			}
		}
		return out
	}

	t.Setenv(provenswitch.Env, "1")
	p := open(t)
	if err := p.GiveExactTest("test-2", given); err == nil || len(gave(p)) != 0 {
		t.Fatalf("a refused given (%v) is a place: %+v", err, gave(p))
	}
	if err := p.GiveExactTest("test-1", given); err != nil {
		t.Fatalf("the open test's given: %v", err)
	}
	if g := gave(p); len(g) != 1 || g[0].Mock != "test test-1" || g[0].Seen[0].Leaf.Path != "given:app" || g[0].Seen[0].Leaf.Value != "app-1" || g[0].Seen[0].Live != "app-2" {
		t.Fatalf("the given's place %+v", g)
	}

	t.Setenv(provenswitch.Env, "0")
	p = open(t)
	if err := p.GiveExactTest("test-1", given); err != nil || len(gave(p)) != 0 {
		t.Fatalf("with the switch off (%v) a given is a place: %+v", err, gave(p))
	}
}

// A value a test gave (a client test case's request: Proxy.pairCase) binds
// only in the scope of variables begun for that test. The view serving
// test-2 again with no variables begun (ServeExactTest) keeps test-1's scope
// in force; a given of test-2's bound there would be test-1's once that
// scope ends, read as test-1's by every call that trails it
// (variables.Table.LookupOf), and a note there would name test-2's reason in
// test-1's misses. Both are refused, and test-2's own scope takes them.
func TestAGivenBindsOnlyInItsOwnTestsScope(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	own := varMockOf("own-1", "/apps", "app-1761609600123456789", true, models.LifetimePerTest)
	mm := stagedManager(t, []*models.Mock{own}, nil)
	mm.NewVariableSet()
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.OpenExactTest("test-1", []*models.Mock{own}, nil)
	mm.ServeExactTest("test-2", nil)

	given := models.VariableBinding{Name: "given:app", Value: "app-1761700000000000002", Recorded: "app-1761609600123456789", Mock: "test case post-apps-2", Place: "request body.name", Given: true}
	if err := mm.GiveExactTest("test-2", given); err == nil || !strings.Contains(err.Error(), "no scope of test-2's variables has begun") {
		t.Fatalf("test-2's given in test-1's scope: %v", err)
	}
	if err := mm.NoteExactTest("test-2", "given:other", "why"); err == nil || !strings.Contains(err.Error(), "no scope of test-2's variables has begun") {
		t.Fatalf("test-2's note in test-1's scope: %v", err)
	}
	if why := mm.vars.Unbound("given:other"); why != "" {
		t.Fatalf("test-1's scope holds test-2's note %q", why)
	}

	mm.OpenExactTest("test-2", nil, nil)
	if b, _, ok := mm.vars.LookupOf("test-1", "given:app"); ok {
		t.Fatalf("test-1's ended scope holds test-2's given %+v", b)
	}
	if err := mm.GiveExactTest("test-2", given); err != nil {
		t.Fatalf("test-2's given in its own scope: %v", err)
	}
	if b, ended, ok := mm.vars.LookupOf("test-2", "given:app"); !ok || ended || b.Value != given.Value {
		t.Fatalf("test-2's scope holds %+v (ended %v, ok %v)", b, ended, ok)
	}
	if err := mm.NoteExactTest("test-2", "given:other", "why"); err != nil || mm.vars.Unbound("given:other") != "why" {
		t.Fatalf("test-2's note in its own scope: %v", err)
	}
}
