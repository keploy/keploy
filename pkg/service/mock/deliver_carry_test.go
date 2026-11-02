package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// httpMockAt is an HTTP mock of a call to method url at at.
func httpMockAt(method, url string, at time.Time) *models.Mock {
	mk := mockAt("", at)
	mk.Spec.HTTPReq.Method, mk.Spec.HTTPReq.URL = models.Method(method), url
	return mk
}

// walkMocks is a mock store that hands WalkTestSetMocks its mocks, or fails
// with err.
type walkMocks struct {
	stubMockDB
	mocks []*models.Mock
	err   error
}

func (w walkMocks) WalkTestSetMocks(_ context.Context, _ string, visit func(*models.Mock, bool, bool)) error {
	for _, mk := range w.mocks {
		visit(mk, true, false)
	}
	return w.err
}

const (
	paidTest  = "pay::TestPaidByCallback"
	payCall   = "POST /webhooks/pay"
	chargeURL = "http://pay.example:18110/v1/charges"
)

// fakeCarrier is a DeliveryCarrier that keeps what it is handed and answers
// kept and lines.
type fakeCarrier struct {
	prior map[string][]PriorDelivery
	tests []CarryTest
	calls int
	kept  map[string][]models.Delivery
	lines []CarryLine
}

func (f *fakeCarrier) Carry(prior map[string][]PriorDelivery, tests []CarryTest) (map[string][]models.Delivery, []CarryLine) {
	f.calls++
	f.prior, f.tests = prior, tests
	return f.kept, f.lines
}

// withCarrier registers c as the DeliveryCarrier for t only.
func withCarrier(t *testing.T, c DeliveryCarrier) {
	t.Helper()
	was := deliveryCarrier
	RegisterDeliveryCarrier(c)
	t.Cleanup(func() { RegisterDeliveryCarrier(was) })
}

