package mock

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func TestSplitSuitesKeepsSuiteMarksOutOfTheTests(t *testing.T) {
	at := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	tests, suites := splitSuites([]models.ScopeWindow{
		{Name: "/repo/e2e/orders", Start: at, End: at.Add(5 * time.Second), Dir: "/repo/e2e/orders", Suite: true},
		{Name: "orders.TestA", Start: at.Add(time.Second), End: at.Add(2 * time.Second), Dir: "/repo/e2e/orders"},
	})
	if len(tests) != 1 || tests[0].Name != "orders.TestA" {
		t.Fatalf("tests = %+v", tests)
	}
	if len(suites) != 1 || suites[0].Dir != "/repo/e2e/orders" || suites[0].End.Sub(suites[0].Start) != 5*time.Second {
		t.Fatalf("suites = %+v", suites)
	}
}

func TestStartsByTestCountsRestartsInsideEachTest(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 10, 4, 10, 0, s, 0, time.UTC) }
	windows := []models.ScopeWindow{
		{Name: "/repo/e2e/orders", Start: at(0), End: at(50), Suite: true},
		{Name: "orders.TestA", Start: at(10), End: at(20)},
		{Name: "orders.TestReload", Start: at(30), End: at(40)},
		{Name: "orders.TestReload/step", Start: at(31), End: at(39)},
		{Name: "app:8080", Start: at(5), End: at(5), App: true, Port: 8080},
		{Name: "app:8080", Start: at(33), End: at(33), App: true, Port: 8080},
		{Name: "app:8080", Start: at(36), End: at(36), App: true, Port: 8080},
	}
	tests, _ := splitSuites(windows)
	if len(tests) != 3 {
		t.Fatalf("app starts and suites must not be tests: %+v", tests)
	}
	got := startsByTest(tests, appStarts(windows))
	if got["orders.TestReload"] != 2 || got["orders.TestA"] != 0 || got["orders.TestReload/step"] != 0 {
		t.Fatalf("starts = %v", got)
	}
}
