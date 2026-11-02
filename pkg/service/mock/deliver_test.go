package mock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// casesDB is a test-case store that answers GetTestCases with cases, or
// fails with err.
type casesDB struct {
	cases []*models.TestCase
	err   error
}

func (db casesDB) InsertTestCase(context.Context, *models.TestCase, string, bool) error { return nil }
func (db casesDB) GetTestCases(context.Context, string) ([]*models.TestCase, error) {
	return db.cases, db.err
}
func (db casesDB) DeleteTests(context.Context, string, []string) error { return nil }

func payCallback(name string) *models.TestCase {
	return &models.TestCase{Name: name, Kind: models.HTTP,
		HTTPReq:  models.HTTPReq{Method: "POST", URL: "http://localhost:8080/webhooks/pay", Header: map[string]string{"X-Pay-Signature": "ab12"}, Body: `{"order":"o-1","status":"paid"}`},
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: "ok"},
	}
}

// The replay's table carries each test's deliver: entries with their cases'
// recorded requests and answers, in the entries' order; a set with none
// carries none. An entry keploy cannot send as recorded refuses the replay
// with what is missing, never a table without it.
func TestRecordedDeliveriesCarryTheCasesRecordedRequests(t *testing.T) {
	ctx := context.Background()
	pay := models.MappedTestCase{ID: "pay::TestPaid", Cases: []string{"test-1", "test-2", "test-3"}, Deliver: []models.Delivery{
		{Case: "test-3", After: "mock-4"},
		{Case: "test-2", After: "mock-3"},
	}}
	refund := models.MappedTestCase{ID: "pay::TestRefund", Cases: []string{"test-5"}, Deliver: []models.Delivery{{Case: "test-5", After: "mock-9"}}}
	plain := models.MappedTestCase{ID: "pay::TestList", Cases: []string{"test-4"}}
	svc := func(db TestDB) *mockService {
		return &mockService{logger: zap.NewNop(), config: &config.Config{Path: "/repo/keploy"}, testDB: db}
	}

	got, err := svc(nil).recordedDeliveries(ctx, "set", nil)
	require.NoError(t, err)
	require.Nil(t, got, "no mapping, nothing to deliver")
	got, err = svc(nil).recordedDeliveries(ctx, "set", &models.Mapping{TestCases: []models.MappedTestCase{plain}})
	require.NoError(t, err, "a set with no deliver: entry needs no recorded case")
	require.Nil(t, got)

	db := casesDB{cases: []*models.TestCase{nil, payCallback("test-2"), payCallback("test-3"), payCallback("test-5"), {Name: "test-4", Kind: models.HTTP}}}
	db.cases[2].HTTPReq.Body = `{"order":"o-1","status":"captured"}`
	// Each carries its case's app port and noise, and the replay's own
	// judging settings.
	db.cases[2].AppPort, db.cases[2].Noise = 18110, map[string][]string{"body.received_at": {}}
	judged := svc(db)
	judged.config.Test.GlobalNoise.Global = config.GlobalNoise{"header": {"X-Request-Id": {}}}
	judged.config.Test.IgnoreOrdering, judged.config.Test.CompareAll = true, true
	got, err = judged.recordedDeliveries(ctx, "set", &models.Mapping{TestCases: []models.MappedTestCase{plain, pay, refund}})
	require.NoError(t, err)
	as := func(d models.Delivery, tc *models.TestCase) models.DeliveryCase {
		return models.DeliveryCase{Delivery: d, Req: tc.HTTPReq, Resp: tc.HTTPResp, AppPort: tc.AppPort, Noise: tc.Noise,
			GlobalNoise: map[string]map[string][]string{"header": {"X-Request-Id": {}}}, IgnoreOrdering: true, CompareAll: true}
	}
	require.Equal(t, map[string][]models.DeliveryCase{
		"pay::TestPaid":   {as(pay.Deliver[0], db.cases[2]), as(pay.Deliver[1], db.cases[1])},
		"pay::TestRefund": {as(refund.Deliver[0], db.cases[3])},
	}, got)
	require.Equal(t, uint16(18110), got["pay::TestPaid"][0].AppPort)

	for _, tt := range []struct {
		name string
		db   TestDB
		want string
	}{
		{"cases not read", nil,
			`keploy cannot deliver the recorded callbacks of mock set "set": test "pay::TestPaid" lists deliver: entries, but this keploy reads no recorded cases (mock.recordRequests is false in keploy.yml); set it to true and record the set again`},
		{"cases unreadable", casesDB{err: errors.New("tests/test-2.yaml: yaml: line 3: did not find expected key")},
			`keploy cannot deliver the recorded callbacks of mock set "set": its recorded cases cannot be read: tests/test-2.yaml: yaml: line 3: did not find expected key`},
		{"case missing", casesDB{cases: []*models.TestCase{payCallback("test-3"), payCallback("test-5")}},
			`keploy cannot deliver the recorded callbacks of mock set "set": case "test-2" of test "pay::TestPaid"'s deliver: has no recorded request in keploy/set/tests; restore its file there, or record the set again`},
		{"case not HTTP", casesDB{cases: []*models.TestCase{payCallback("test-2"), {Name: "test-3", Kind: models.GRPC_EXPORT}, payCallback("test-5")}},
			`keploy cannot deliver the recorded callbacks of mock set "set": case "test-3" of test "pay::TestPaid"'s deliver: is a gRPC request; keploy delivers recorded HTTP requests only`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := svc(tt.db).recordedDeliveries(ctx, "set", &models.Mapping{TestCases: []models.MappedTestCase{refund, pay}})
			require.Nil(t, got)
			require.EqualError(t, err, tt.want)
		})
	}
}

