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
