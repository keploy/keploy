package replay

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// One replay of a test set reads its mock file once. RunTestSet loaded the
// per-test pool, then the session pool, then the session pool again for the
// report's mock lookup, and the YAML mock store read and decoded the whole
// file for each: three decodes of the whole file before the first test could
// run.

// decodeProbeKind is a mock kind only this file records. mockdb decodes every
// document of a kind that has a MockYAMLMapper through that mapper, so the
// mapper below counts decodes: a mock file holding one probe document has
// been decoded as many times as the probe was.
const decodeProbeKind models.Kind = "ReplayDecodeProbe"

var (
	decodeProbeOnce  sync.Once
	decodeProbeCount atomic.Int64
)

func registerDecodeProbe() {
	decodeProbeOnce.Do(func() {
		mockdb.RegisterMockYAMLMapper(decodeProbeKind, mockdb.MockYAMLMapper{
			Encode: func(*models.Mock, *yaml.NetworkTrafficDoc) error { return nil },
			Decode: func(_ *yaml.NetworkTrafficDoc, m *models.Mock) error {
				decodeProbeCount.Add(1)
				m.Spec = models.MockSpec{Metadata: map[string]string{"type": "config"}}
				return nil
			},
		})
	})
}

// useYAMLMocks makes the harness load its mocks from a mocks.yaml in the
// run's directory, through the YAML mock store keploy replays with: the given
// mocks, then one decode-probe document.
func useYAMLMocks(t *testing.T, h *prRun, mocks ...*models.Mock) *mockdb.MockYaml {
	t.Helper()
	registerDecodeProbe()
	t.Setenv("KEPLOY_MOCK_FORMAT", "") // gob would write, and read, a mocks.gob instead
	ys := mockdb.New(zap.NewNop(), h.replayer.config.Path, "mocks")
	for _, m := range mocks {
		if err := ys.InsertMock(context.Background(), m, "test-set-0"); err != nil {
			t.Fatalf("InsertMock: %v", err)
		}
	}
	path := filepath.Join(h.replayer.config.Path, "test-set-0", "mocks.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("---\nversion: api.keploy.io/v1beta1\nkind: " + string(decodeProbeKind) + "\nname: decode-probe\nspec: {}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	h.replayer.mockDB = ys
	return ys
}

// perPoolStore is a mock store that reads one pool per call: the YAML store
// without GetTestSetMocks, as a MockDB from before it, or from elsewhere, is.
type perPoolStore struct{ ys *mockdb.MockYaml }

func (s perPoolStore) GetFilteredMocks(ctx context.Context, id string, after, before time.Time, mapped, needed map[string]bool) ([]*models.Mock, error) {
	return s.ys.GetFilteredMocks(ctx, id, after, before, mapped, needed)
}
func (s perPoolStore) GetUnFilteredMocks(ctx context.Context, id string, after, before time.Time, mapped, needed map[string]bool) ([]*models.Mock, error) {
	return s.ys.GetUnFilteredMocks(ctx, id, after, before, mapped, needed)
}
func (s perPoolStore) UpdateMocks(ctx context.Context, id string, states map[string]models.MockState, pruneBefore, cutoff time.Time) error {
	return s.ys.UpdateMocks(ctx, id, states, pruneBefore, cutoff)
}

func sessionHTTPMock(url string) *models.Mock {
	ts := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	return &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "config"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: url, ProtoMajor: 1, ProtoMinor: 1},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, StatusMessage: "OK", Body: `{"ok":true}`},
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(time.Millisecond),
		},
	}
}

func storedNames(mocks []*models.Mock) map[string]bool {
	names := map[string]bool{}
	for _, m := range mocks {
		names[m.Name] = true
	}
	return names
}