// Recorded again, a set hands the registered carrier each accepted deliver:
// entry of a test it ran, with the calls the entry's case and after: were in
// the previous recording and the test's new cases and calls; it keeps the
// entries the carrier keeps, named as the new recording names them, and
// says the carrier's lines: kept at INFO, dropped at WARN. With no carrier
// every entry is dropped, said with how to add it back.
func TestAReRecordHandsADeliverEntryToTheCarrier(t *testing.T) {
	provenOn(t)
	const set = "set"
	ctx := context.Background()
	callback := func(name string, at time.Duration) *models.TestCase {
		return httpCase(name, "POST", "http://localhost:8080/webhooks/pay?attempt=1", 200, "ok", runnerT0.Add(at))
	}
	// reRecord records the set, accepts the delivery as an agent's edit
	// would, records it again, and answers its mapping then.
	reRecord := func(t *testing.T, logger *zap.Logger) *models.Mapping {
		t.Helper()
		dir := t.TempDir()
		w := mark(paidTest, 0, 10*time.Millisecond)
		w.Outcome, w.Dir = models.ScopeOutcomePassed, "pay"
		record := func(logger *zap.Logger, db *memTestDB, incoming []*models.TestCase, mocks ...*models.Mock) {
			t.Helper()
			instr := newRunnerInstr(t, w)
			instr.incoming, instr.mocks = incoming, mocks
			cfg := instrConfig(instr.composeInstr, utils.Native, "go test -p 1 -json ./...")
			cfg.Path = dir
			withRequests(cfg)
			svc := New(logger, instr, mockdb.New(zap.NewNop(), dir, ""), mapdb.New(zap.NewNop(), dir, ""), nil, nil, cfg)
			svc.(TestDBSetter).SetTestDB(db)
			require.NoError(t, svc.Record(context.Background()))
		}
		mdb := mapdb.New(zap.NewNop(), dir, "")
		record(zap.NewNop(), &memTestDB{}, []*models.TestCase{callback("post-webhooks-pay-1", 4*time.Millisecond)},
			httpMockAt("POST", chargeURL, runnerT0.Add(2*time.Millisecond)),
			httpMockAt("GET", chargeURL+"/ch_1", runnerT0.Add(6*time.Millisecond)))
		mp, err := mdb.GetMapping(ctx, set)
		require.NoError(t, err)
		require.Len(t, mp.TestCases, 1)
		require.Equal(t, []string{"mock-0", "mock-1"}, mp.TestCases[0].MockNames())
		accepted := []models.Delivery{{Case: "post-webhooks-pay-1", After: "mock-0"}}
		require.NoError(t, mdb.UpsertCases(ctx, set, map[string]models.MappedTestCase{paidTest: {At: runnerT0, Deliver: accepted}}, nil, nil))
		require.NoError(t, mdb.UpsertCases(ctx, set, map[string]models.MappedTestCase{paidTest: {Cases: []string{"post-webhooks-pay-1"}}}, nil, nil))
		mp, err = mdb.GetMapping(ctx, set)
		require.NoError(t, err)
		require.Equal(t, accepted, mp.TestCases[0].Deliver, "a write of the test's cases alone keeps its entries; only a record's run of the test replaces them")

		// Recorded again: a lookup comes first now, so the charge is
		// mock-1, and the callback is case post-webhooks-pay-2.
		record(logger, &memTestDB{existing: []*models.TestCase{callback("post-webhooks-pay-1", 4*time.Millisecond)}},
			[]*models.TestCase{callback("post-webhooks-pay-2", 5*time.Millisecond)},
			httpMockAt("GET", "http://pay.example:18110/v1/customers/c_1", runnerT0.Add(1*time.Millisecond)),
			httpMockAt("POST", chargeURL, runnerT0.Add(3*time.Millisecond)),
			httpMockAt("GET", chargeURL+"/ch_1", runnerT0.Add(7*time.Millisecond)))
		mp, err = mdb.GetMapping(ctx, set)
		require.NoError(t, err)
		require.Len(t, mp.TestCases, 1)
		require.Equal(t, []string{"post-webhooks-pay-2"}, mp.TestCases[0].Cases)
		return mp
	}

	t.Run("carrier", func(t *testing.T) {
		c := &fakeCarrier{
			kept:  map[string][]models.Delivery{paidTest: {{Case: "post-webhooks-pay-2", After: "mock-1"}}},
			lines: []CarryLine{{Kept: true, Msg: "kept line"}, {Msg: "dropped line"}},
		}
		withCarrier(t, c)
		core, logs := observer.New(zap.InfoLevel)
		mp := reRecord(t, zap.New(core))
		require.Equal(t, map[string][]PriorDelivery{paidTest: {{Delivery: models.Delivery{Case: "post-webhooks-pay-1", After: "mock-0"}, CaseCall: payCall, AfterCall: "POST pay.example:18110/v1/charges"}}}, c.prior,
			"the carrier is handed the entry with the calls its names stood for in the previous recording")
		require.Equal(t, []CarryTest{{Name: paidTest,
			Cases: []NamedCall{{Name: "post-webhooks-pay-2", Call: payCall}},
			Mocks: []NamedCall{{Name: "mock-0", Call: "GET pay.example:18110/v1/customers/c_1"}, {Name: "mock-1", Call: "POST pay.example:18110/v1/charges"}, {Name: "mock-2", Call: "GET pay.example:18110/v1/charges/ch_1"}},
		}}, c.tests, "and the test's new cases and calls, each with its call")
		require.Equal(t, []models.Delivery{{Case: "post-webhooks-pay-2", After: "mock-1"}}, mp.TestCases[0].Deliver, "the mapping keeps what the carrier kept")
		kept := logs.FilterMessage("kept line").All()
		require.Len(t, kept, 1)
		require.Equal(t, zap.InfoLevel, kept[0].Level)
		dropped := logs.FilterMessage("dropped line").All()
		require.Len(t, dropped, 1)
		require.Equal(t, zap.WarnLevel, dropped[0].Level)
	})

	t.Run("no carrier", func(t *testing.T) {
		withCarrier(t, nil)
		core, logs := observer.New(zap.InfoLevel)
		mp := reRecord(t, zap.New(core))
		require.Nil(t, mp.TestCases[0].Deliver, "an entry no carrier found again is dropped, never kept under the old recording's names")
		said := logs.FilterMessage(`test "pay::TestPaidByCallback": the deliver: entry {case: post-webhooks-pay-1, after: mock-0} of the previous recording is dropped: this keploy cannot find an entry's case and after: call again in a new recording; to deliver its callback again, add the entry back under the test's deliver: in mappings.yaml with the names its case and its after: call have in this recording`).All()
		require.Len(t, said, 1)
		require.Equal(t, zap.WarnLevel, said[0].Level)
	})
}

