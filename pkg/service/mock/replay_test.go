package mock

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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

type setTableDB struct {
	MappingDB
	mapping *models.Mapping
}

func (d setTableDB) GetMapping(context.Context, string) (*models.Mapping, error) {
	return d.mapping, nil
}

type setTablePusher struct {
	Instrumentation
	sets map[string]models.SetTable
}

func (p *setTablePusher) PushSetTable(_ context.Context, _ string, sets map[string]models.SetTable) error {
	p.sets = sets
	return nil
}

func TestPushSetTableWithSuitesButNoBoots(t *testing.T) {
	t0 := time.Date(2026, 10, 12, 1, 6, 5, 0, time.UTC)
	p := &setTablePusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "T", Dir: "e2e/b", Mocks: []models.MockEntry{{Name: "mock-1"}}}},
		Suites:    []models.SuiteSpan{{Dir: "e2e/b", Start: t0}},
	}}}
	if !m.pushSetTable(context.Background(), "rec") {
		t.Fatal("the set table was not pushed")
	}
	if !p.sets["e2e/b"].Start.Equal(t0) {
		t.Fatalf("sets %+v", p.sets)
	}
}

func TestPushSetTableSkipsAMappingWithNoBootsOrSuites(t *testing.T) {
	p := &setTablePusher{}
	m := &mockService{logger: zap.NewNop(), instrumentation: p, mappingDB: setTableDB{mapping: &models.Mapping{
		TestCases: []models.MappedTestCase{{ID: "T", Dir: "e2e/b", Mocks: []models.MockEntry{{Name: "mock-1"}}}},
	}}}
	if m.pushSetTable(context.Background(), "rec") || p.sets != nil {
		t.Fatalf("pushed %+v", p.sets)
	}
}