func TestRunTestSetDecodesTheMockFileOnce(t *testing.T) {
	h := newPartialRunHarness(t, 3, 0)
	useYAMLMocks(t, h, sessionHTTPMock("http://orders.internal/v1/orders"))
	before := decodeProbeCount.Load()

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("the run reported %q; want PASSED", status)
	}
	if got := decodeProbeCount.Load() - before; got != 1 {
		t.Fatalf("one replay of the test set decoded its mock file %d times; want 1", got)
	}
	if stored := storedNames(h.instr.storedUnfiltered); !stored["decode-probe"] || !stored["mock-0"] {
		t.Fatalf("the agent was not handed the recorded session mocks; it got %v", stored)
	}
}

// mutatingHooks is a MockMutator: it changes the loaded mocks in place, after
// they are read and before the agent gets them, as a hook that decrypts
// recorded secrets does.
type mutatingHooks struct {
	prHooks
	mutate func(filtered, unfiltered []*models.Mock)
}

func (h mutatingHooks) AfterGetMocks(_ context.Context, filtered, unfiltered []*models.Mock) error {
	h.mutate(filtered, unfiltered)
	return nil
}

// The report describes a mock as it is recorded, not as a MockMutator left
// it: the separate read the report's lookup came from saw the file, and the
// lookup now comes from the read that loads the mocks, which the mutator
// changes in place afterwards. A mutator decrypts secrets for the agent; the
// report must not carry them. A store that reads one pool per call still gets
// the lookup from a read of its own.
func TestRunTestSetReportsMocksAsRecordedNotAsMutated(t *testing.T) {
	const recorded = "http://vault.internal/v1/secret?token=ENC:ciphertext"
	const mutated = "http://vault.internal/v1/secret?token=plaintext"
	for _, store := range []string{"one read", "a read per pool"} {
		t.Run(store, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			ys := useYAMLMocks(t, h, sessionHTTPMock(recorded))
			if store == "a read per pool" {
				h.replayer.mockDB = perPoolStore{ys}
			}
			h.replayer.hookImpl = mutatingHooks{
				prHooks: prHooks{
					consumed:  []models.MockState{{Name: "mock-0", Kind: models.HTTP}},
					wrongBody: map[string]bool{"test-1": true},
				},
				mutate: func(_, unfiltered []*models.Mock) {
					for _, m := range unfiltered {
						if m.Spec.HTTPReq != nil {
							m.Spec.HTTPReq.URL = mutated
						}
					}
				},
			}

			_ = h.run(t)

			var agentGot string
			for _, m := range h.instr.storedUnfiltered {
				if m.Name == "mock-0" {
					agentGot = m.Spec.HTTPReq.URL
				}
			}
			if agentGot != mutated {
				t.Fatalf("precondition: the agent got %q; the mutator did not run before the store", agentGot)
			}
			var matched []models.MatchedCall
			for _, r := range h.report.results {
				if r.TestCaseID == "test-1" {
					matched = r.FailureInfo.MatchedCalls
				}
			}
			if len(matched) != 1 {
				t.Fatalf("want test-1's one matched call in the report, got %+v", matched)
			}
			if want := "GET " + recorded; matched[0].Summary != want {
				t.Fatalf("the report describes mock-0 as %q; want it as recorded, %q", matched[0].Summary, want)
			}
			if strings.Contains(matched[0].Summary, "plaintext") {
				t.Fatalf("the report carries the mutated mock: %q", matched[0].Summary)
			}
		})
	}
}

// perTestGRPCMock is a per-test mock: a gRPC call, a kind the YAML store
// routes by its per-test tag whatever the strict-window setting, which it does
// not do for HTTP, MySQL or Postgres.
func perTestGRPCMock(destAddr, operation string) *models.Mock {
	ts := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	return &models.Mock{
		Version: models.GetVersion(),
		Kind:    models.GRPC_EXPORT,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks", "destAddr": destAddr, "operation": operation},
			GRPCReq:          &models.GrpcReq{},
			GRPCResp:         &models.GrpcResp{},
			ReqTimestampMock: ts,
			ResTimestampMock: ts.Add(time.Millisecond),
		},
	}
}

