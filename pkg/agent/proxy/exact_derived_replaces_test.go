package proxy

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// derivedFrom is an authored mock of name derived from the recording source
// names ("<test>/<mock>"), recorded at at, making path's call.
func derivedFrom(name, source string, at time.Duration, path string) *models.Mock {
	m := httpCall(name, at, path)
	m.Spec.Authored = &models.Authored{How: models.AuthoredDerived, Reason: "declined card", Source: source}
	return m
}

// Under exact serving a test whose own mocks hold a derived mock is served it
// in place of its source: the view never shows the source, whether it is the
// reached worker scope's, the pool's under the background rules, or a
// recording shared with the test, while the next test's view, and the pool
// view between them, show it as before. A generated mock replaces nothing.
func TestADerivedMockReplacesItsSourceInItsOwnTestAloneUnderExactServing(t *testing.T) {
	for _, rules := range []string{"", models.RecordedWithS12Routing} {
		t.Run("rules="+rules, func(t *testing.T) {
			mm, ms := s12Set(t)
			dWorker := derivedFrom("d-worker", "startup:/w-boot", 5*time.Second, "/w-boot")
			dPool := derivedFrom("d-pool", "pkg::TestA/pool", 6*time.Second, "/pool")
			dOther := derivedFrom("d-other", "T2/other", 7*time.Second, "/other")
			generated := httpCall("generated", 8*time.Second, "/own")
			generated.Spec.Authored = &models.Authored{How: models.AuthoredGenerated, Reason: "timeout", Source: "T2/own"}
			mm.ServeExactBackground(rules, []string{"pool"}, []string{"w-boot", "w-later"})
			mm.ServeExactWorker([]string{"w-boot"})

			mm.ServeExactTestConcurrent("T1", []*models.Mock{dWorker, dPool, dOther, generated}, []*models.Mock{ms["other"], ms["own"]})
			v := mm.exactView()
			for _, src := range []string{"w-boot", "pool", "other"} {
				if v.shows(ms[src]) || v.holds(src) {
					t.Fatalf("T1, whose derived mock replaces %s, is still served it", src)
				}
			}
			if !v.holds("own") {
				t.Fatal("a generated mock replaced the recording its Source names: T1 lost the shared own")
			}
			for _, d := range []string{"d-worker", "d-pool", "d-other", "generated"} {
				if !v.holds(d) {
					t.Fatalf("T1 is not served its own %s", d)
				}
			}
			if got := sessionNames(mm); containsName(got, "w-boot") {
				t.Fatalf("a reusable read in T1 shows %s; the replaced w-boot must not be among them", got)
			}

			mm.ServeExactTest("T2", []*models.Mock{ms["other"]})
			v = mm.exactView()
			if !v.shows(ms["w-boot"]) {
				t.Fatal("T2, which holds no derived mock, is not served the worker scope's w-boot")
			}
			if rules != "" && !v.shows(ms["pool"]) {
				t.Fatal("T2 is not served the pool's recording under the background rules")
			}
			if !v.holds("other") {
				t.Fatal("T2 is not served its own other, which T1's derived mock replaced only in T1")
			}
			if got := sessionNames(mm); !containsName(got, "w-boot") {
				t.Fatalf("a reusable read in T2 shows %s; want w-boot among them", got)
			}

			mm.ServeExactPool(models.PoolKeyNone, []string{"pool"})
			if v = mm.exactView(); !v.shows(ms["pool"]) || !v.shows(ms["w-boot"]) {
				t.Fatal("the pool view between tests does not show the recordings a test's derived mocks replaced in it")
			}
		})
	}
}

// SharedGeneration must change when a test's derived mocks replace a shared
// recording so a parser caching SharedMocks by SharedGen never serves a
// replaced source from the previous test's cached shared view.
func TestADerivedMockMovesTheSharedViewGeneration(t *testing.T) {
	mm, ms := s12Set(t)
	mm.ServeExactBackground(models.RecordedWithS12Routing, []string{"pool"}, []string{"w-boot"})
	mm.ServeExactWorker([]string{"w-boot"})

	mm.ServeExactTest("T1", []*models.Mock{ms["other"]})
	g1, ok := mm.SharedGeneration(models.HTTP)
	if !ok {
		t.Fatal("T1 has no SharedGeneration")
	}
	_, sess1, ok := mm.SharedMocks()
	if !ok || !mockSliceHas(sess1, "w-boot") {
		t.Fatalf("T1 SharedMocks() must include w-boot; ok=%v", ok)
	}

	dWorker := derivedFrom("d-worker", "startup:/w-boot", 5*time.Second, "/w-boot")
	mm.ServeExactTest("T2", []*models.Mock{dWorker})
	g2, ok := mm.SharedGeneration(models.HTTP)
	if !ok {
		t.Fatal("T2 has no SharedGeneration")
	}
	if g2 == g1 {
		t.Fatalf("T2 replaces w-boot but kept T1's SharedGeneration %+v", g2)
	}
	_, sess2, ok := mm.SharedMocks()
	if !ok || mockSliceHas(sess2, "w-boot") {
		t.Fatalf("T2 SharedMocks() must omit replaced w-boot; ok=%v", ok)
	}

	// Another test replacing the same source reuses T2's shared view; a test
	// replacing nothing moves the generation again and sees w-boot.
	dWorker2 := derivedFrom("d-worker-2", "startup:/w-boot", 6*time.Second, "/w-boot")
	mm.ServeExactTest("T2b", []*models.Mock{dWorker2})
	g2b, _ := mm.SharedGeneration(models.HTTP)
	if g2b != g2 {
		t.Fatalf("T2b replaces the same source as T2: got %+v, want %+v", g2b, g2)
	}

	mm.ServeExactTest("T3", []*models.Mock{ms["other"]})
	g3, _ := mm.SharedGeneration(models.HTTP)
	if g3 == g2 {
		t.Fatalf("T3 replaces nothing but kept T2's SharedGeneration %+v", g3)
	}
	_, sess3, _ := mm.SharedMocks()
	if !mockSliceHas(sess3, "w-boot") {
		t.Fatal("T3 SharedMocks() must include w-boot again")
	}
}