// The carrier is handed only the tests this recording ran that have entries,
// each once; a test it did not run, or that has no entry, is left as it is,
// and with no such test the carrier is not asked. With no carrier, each
// entry of a test that ran is said dropped, and only those.
func TestCarryDeliveriesHandsOnlyTheTestsThatRan(t *testing.T) {
	entry := PriorDelivery{Delivery: models.Delivery{Case: "post-webhooks-pay-1", After: "mock-0"}, CaseCall: payCall, AfterCall: "POST pay.example:18110/v1/charges"}
	cases := []capturedMock{{name: "post-webhooks-pay-2", call: payCall}, {name: "get-orders-1", call: "GET /orders"}}
	mocks := []capturedMock{{name: "mock-1", call: "POST pay.example:18110/v1/charges"}, {name: "mock-4"}}
	prior := map[string][]PriorDelivery{paidTest: {entry}, "pay::TestNotRun": {entry}, "pay::TestEmpty": {entry}}
	windows := []models.ScopeWindow{{Name: paidTest}, {Name: "pay::TestOther"}, {Name: paidTest}, {Name: "pay::TestEmpty"}}
	byTest := map[string][]models.MockEntry{paidTest: {{Name: "mock-1"}, {Name: "mock-4"}}}
	newByCase := func() map[string]models.MappedTestCase {
		return map[string]models.MappedTestCase{paidTest: {Cases: []string{"post-webhooks-pay-2"}}, "pay::TestOther": {Cases: []string{"get-orders-1"}}}
	}

	c := &fakeCarrier{kept: map[string][]models.Delivery{paidTest: {{Case: "post-webhooks-pay-2", After: "mock-1"}}}}
	withCarrier(t, c)
	byCase := newByCase()
	(&mockService{logger: zap.NewNop()}).carryDeliveries("set", prior, windows, byCase, byTest, cases, mocks)
	require.Equal(t, 1, c.calls)
	require.Equal(t, []CarryTest{
		{Name: paidTest, Cases: []NamedCall{{Name: "post-webhooks-pay-2", Call: payCall}}, Mocks: []NamedCall{{Name: "mock-1", Call: "POST pay.example:18110/v1/charges"}, {Name: "mock-4"}}},
		{Name: "pay::TestEmpty", Cases: []NamedCall{}, Mocks: []NamedCall{}},
	}, c.tests, "the tests that ran with entries, each once, in the order they ran")
	require.Equal(t, prior, c.prior)
	require.Equal(t, []models.Delivery{{Case: "post-webhooks-pay-2", After: "mock-1"}}, byCase[paidTest].Deliver)
	require.Equal(t, []string{"post-webhooks-pay-2"}, byCase[paidTest].Cases, "the test keeps its new cases")
	require.Nil(t, byCase["pay::TestOther"].Deliver)
	require.NotContains(t, byCase, "pay::TestNotRun", "a test the recording did not run is left as it is")
	require.NotContains(t, byCase, "pay::TestEmpty", "a test the carrier kept nothing of gets no mapping entry from the carry")

	c.calls = 0
	(&mockService{logger: zap.NewNop()}).carryDeliveries("set", map[string][]PriorDelivery{"pay::TestNotRun": {entry}}, windows, newByCase(), byTest, cases, mocks)
	(&mockService{logger: zap.NewNop()}).carryDeliveries("set", nil, windows, newByCase(), byTest, cases, mocks)
	require.Zero(t, c.calls, "no test that ran has an entry: the carrier is not asked")

	withCarrier(t, nil)
	core, logs := observer.New(zap.InfoLevel)
	byCase = newByCase()
	(&mockService{logger: zap.New(core)}).carryDeliveries("set", prior, windows, byCase, byTest, cases, mocks)
	require.Equal(t, newByCase(), byCase, "with no carrier nothing is kept")
	var said []string
	for _, e := range logs.All() {
		require.Equal(t, zap.WarnLevel, e.Level)
		said = append(said, e.Message[:strings.Index(e.Message, "\": ")+1])
	}
	require.Equal(t, []string{`test "pay::TestPaidByCallback"`, `test "pay::TestEmpty"`}, said, "one line an entry of a test that ran, and none for a test that did not")
}