// mappedTests is a mapping store holding one recorded mapping, so the run
// takes the mapping-based path whose per-test dependency rows the report
// carries.
type mappedTests struct {
	*prMappingDB
	tests map[string][]models.MockEntry
}

func (m mappedTests) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	return m.tests, true, nil
}

// useGobMocks makes the harness load its mocks from a mocks.gob in the run's
// directory, through the YAML mock store, which reads a gob file before a
// text one. Its per-test pool goes through the window filter, as a YAML
// file's does not.
func useGobMocks(t *testing.T, h *prRun, mocks ...*models.Mock) {
	t.Helper()
	t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
	ys := mockdb.New(zap.NewNop(), h.replayer.config.Path, "mocks")
	for _, m := range mocks {
		if err := ys.InsertMock(context.Background(), m, "test-set-0"); err != nil {
			t.Fatalf("InsertMock: %v", err)
		}
	}
	// The gob writer is asynchronous; Close drains it to the file.
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h.replayer.mockDB = mockdb.New(zap.NewNop(), h.replayer.config.Path, "mocks")
}

// The report names the per-test mocks of a failed test, not only its session
// mocks: a matched call's summary and a missing dependency's target come from
// the lookup RunTestSet builds from the mocks it loads, and that lookup held
// the session pool alone, so a per-test mock (the pool a test's own calls are
// recorded in, and the only tier a dependency row is written for) was
// reported with an empty summary and a row with no target. The lookup
// describes the mocks as recorded: before a MockMutator changes them in
// place, and whether or not the window filter kept them for the agent.
func TestRunTestSetReportsTheFailedTestsPerTestMocks(t *testing.T) {
	const (
		ordersOp = "/orders.Orders/Get"
		chargeOp = "/payments.Payments/Charge"
		mutated  = "?token=plaintext"
	)
	for _, tc := range []struct {
		store string
		// decodes is how many times the run decodes the mock file; 0 for a
		// gob file, which the probe cannot count.
		decodes int64
	}{
		{"one read", 1},
		// The per-test pool, the session pool, and the session pool before
		// the prune, as before.
		{"a read per pool", 3},
		// mock-1's response is stamped before its request, so the window
		// filter keeps it from the agent; the recording still maps it to
		// test-1.
		{"a gob file", 0},
	} {
		t.Run(tc.store, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			// mock-0 is the call test-1 made; mock-1 is one it was recorded
			// making and did not make this time.
			orders := perTestGRPCMock("orders.internal:50051", ordersOp)
			charge := perTestGRPCMock("payments.internal:50051", chargeOp)
			switch tc.store {
			case "a gob file":
				charge.Spec.ResTimestampMock = charge.Spec.ReqTimestampMock.Add(-time.Millisecond)
				useGobMocks(t, h, orders, charge)
			case "a read per pool":
				h.replayer.mockDB = perPoolStore{useYAMLMocks(t, h, orders, charge)}
			default:
				useYAMLMocks(t, h, orders, charge)
			}
			before := decodeProbeCount.Load()
			h.replayer.mappingDB = mappedTests{prMappingDB: h.mappings, tests: map[string][]models.MockEntry{
				"test-1": {{Name: "mock-0", Kind: string(models.GRPC_EXPORT)}, {Name: "mock-1", Kind: string(models.GRPC_EXPORT)}},
			}}
			h.replayer.hookImpl = mutatingHooks{
				prHooks: prHooks{
					consumed:  []models.MockState{{Name: "mock-0", Kind: models.GRPC_EXPORT}},
					wrongBody: map[string]bool{"test-1": true},
				},
				mutate: func(filtered, _ []*models.Mock) {
					for _, m := range filtered {
						if m.Kind == models.GRPC_EXPORT {
							m.Spec.Metadata["operation"] += mutated
						}
					}
				},
			}

			_ = h.run(t)

			if tc.decodes != 0 {
				if got := decodeProbeCount.Load() - before; got != tc.decodes {
					t.Fatalf("the run decoded its mock file %d times; want %d", got, tc.decodes)
				}
			}
			agentGot := map[string]string{}
			for _, m := range h.instr.storedFiltered {
				agentGot[m.Name] = m.Spec.Metadata["operation"]
			}
			wantAgent := map[string]string{"mock-0": ordersOp + mutated, "mock-1": chargeOp + mutated}
			if tc.store == "a gob file" {
				delete(wantAgent, "mock-1")
			}
			if !reflect.DeepEqual(agentGot, wantAgent) {
				t.Fatalf("precondition: the agent's per-test pool is %v; want %v", agentGot, wantAgent)
			}
			var got *models.TestResult
			for i := range h.report.results {
				if h.report.results[i].TestCaseID == "test-1" {
					got = &h.report.results[i]
				}
			}
			// The missing mock-1 makes the failed test OBSOLETE; the report
			// carries a test's matched calls when it is either.
			if got == nil || (got.Status != models.TestStatusFailed && got.Status != models.TestStatusObsolete) {
				t.Fatalf("precondition: want test-1 FAILED or OBSOLETE in the report, got %+v", got)
			}
			matched := got.FailureInfo.MatchedCalls
			if len(matched) != 1 || matched[0].MockName != "mock-0" {
				t.Fatalf("want test-1's one matched call, mock-0, in the report, got %+v", matched)
			}
			if want := "gRPC " + ordersOp; matched[0].Summary != want {
				t.Fatalf("the report describes test-1's per-test mock-0 as %q; want it as recorded, %q", matched[0].Summary, want)
			}
			var rows []string
			for _, row := range got.Result.DepResult {
				rows = append(rows, row.Name)
			}
			want := models.DepRowName(1, models.DepTypeGRPC, "payments.internal:50051 "+chargeOp)
			if len(rows) != 1 || rows[0] != want {
				t.Fatalf("test-1's dependency rows are %q; want the missing per-test mock-1 named with its target as recorded, %q", rows, want)
			}
		})
	}
}

