package proxy

import (
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