// Before a re-record deletes the mappings, keploy reads each deliver: entry
// with the calls its case and after: stood for ("" for a name that stood for
// none); an entry whose calls cannot be read carries why, and a mapping that
// cannot be read is said.
func TestPriorDeliveriesReadTheCallsTheEntriesStoodFor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mdb := mapdb.New(zap.NewNop(), dir, "")
	t.Setenv(provenswitch.Env, "1")
	require.NoError(t, mdb.UpsertBatch(ctx, "set", map[string][]models.MockEntry{
		paidTest:      {{Name: "mock-0", Kind: string(models.HTTP)}, {Name: "mock-1", Kind: string(models.HTTP)}, {Name: "mock-2", Kind: string(models.HTTP)}},
		"pay::TestNo": {{Name: "mock-3", Kind: string(models.HTTP)}},
	}))
	deliver := []models.Delivery{{Case: "post-webhooks-pay-1", After: "mock-0"}, {Case: "test-2", After: "mock-1"}, {Case: "test-3", After: "mock-2"}}
	require.NoError(t, mdb.UpsertCases(ctx, "set", map[string]models.MappedTestCase{
		paidTest:      {At: runnerT0, Cases: []string{"post-webhooks-pay-1", "test-2", "test-3"}, Deliver: deliver},
		"pay::TestNo": {At: runnerT0, Cases: []string{"test-4"}},
	}, nil, nil))
	grpc := &models.TestCase{Name: "test-2", Kind: models.GRPC_EXPORT}
	cases := casesDB{cases: []*models.TestCase{nil, httpCase("post-webhooks-pay-1", "POST", "http://localhost:8080/webhooks/pay?x=1", 200, "ok", runnerT0), grpc, httpCase("test-4", "GET", "/orders", 200, "", runnerT0)}}
	pg := &models.Mock{Name: "mock-1", Kind: models.Postgres}
	mocks := walkMocks{mocks: []*models.Mock{httpMockAt("POST", chargeURL, runnerT0), pg, httpMockAt("GET", "/x", runnerT0)}}
	mocks.mocks[0].Name, mocks.mocks[2].Name = "mock-0", "mock-9"
	svc := func(testDB TestDB, mockDB MockDB) *mockService {
		return &mockService{logger: zap.NewNop(), mappingDB: mdb, testDB: testDB, mockDB: mockDB}
	}
	paid := PriorDelivery{Delivery: deliver[0], CaseCall: payCall, AfterCall: "POST pay.example:18110/v1/charges"}

	require.Equal(t, map[string][]PriorDelivery{paidTest: {
		paid,
		{Delivery: deliver[1]},
		{Delivery: deliver[2]},
	}}, svc(cases, mocks).priorDeliveries(ctx, "set"), "a case that is not an HTTP request, or not in the cases, stood for no call")

	cases.cases = append(cases.cases, httpCase("test-2", "PUT", "/a", 200, "", runnerT0), httpCase("test-3", "PUT", "/b", 200, "", runnerT0))
	cases.cases[2] = nil
	require.Equal(t, map[string][]PriorDelivery{paidTest: {
		paid,
		{Delivery: deliver[1], CaseCall: "PUT /a"},
		{Delivery: deliver[2], CaseCall: "PUT /b"},
	}}, svc(cases, mocks).priorDeliveries(ctx, "set"), "a case read, its after: not an HTTP call, or not in the mocks")

	for _, tt := range []struct {
		name   string
		testDB TestDB
		mockDB MockDB
		lost   string
	}{
		{"no cases kept", nil, mocks, "this keploy keeps no recorded cases (mock.recordRequests is false in keploy.yml)"},
		{"cases unreadable", casesDB{err: errors.New("disk")}, mocks, "the previous recording's cases could not be read: disk"},
		{"mocks not walked", cases, stubMockDB{}, "this keploy's mock store cannot read the previous recording's mocks one by one"},
		{"mocks unreadable", cases, walkMocks{err: errors.New("cut file")}, "the previous recording's mocks could not be read: cut file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := svc(tt.testDB, tt.mockDB).priorDeliveries(ctx, "set")
			require.Len(t, got[paidTest], 3)
			for _, d := range got[paidTest] {
				require.Equal(t, tt.lost, d.Lost)
			}
		})
	}

	require.Nil(t, svc(cases, mocks).priorDeliveries(ctx, "none"), "a set with no mappings has nothing to carry")
	require.Nil(t, (&mockService{logger: zap.NewNop(), mockDB: mocks}).priorDeliveries(ctx, "set"), "no mapping store, nothing to carry")
	require.NoError(t, mdb.UpsertCases(ctx, "plain", map[string]models.MappedTestCase{"pay::TestNo": {At: runnerT0}}, nil, nil))
	require.Nil(t, svc(cases, mocks).priorDeliveries(ctx, "plain"), "a set with no deliver: entry has nothing to carry")

	// An unreadable mapping is said, and nothing is carried.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "set", "mappings.yaml"), []byte("tests: [\n"), 0o644))
	core, logs := observer.New(zap.InfoLevel)
	bad := svc(cases, mocks)
	bad.logger = zap.New(core)
	require.Nil(t, bad.priorDeliveries(ctx, "set"))
	said := logs.FilterMessage("keploy could not read the set's mappings.yaml before recording it again, so it carries none of its deliver: entries into this recording").All()
	require.Len(t, said, 1)
	require.Equal(t, zap.WarnLevel, said[0].Level)

	// Without the proven switch deliver: is never read, and nothing is said.
	t.Setenv(provenswitch.Env, "")
	core, logs = observer.New(zap.InfoLevel)
	bad.logger = zap.New(core)
	require.Nil(t, bad.priorDeliveries(ctx, "set"))
	require.Zero(t, logs.Len())
}