// A test set whose mocks RunTestSet does not load (a compose app with no agent
// to load them into) has its lookup read on its own, and it too names the
// per-test mocks.
func TestRunTestSetReportsPerTestMocksItDoesNotLoad(t *testing.T) {
	const ordersOp = "/orders.Orders/Get"
	for _, tc := range []struct {
		store   string
		decodes int64
	}{
		{"one read", 1},
		// One read per pool: the per-test pool can come from no other read.
		{"a read per pool", 2},
	} {
		t.Run(tc.store, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.instrument = false
			h.replayer.config.CommandType = string(utils.DockerCompose)
			ys := useYAMLMocks(t, h, perTestGRPCMock("orders.internal:50051", ordersOp))
			if tc.store == "a read per pool" {
				h.replayer.mockDB = perPoolStore{ys}
			}
			before := decodeProbeCount.Load()
			h.replayer.hookImpl = prHooks{
				consumed:  []models.MockState{{Name: "mock-0", Kind: models.GRPC_EXPORT}},
				wrongBody: map[string]bool{"test-1": true},
			}

			_ = h.run(t)

			if h.instr.storedFiltered != nil || h.instr.storedUnfiltered != nil {
				t.Fatalf("precondition: the run loaded mocks into the agent; this is the path that does not")
			}
			if got := decodeProbeCount.Load() - before; got != tc.decodes {
				t.Fatalf("the lookup decoded the mock file %d times; want %d", got, tc.decodes)
			}
			var matched []models.MatchedCall
			for _, r := range h.report.results {
				if r.TestCaseID == "test-1" {
					matched = r.FailureInfo.MatchedCalls
				}
			}
			if len(matched) != 1 || matched[0].MockName != "mock-0" {
				t.Fatalf("want test-1's one matched call, mock-0, in the report, got %+v", matched)
			}
			if want := "gRPC " + ordersOp; matched[0].Summary != want {
				t.Fatalf("the report describes test-1's per-test mock-0 as %q; want %q", matched[0].Summary, want)
			}
		})
	}
}
