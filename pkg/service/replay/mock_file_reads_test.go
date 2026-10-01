package replay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
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
