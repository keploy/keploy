package mock

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func TestSetTableCarriesTheFirstSuiteStart(t *testing.T) {
	t0 := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	m := &models.Mapping{
		Boots:     []models.BootSpec{{Dir: "tests/e2e", Key: "k", Mocks: []models.MockEntry{{Name: "mock-1"}}}},
		TestCases: []models.MappedTestCase{{ID: "T", Dir: "tests/e2e", Mocks: []models.MockEntry{{Name: "mock-2"}}}},
		Suites: []models.SuiteSpan{
			{Dir: "tests/e2e", Start: t0.Add(time.Second)},
			{Dir: "tests/e2e", Start: t0},
			{Dir: "other", Start: t0.Add(-time.Hour)},
		},
	}
	sets := setTable(m, "")
	if len(sets) != 1 || !sets["tests/e2e"].Start.Equal(t0) {
		t.Fatalf("sets %+v", sets)
	}
}
