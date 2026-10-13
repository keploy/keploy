package mock

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
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

// pulledDB is a recording pulled from the registry: its sets (Suites) and its
// per-test mappings.
type pulledDB struct {
	mapping *models.Mapping
	tests   map[string][]models.MockEntry
}

func (d pulledDB) UpsertBatch(context.Context, string, map[string][]models.MockEntry) error {
	return nil
}

func (d pulledDB) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	return d.tests, true, nil
}

func (d pulledDB) GetMapping(context.Context, string) (*models.Mapping, error) {
	return d.mapping, nil
}

type bothPusher struct {
	Instrumentation
	sets  map[string]models.SetTable
	table map[string][]string
}

func (p *bothPusher) PushSetTable(_ context.Context, _ string, sets map[string]models.SetTable) error {
	p.sets = sets
	return nil
}

func (p *bothPusher) PushScopeTable(_ context.Context, table map[string][]string, _ bool, _ map[string]string) error {
	p.table = table
	return nil
}

// A pulled browser suite carries its sets and its per-test mappings: the set
// table gives each set its start, and the per-test table still scopes each
// test's mocks. Pushing only the set table left every test on the whole pool.
func TestPushScopeTablePushesPerTestMappingsBesideTheSetTable(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 11, 0, 0, 0, time.UTC)
	p := &bothPusher{}
	m := &mockService{logger: zap.NewNop(), config: &config.Config{}, instrumentation: p, mappingDB: pulledDB{
		mapping: &models.Mapping{
			TestCases: []models.MappedTestCase{{ID: "sso > non-admin", Dir: "tests/projects/sso/assigner", Mocks: []models.MockEntry{{Name: "mock-7"}}}},
			Suites:    []models.SuiteSpan{{Dir: "tests/projects/sso/assigner", Start: t0}},
		},
		tests: map[string][]models.MockEntry{"sso > non-admin": {{Name: "mock-7"}}},
	}}
	m.pushScopeTable(context.Background(), "enterprise-ui")
	if !p.sets["tests/projects/sso/assigner"].Start.Equal(t0) {
		t.Fatalf("set table %+v", p.sets)
	}
	if got := p.table["sso > non-admin"]; len(got) != 1 || got[0] != "mock-7" {
		t.Fatalf("per-test table %+v, want the test's own mock", p.table)
	}
}
