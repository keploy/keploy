package proxy

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// declaredRule is the session call the rows below declare, as keploy.yaml's
// sessionCalls would: GET /echo/rates on 127.0.0.1.
var declaredRule = models.SessionCallRule{Host: "127.0.0.1", Method: "GET", PathPrefix: "/echo/rates"}

// statedCall is an HTTP call its recorder stated per-test, its lifetime
// derived under the session calls installed now.
func statedCall(name string, at time.Duration, path string) *models.Mock {
	m := exactMock(name, at, models.LifetimePerTest)
	m.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: path, Header: map[string]string{"Host": "127.0.0.1:9000"}}
	m.Spec.Metadata = map[string]string{"type": string(models.HTTPClient), models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	m.DeriveLifetime()
	return m
}

// declareSessionCalls installs rules for the row, and none after it.
func declareSessionCalls(t *testing.T, rules ...models.SessionCallRule) {
	t.Helper()
	t.Cleanup(func() { models.SetSessionCalls(nil) })
	models.SetSessionCalls(rules)
}

// keepNamed answers a call with the recording named as HTTP answers one of a
// session lifetime: through a door that keeps it (UpdateUnFilteredMock). It
// says "a miss" when no read shows the recording, and "refused" when the
// consume was.
func keepNamed(t *testing.T, mm *MockManager, name string) string {
	t.Helper()
	mocks, _ := mm.GetSessionScopedMocks()
	for _, mk := range mocks {
		if mk.Name != name {
			continue
		}
		cp := *mk
		if mm.UpdateUnFilteredMock(mk, &cp) {
			return name
		}
		return "refused"
	}
	return "a miss"
}

// startupSet stages, under the proven switch: rates and cfg, recorded at
// start-up (the pool), of which keploy.yaml's sessionCalls cover rates
// alone; rates-T0, rates-T3 and rates-T4, other tests' recordings of rates's
// call; and own, T1's own.
func startupSet(t *testing.T) (*MockManager, map[string]*models.Mock) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	ms := map[string]*models.Mock{
		"rates":    statedCall("rates", 0, "/echo/rates"),
		"cfg":      statedCall("cfg", time.Second, "/echo/cfg"),
		"rates-T0": statedCall("rates-T0", 2*time.Second, "/echo/rates"),
		"rates-T3": statedCall("rates-T3", 3*time.Second, "/echo/rates"),
		"rates-T4": statedCall("rates-T4", 4*time.Second, "/echo/rates"),
		"own":      statedCall("own", 5*time.Second, "/echo/quote"),
	}
	var all []*models.Mock
	for _, n := range []string{"rates", "cfg", "rates-T0", "rates-T3", "rates-T4", "own"} {
		all = append(all, ms[n])
	}
	return stagedManager(t, nil, all), ms
}

// Under the proven switch a call keploy.yaml's sessionCalls declare is served
// to every test of the run from its recording made outside every test (at
// start-up), as many times as a test makes it: TestMain loads the rates, and
// a test that loads them again is answered with that recording. A start-up
// recording no rule covers stays the calls' between tests, and another test's
// recording of the declared call stays that test's.
func TestADeclaredSessionCallRecordedAtStartUpIsServedToEveryTest(t *testing.T) {
	declareSessionCalls(t, declaredRule)
	mm, ms := startupSet(t)
	pool := []string{"rates", "cfg"}
	mm.ServeExactPool("k", pool)
	if got := keepNamed(t, mm, "rates"); got != "rates" {
		t.Fatalf("the start-up call was not answered before the first test: %s", got)
	}
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if got := sessionNames(mm); got != "rates" {
		t.Fatalf("in a test the reusable read shows %s; want rates (the declared start-up call, not cfg or another test's)", got)
	}
	for i := 0; i < 2; i++ {
		if got := keepNamed(t, mm, "rates"); got != "rates" {
			t.Fatalf("call %d to the declared call in T1 was not answered by its start-up recording: %s", i+1, got)
		}
	}
	if got := keepNamed(t, mm, "cfg"); got != "a miss" {
		t.Fatalf("a start-up call no rule declares was served in a test: %s", got)
	}
	// The next test, after the gap: served again.
	mm.ServeExactPool("k", pool)
	mm.ServeExactTest("T2", nil)
	if got := keepNamed(t, mm, "rates"); got != "rates" {
		t.Fatalf("the declared start-up call was not served to the next test: %s", got)
	}
	// A keyed read (a parser that walks the recordings filed under the
	// request) finds it too, and none of the other tests' recordings filed
	// under the same request, whichever way the view walks the key.
	ix := &integrations.MockIndex{Keys: func(mk *models.Mock) []string {
		if mk.Spec.HTTPReq == nil {
			return nil
		}
		return []string{mk.Spec.HTTPReq.URL}
	}}
	if got := strings.Join(keyedWalk(t, mm, ix, "/echo/rates"), ","); got != "rates" {
		t.Fatalf("a keyed read of the declared call in a test shows %q; want rates", got)
	}
}