// Under the proven switch a start-up recording that keploy.yaml's sessionCalls
// name exactly is served to every test (declaredInPool, #5198). A test whose
// own mocks hold a derived mock replacing that start-up recording is never
// served the source (sharesRest drops sourcesReplaced before declaredInPool),
// and its shared view's generation (SharedGeneration's View) differs from
// that of a test that keeps it, while the pool view between tests and the
// next test, which holds no derived mock, still get the start-up recording.
func TestADerivedMockReplacesADeclaredStartUpSourceInItsOwnTestAlone(t *testing.T) {
	declareSessionCalls(t, declaredRule)
	mm, ms := startupSet(t)
	dRates := statedCall("d-rates", 6*time.Second, "/echo/rates")
	dRates.Spec.Authored = &models.Authored{How: models.AuthoredDerived, Reason: "rates down", Source: "startup:/rates"}
	ix := urlIndex()
	pool := []string{"rates", "cfg"}

	mm.ServeExactPool("k", pool)
	if got := keepNamed(t, mm, "rates"); got != "rates" {
		t.Fatalf("the declared start-up call was not answered before the first test: %s", got)
	}

	// T1 replaces startup:/rates with d-rates: rates must not appear in T1's
	// view, reusable reads, keyed walks, or SharedMocks.
	mm.ServeExactTest("T1", []*models.Mock{dRates})
	v := mm.exactView()
	if v.shows(ms["rates"]) {
		t.Fatal("T1, whose derived mock replaces declared start-up rates, is still served it")
	}
	if !v.holds("d-rates") {
		t.Fatal("T1 is not served its own d-rates")
	}
	if got := keepNamed(t, mm, "rates"); got != "a miss" {
		t.Fatalf("T1 reusable read answered replaced declared start-up rates: %s", got)
	}
	if got := sessionNames(mm); containsName(got, "rates") {
		t.Fatalf("T1 sessionNames %q includes replaced declared start-up rates", got)
	}
	if got := keyedWalk(t, mm, ix, "/echo/rates"); slices.Contains(got, "rates") || !slices.Contains(got, "d-rates") {
		t.Fatalf("T1 keyed walk of /echo/rates = %q; want d-rates without rates", got)
	}
	_, sess1, ok := mm.SharedMocks()
	if !ok || mockSliceHas(sess1, "rates") {
		t.Fatalf("T1 SharedMocks() must omit replaced declared start-up rates; ok=%v", ok)
	}
	g1, ok := mm.SharedGeneration(models.HTTP)
	if !ok {
		t.Fatal("T1 has no SharedGeneration")
	}

	// Between tests: the pool view keeps the declared start-up recording.
	mm.ServeExactPool("k", pool)
	if got := keepNamed(t, mm, "rates"); got != "rates" {
		t.Fatalf("the pool view between tests lost declared start-up rates: %s", got)
	}

	// T2 holds no derived mock: it keeps the declared start-up recording, and
	// its shared view's generation differs from T1's. (Only the View half
	// tells: the answered keepNamed calls since g1 move the Store half.)
	mm.ServeExactTest("T2", []*models.Mock{ms["own"]})
	v = mm.exactView()
	if !v.shows(ms["rates"]) {
		t.Fatal("T2, which holds no derived mock, is not served declared start-up rates")
	}
	if got := keepNamed(t, mm, "rates"); got != "rates" {
		t.Fatalf("T2 reusable read did not answer declared start-up rates: %s", got)
	}
	if got := strings.Join(keyedWalk(t, mm, ix, "/echo/rates"), ","); got != "rates" {
		t.Fatalf("T2 keyed walk of /echo/rates = %q; want rates", got)
	}
	_, sess2, ok := mm.SharedMocks()
	if !ok || !mockSliceHas(sess2, "rates") {
		t.Fatalf("T2 SharedMocks() must include declared start-up rates; ok=%v", ok)
	}
	g2, ok := mm.SharedGeneration(models.HTTP)
	if !ok || g2.View == g1.View {
		t.Fatalf("T2 SharedGeneration = %+v (ok=%v); want a View other than T1's %+v", g2, ok, g1)
	}
}

func mockSliceHas(mocks []*models.Mock, name string) bool {
	for _, m := range mocks {
		if m != nil && m.Name == name {
			return true
		}
	}
	return false
}

func containsName(list, name string) bool {
	for _, n := range splitNames(list) {
		if n == name {
			return true
		}
	}
	return false
}

func splitNames(list string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(list); i++ {
		if i == len(list) || list[i] == ',' {
			if i > start {
				out = append(out, list[start:i])
			}
			start = i + 1
		}
	}
	return out
}
