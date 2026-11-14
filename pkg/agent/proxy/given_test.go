package proxy

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent"
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

// A value the test's worker gave outside its tests is bound as the test's
// view opens (OpenExactTest's given): the proxy hands the learn step it too,
// as an earlier place of the test's calls, under the proven switch only. A
// template key's binding opened the same way is no value the test gave, and
// no such place.
func TestTheProxyHandsTheLearnStepTheGivensATestOpensWith(t *testing.T) {
	given := models.VariableBinding{Name: "given:uniq", Value: "u-2", Recorded: "u-1", Mock: "test test-1", Place: "s.spec.ts:24", Given: true}
	key := models.VariableBinding{Name: "order_id", Value: "o-2", Recorded: "o-1", Mock: "test case test-1", Place: "request", Given: true}
	gave := func(p *Proxy) []learn.Earlier {
		var out []learn.Earlier
		for _, e := range p.learnTracker.Placed().Earlier {
			if e.Gave {
				out = append(out, e)
			}
		}
		return out
	}
	open := func(t *testing.T, bound ...models.VariableBinding) *Proxy {
		own := varMockOf("own-1", "/apps", "u-1", true, models.LifetimePerTest)
		own.Spec.Variables = models.MockVariables{"given:uniq": own.Spec.Variables["key"]}
		mm := stagedManager(t, []*models.Mock{own}, nil)
		mm.NewVariableSet()
		mm.ServeExactPool(models.PoolKeyNone, nil)
		p := &Proxy{mockManager: mm, learnTracker: learn.NewTracker()}
		p.learnTracker.Begin()
		if err := p.OpenExactTest("test-1", []*models.Mock{own}, bound); err != nil {
			t.Fatalf("open: %v", err)
		}
		return p
	}

	t.Setenv(provenswitch.Env, "1")
	p := open(t, given, key)
	if g := gave(p); len(g) != 1 || g[0].Mock != "test test-1" || g[0].Seen[0].Leaf.Path != "given:uniq" || g[0].Seen[0].Leaf.Value != "u-1" || g[0].Seen[0].Live != "u-2" {
		t.Fatalf("the places the open gave: %+v", g)
	}
	if got := postKey(t, p.mockManager, "/apps", "u-2"); !strings.HasPrefix(got, "200 ") {
		t.Fatalf("the call carrying the worker's given value was answered %s", got)
	}

	t.Setenv(provenswitch.Env, "0")
	p = open(t, given)
	if g := gave(p); len(g) != 0 {
		t.Fatalf("with the switch off a given is a place: %+v", g)
	}
}

// A value a test worker gave outside its tests is kept for the calls it
// makes in the gap on its own manager, the run's or a worker's: a second of
// one name is refused, an end ends it, and a proxy with no manager, or a
// worker it does not serve, refuses both.
func TestTheProxyKeepsAWorkersGivensForItsCallsOutsideTests(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	b := models.VariableBinding{Name: "given:suite", Value: "s-2", Recorded: "s-1", Mock: "test worker 0", Place: "a.spec.ts:9", Given: true}
	run, worker := stagedManager(t, nil, nil), stagedManager(t, nil, nil)
	p := &Proxy{mockManager: run, learnTracker: learn.NewTracker()}
	p.instances.mgrs = map[uint32]*MockManager{7: worker}
	for _, c := range []struct {
		name string
		mm   *MockManager
		g    agent.ExactBetweenGiver
	}{{"the run's", run, p}, {"worker 7's", worker, instanceExact{p: p, inst: 7}}} {
		if err := c.g.GiveBetweenTests(b); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got, ok := c.mm.vars.Lookup("given:suite"); !ok || got.Value != "s-2" {
			t.Fatalf("%s manager reads %+v, %v", c.name, got, ok)
		}
		if err := c.g.GiveBetweenTests(b); err == nil || !strings.Contains(err.Error(), "variable given:suite is given between tests already") {
			t.Fatalf("%s: a second given of one name: %v", c.name, err)
		}
		if err := c.g.EndBetweenTests(); err != nil {
			t.Fatalf("%s: end: %v", c.name, err)
		}
		if got, ok := c.mm.vars.Lookup("given:suite"); ok {
			t.Fatalf("%s manager reads %+v after the end", c.name, got)
		}
	}
	for _, c := range []struct {
		name string
		g    agent.ExactBetweenGiver
	}{{"no manager", &Proxy{}}, {"a worker not served", instanceExact{p: p, inst: 8}}} {
		if err := c.g.GiveBetweenTests(b); err == nil {
			t.Fatalf("%s kept the given", c.name)
		}
		if err := c.g.EndBetweenTests(); err == nil {
			t.Fatalf("%s took the end", c.name)
		}
	}
}
