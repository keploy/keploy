package mockdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// TestPayloadSource_BatchLoadSingleOpen proves that reading back a 500-mock
// batch from payloadSource uses LoadPayloadBatch (1 os.Open/Stat/Close) instead
// of 500 serial file opens, matches GetTestSetMocks exactly, supports
// BatchPayloadMutator, and leaves zero leaked file descriptors.
func TestPayloadSource_BatchLoadSingleOpen(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	ctx := context.Background()

	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", format)

			base := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
			const n = 500
			for i := 0; i < n; i++ {
				m := &models.Mock{
					Version: models.GetVersion(),
					Kind:    models.HTTP,
					Spec: models.MockSpec{
						Metadata:         map[string]string{"type": "mocks", "operation": fmt.Sprintf("GET /item/%d", i)},
						HTTPReq:          &models.HTTPReq{Method: "GET", URL: fmt.Sprintf("http://svc/item/%d", i), ProtoMajor: 1, ProtoMinor: 1},
						HTTPResp:         &models.HTTPResp{StatusCode: 200, Body: fmt.Sprintf(`{"idx":%d}`, i)},
						ReqTimestampMock: base.Add(time.Duration(i) * time.Millisecond),
						ResTimestampMock: base.Add(time.Duration(i)*time.Millisecond + time.Microsecond),
					},
				}
				if err := ys.InsertMock(ctx, m, "set-0"); err != nil {
					t.Fatalf("InsertMock %d: %v", i, err)
				}
			}
			if err := ys.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			idx, err := ys.IndexTestSetMocks(ctx, "set-0", time.Time{}, time.Time{}, nil, nil)
			if err != nil {
				t.Fatalf("IndexTestSetMocks: %v", err)
			}
			all := append(append([]*models.Mock(nil), idx.Filtered...), idx.Unfiltered...)
			if len(all) != n {
				t.Fatalf("indexed %d mocks (filtered=%d unfiltered=%d), want %d", len(all), len(idx.Filtered), len(idx.Unfiltered), n)
			}

			ps, ok := idx.Payloads.(*payloadSource)
			if !ok {
				t.Fatalf("expected *payloadSource, got %T", idx.Payloads)
			}
			if _, ok := idx.Payloads.(models.BatchPayloadSource); !ok {
				t.Fatalf("payloadSource (%T) does not implement models.BatchPayloadSource", idx.Payloads)
			}

			whole, err := ys.GetTestSetMocks(ctx, "set-0", time.Time{}, time.Time{}, nil, nil)
			if err != nil {
				t.Fatalf("GetTestSetMocks: %v", err)
			}
			wholeAll := append(append([]*models.Mock(nil), whole.Filtered...), whole.Unfiltered...)
			if len(wholeAll) != n {
				t.Fatalf("whole mocks = %d, want %d", len(wholeAll), n)
			}

			beforeOpens := ps.openCount()
			resolved, readBack, err := models.WithPayloads(ctx, all)
			if err != nil {
				t.Fatalf("WithPayloads: %v", err)
			}
			afterOpens := ps.openCount()
			if got := afterOpens - beforeOpens; got != 1 {
				t.Fatalf("WithPayloads(%d mocks) performed %d os.Open calls, want exactly 1", n, got)
			}
			if readBack != n {
				t.Fatalf("readBack = %d, want %d", readBack, n)
			}
			if len(resolved) != n {
				t.Fatalf("resolved %d mocks, want %d", len(resolved), n)
			}
			for i := range resolved {
				if g, w := asTheAgentGetsIt(t, resolved[i]), asTheAgentGetsIt(t, wholeAll[i]); !reflect.DeepEqual(g, w) {
					t.Fatalf("mock %d mismatch:\ngot  %+v\nwant %+v", i, g, w)
				}
			}

			// Verify BatchPayloadMutator is invoked once per batch (not per mock).
			var batchCalls atomic.Int64
			ps.SetBatchPayloadMutator(func(_ context.Context, ms []*models.Mock) error {
				batchCalls.Add(1)
				for _, m := range ms {
					if m.Spec.HTTPResp != nil {
						m.Spec.HTTPResp.Body = "decrypted:" + m.Spec.HTTPResp.Body
					}
				}
				return nil
			})
			mutated, _, err := models.WithPayloads(ctx, all)
			if err != nil {
				t.Fatalf("WithPayloads with batch mutator: %v", err)
			}
			if batchCalls.Load() != 1 {
				t.Fatalf("batch mutator calls = %d, want 1", batchCalls.Load())
			}
			if got := ps.openCount() - afterOpens; got != 1 {
				t.Fatalf("second WithPayloads performed %d opens, want 1", got)
			}
			if mutated[0].Spec.HTTPResp.Body != `decrypted:{"idx":0}` {
				t.Fatalf("mutated body = %q, want decrypted prefix", mutated[0].Spec.HTTPResp.Body)
			}

			// Verify BatchPayloadMutator refuses pool-key mutation.
			ps.SetBatchPayloadMutator(func(_ context.Context, ms []*models.Mock) error {
				ms[0].Name = "tampered"
				return nil
			})
			if _, _, err := models.WithPayloads(ctx, all[:5]); !errors.Is(err, models.ErrPayloadMutatorChangedMock) {
				t.Fatalf("tampered batch mutator returned %v, want ErrPayloadMutatorChangedMock", err)
			}
			ps.SetBatchPayloadMutator(nil)

			// Verify file was closed cleanly (zero FD leak) and replaced/removed detection still works.
			if err := os.Remove(ps.path); err != nil {
				t.Fatalf("remove backing mock file after batch read: %v", err)
			}
			if _, _, err := models.WithPayloads(ctx, all[:1]); !errors.Is(err, models.ErrPayloadFileReplaced) {
				t.Fatalf("batch read after file removal returned %v, want ErrPayloadFileReplaced", err)
			}
		})
	}
}