// The calls a carried entry is found by: a case by method and path, a mock
// by method, host and path; neither by query.
func TestDeliverCallsAreMethodAndPath(t *testing.T) {
	require.Equal(t, "POST /webhooks/pay", caseCall(httpCase("c", "POST", "http://localhost:8080/webhooks/pay?attempt=2", 200, "", runnerT0)))
	require.Equal(t, "", caseCall(&models.TestCase{Kind: models.GRPC_EXPORT}))
	require.Equal(t, "", caseCall(nil))
	require.Equal(t, "POST pay.example:18110/v1/charges", mockCall(httpMockAt("POST", chargeURL+"?expand=1", runnerT0)))
	require.Equal(t, "", mockCall(&models.Mock{Kind: models.HTTP}), "an HTTP mock with no request")
	require.Equal(t, "", mockCall(&models.Mock{Kind: models.Postgres, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "POST"}}}))
	require.Equal(t, "", mockCall(nil))
	require.Equal(t, "%zz/a", callPath("%zz/a?b=1", true), "not a URL: the raw path, without its query")
	require.Equal(t, "/v1/charges", callPath(chargeURL, false))

	// The record keeps each case's call as it is stored.
	c := &caseCapture{}
	c.add(httpCase("post-webhooks-pay-2", "POST", "http://localhost:8080/webhooks/pay", 200, "", runnerT0))
	require.Equal(t, payCall, c.list()[0].call)
}