// Without a declaration the start-up recording is the calls' between tests
// alone, as before: a test that makes the call again misses it.
func TestAStartUpCallNoRuleDeclaresIsNotServedInATest(t *testing.T) {
	models.SetSessionCalls(nil)
	mm, ms := startupSet(t)
	mm.ServeExactPool("k", []string{"rates", "cfg"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	if got := keepNamed(t, mm, "rates"); got != "a miss" {
		t.Fatalf("a start-up call no rule declares was served in a test: %s", got)
	}
}

// Under the switch a rule serves a test a start-up recording only of a call
// it names exactly, by its method and whole path: one that covers the call
// by a path prefix only, or with no method, serves it between tests alone,
// as with no rule, though it makes the recording session (DeriveLifetime),
// so that no recording is served in a test to a call no one declared.
func TestAStartUpCallARuleCoversOnlyByPrefixIsNotServedInATest(t *testing.T) {
	for _, rule := range []models.SessionCallRule{
		{Host: "127.0.0.1", Method: "GET", PathPrefix: "/echo"},
		{Host: "127.0.0.1", PathPrefix: "/echo/rates"},
	} {
		t.Run(rule.Method+rule.PathPrefix, func(t *testing.T) {
			declareSessionCalls(t, rule)
			mm, ms := startupSet(t)
			if ms["rates"].TestModeInfo.Lifetime != models.LifetimeSession {
				t.Fatalf("the rule does not make the start-up recording session: %v", ms["rates"].TestModeInfo.Lifetime)
			}
			mm.ServeExactPool("k", []string{"rates", "cfg"})
			mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
			if got := keepNamed(t, mm, "rates"); got != "a miss" {
				t.Fatalf("a start-up call the rule covers only by prefix was served in a test: %s", got)
			}
		})
	}
}

// A test's view shows the declared calls of the pool slice served before it
// opened: after a pool view of another key, whose slice holds none, the next
// test is served none, and its shared part is told apart from the last
// test's (SharedGeneration). With no call declared, two tests with a pool
// view between them share their shared part's number, as before.
func TestATestShowsTheDeclaredCallsOfThePoolSliceBeforeIt(t *testing.T) {
	declareSessionCalls(t, declaredRule)
	mm, ms := startupSet(t)
	mm.ServeExactPool("k1", []string{"rates"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	g1, _ := mm.SharedGeneration("")
	mm.ServeExactPool("k2", nil)
	mm.ServeExactTest("T2", nil)
	g2, _ := mm.SharedGeneration("")
	if got := keepNamed(t, mm, "rates"); got != "a miss" {
		t.Fatalf("a test after another key's pool slice was served k1's declared call: %s", got)
	}
	if g1.View == g2.View {
		t.Fatalf("two tests that show different declared calls share their shared part's number %d", g1.View)
	}

	models.SetSessionCalls(nil)
	mm2, ms2 := startupSet(t)
	mm2.ServeExactPool("k", []string{"rates"})
	mm2.ServeExactTest("T1", []*models.Mock{ms2["own"]})
	h1, _ := mm2.SharedGeneration("")
	mm2.ServeExactPool("k", []string{"rates"})
	mm2.ServeExactTest("T2", nil)
	h2, _ := mm2.SharedGeneration("")
	if h1.View != h2.View {
		t.Fatalf("with no call declared, two tests with a pool view between them got shared numbers %d and %d; want one", h1.View, h2.View)
	}
}

// With a call declared, two tests with a pool view of the same names between
// them share their shared part, as two tests with no call declared do: one
// number (SharedGeneration), which a parser files its index of the shared
// reads by (integrations.SharedReads), so the second test builds none. Each
// view files what it shows itself, so the store's count of those
// (shownBuilds) cannot tell; the number is what is shared. A pool view of
// other names is told apart (TestATestShowsTheDeclaredCallsOfThePoolSliceBeforeIt).
func TestTestsWithAPoolViewOfTheSameNamesBetweenThemShareTheirSharedPart(t *testing.T) {
	declareSessionCalls(t, declaredRule)
	mm, ms := startupSet(t)
	ix := &integrations.MockIndex{Keys: func(mk *models.Mock) []string {
		if mk.Spec.HTTPReq == nil {
			return nil
		}
		return []string{mk.Spec.HTTPReq.URL}
	}}
	mm.ServeExactPool("k", []string{"rates", "cfg"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	g1, _ := mm.SharedGeneration("")
	if got := strings.Join(keyedWalk(t, mm, ix, "/echo/rates"), ","); got != "rates" {
		t.Fatalf("T1's keyed read of the declared call shows %q; want rates", got)
	}
	mm.ServeExactPool("k", []string{"rates", "cfg"})
	mm.ServeExactTest("T2", nil)
	g2, _ := mm.SharedGeneration("")
	if g1.View != g2.View {
		t.Fatalf("two tests with a pool view of the same names between them got shared numbers %d and %d; want one", g1.View, g2.View)
	}
	if got := strings.Join(keyedWalk(t, mm, ix, "/echo/rates"), ","); got != "rates" {
		t.Fatalf("T2's keyed read of the declared call shows %q; want rates", got)
	}
	// Three tests, each after a pool view of the same names: one number.
	mm.ServeExactPool("k", []string{"cfg", "rates"})
	mm.ServeExactTest("T3", nil)
	if g3, _ := mm.SharedGeneration(""); g3.View != g1.View {
		t.Fatalf("a third test after a pool view of the same names got shared number %d; want %d", g3.View, g1.View)
	}
}

// A test opened before any pool slice was served is served no declared
// session call: the calls it shows are those of the pool slice served
// before it, and there is none, so neither the start-up recording nor
// another test's recording of the declared call answers in it.
func TestATestOpenedBeforeAnyPoolSliceIsServedNoDeclaredCall(t *testing.T) {
	declareSessionCalls(t, declaredRule)
	mm, ms := startupSet(t)
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	for _, name := range []string{"rates", "rates-T0"} {
		if got := keepNamed(t, mm, name); got != "a miss" {
			t.Fatalf("a test opened before any pool slice was served %s: %s", name, got)
		}
	}
}

// Under the background rules a pool recording answers at most one call in the
// staging; one keploy.yaml declares served to every test is not spent, and
// answers every test's call, though TestMain made the call between tests
// first.
func TestUnderTheBackgroundRulesADeclaredPoolCallIsNotSpent(t *testing.T) {
	declareSessionCalls(t, declaredRule)
	mm, ms := startupSet(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"rates", "cfg"}, nil)
	mm.ServeExactPool(models.PoolKeyNone, []string{"rates", "cfg"})
	if got := keepNamed(t, mm, "rates"); got != "rates" {
		t.Fatalf("TestMain's call to the declared pool call between tests under the background rules: %s", got)
	}
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	for i := 0; i < 2; i++ {
		if got := keepNamed(t, mm, "rates"); got != "rates" {
			t.Fatalf("call %d to the declared pool call in T1 under the background rules: %s", i+1, got)
		}
	}
	mm.ServeExactPool(models.PoolKeyNone, []string{"rates", "cfg"})
	mm.ServeExactTest("T2", nil)
	if got := keepNamed(t, mm, "rates"); got != "rates" {
		t.Fatalf("the declared pool call was not served to the next test under the background rules: %s", got)
	}
	// One no rule declares is spent once, as before.
	if got := keepNamed(t, mm, "cfg"); got != "cfg" {
		t.Fatalf("an undeclared pool call under the background rules was not served once: %s", got)
	}
	if got := keepNamed(t, mm, "cfg"); got != "a miss" {
		t.Fatalf("an undeclared pool call under the background rules answered twice: %s", got)
	}
}

// With no session call declared, a test's view carries no pool slice
// (exactView.declared): tests share their shared part across pool views of
// other names too, as they did before sessionCalls existed.
func TestWithNoSessionCallDeclaredTestsShareTheirSharedPartAcrossPoolViews(t *testing.T) {
	declareSessionCalls(t)
	mm, ms := startupSet(t)
	mm.ServeExactPool("k", []string{"rates"})
	mm.ServeExactTest("T1", []*models.Mock{ms["own"]})
	g1, _ := mm.SharedGeneration("")
	mm.ServeExactPool("k", []string{"rates", "cfg"})
	mm.ServeExactTest("T2", nil)
	if g2, _ := mm.SharedGeneration(""); g2.View != g1.View {
		t.Fatalf("two tests with a pool view of other names between them, and no session call declared, got shared numbers %d and %d; want one", g1.View, g2.View)
	}
}