const deliverMappings = `version: api.keploy.io/v1beta1
kind: TestMocksMapping
test_set_id: set
tests:
  - id: pay::TestPaid
    mock_entries:
      - name: mock-0
        kind: Http
    cases: [test-1, test-2]
    deliver:
      - case: test-2
        after: mock-0
`

// keploy mock replay hands the agent each test's deliver: entries with the
// table, read from the set's mappings and its cases; a case it cannot read
// refuses the replay before any table is pushed.
func TestReplayPushesEachTestsDeliveries(t *testing.T) {
	provenOn(t)
	for _, tt := range []struct {
		name  string
		cases []*models.TestCase
		want  string
	}{
		{name: "delivered", cases: []*models.TestCase{payCallback("test-1"), payCallback("test-2")}},
		{name: "case missing", cases: []*models.TestCase{payCallback("test-1")},
			want: `keploy cannot deliver the recorded callbacks of mock set "set": case "test-2" of test "pay::TestPaid"'s deliver: has no recorded request in keploy/set/tests; restore its file there, or record the set again`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			t.Chdir(repo)
			keployDir := filepath.Join(repo, "keploy")
			require.NoError(t, os.MkdirAll(filepath.Join(keployDir, "set"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(keployDir, "set", "mappings.yaml"), []byte(deliverMappings), 0o644))
			instr := newInstr(t, agentUpFromSetup, false, models.AppError{AppErrorType: models.ErrAppStopped})
			cfg := instrConfig(instr, utils.Native, "go test -p 1 -json ./...")
			cfg.Path, cfg.Mock.OnMiss = keployDir, "fail"
			utils.ErrCode = 0
			t.Cleanup(func() { utils.ErrCode = 0 })
			svc := New(zap.NewNop(), instr, named("mock-0"), mapdb.New(zap.NewNop(), keployDir, ""), nil, nil, cfg)
			svc.(TestDBSetter).SetTestDB(casesDB{cases: tt.cases})
			err := svc.Replay(context.Background())
			if tt.want != "" {
				require.ErrorContains(t, err, tt.want)
				require.Nil(t, instr.table, "no table is pushed without its deliveries")
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{"mock-0"}, instr.table["pay::TestPaid"])
			require.Equal(t, map[string][]models.DeliveryCase{"pay::TestPaid": {{
				Delivery: models.Delivery{Case: "test-2", After: "mock-0"}, Req: tt.cases[1].HTTPReq, Resp: tt.cases[1].HTTPResp,
			}}}, instr.deliver)
		})
	}
}
