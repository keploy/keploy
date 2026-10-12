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
	if !m.pushSetTable(context.Background(), "rec", false) {
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
	if m.pushSetTable(context.Background(), "rec", false) || p.sets != nil {
		t.Fatalf("pushed %+v", p.sets)
	}
}

func TestAddCasesListsEachTestsHTTPCasesInRequestOrder(t *testing.T) {
	t0 := time.Date(2026, 10, 12, 1, 6, 5, 0, time.UTC)
	m := &models.Mapping{TestCases: []models.MappedTestCase{{
		ID: "T", Dir: "e2e", Mocks: []models.MockEntry{{Name: "mock-1"}, {Name: "mock-2"}, {Name: "mock-3"}},
		Cases:     []string{"test-2", "test-1", "test-9"},
		CaseMocks: map[string][]string{"test-1": {"mock-1"}, "test-2": {"mock-2"}},
	}}}
	sets := setTable(m, "")
	tcs := []*models.TestCase{
		{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: "http://h/a", Timestamp: t0}},
		{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "POST", URL: "http://h/b", Timestamp: t0.Add(time.Second)}},
		{Name: "test-9", Kind: models.GRPC_EXPORT},
	}
	addCases(sets, m, "", tcs)
	got := sets["e2e"].Cases["T"]
	if len(got) != 2 || got[0].Name != "test-1" || got[1].Name != "test-2" || got[0].Mocks[0] != "mock-1" || got[1].Method != "POST" {
		t.Fatalf("cases %+v", got)
	}
}

func TestAddCasesSkipsATestWhoseCasesHaveNoMappedMocks(t *testing.T) {
	t0 := time.Date(2026, 10, 12, 1, 6, 5, 0, time.UTC)
	m := &models.Mapping{TestCases: []models.MappedTestCase{
		{ID: "Old", Dir: "e2e", Mocks: []models.MockEntry{{Name: "mock-1"}}, Cases: []string{"test-1"}},
		{ID: "Bare", Dir: "e2e", Cases: []string{"test-2"}},
	}}
	sets := setTable(m, "")
	addCases(sets, m, "", []*models.TestCase{
		{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: "http://h/a", Timestamp: t0}},
		{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: "http://h/b", Timestamp: t0}},
	})
	if _, ok := sets["e2e"].Cases["Old"]; ok {
		t.Fatalf("a recording without case mocks got cases: %+v", sets["e2e"].Cases)
	}
	if got := sets["e2e"].Cases["Bare"]; len(got) != 1 || got[0].Name != "test-2" {
		t.Fatalf("a test with no mocks at all: cases %+v", got)
	}
}
