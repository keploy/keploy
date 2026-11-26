package replay

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

type batchCapableSource struct {
	mut      models.PayloadMutator
	batchMut models.BatchPayloadMutator
	payloads map[string]*models.Mock
}

func (s *batchCapableSource) LoadPayload(_ context.Context, at models.PayloadAt) (*models.Mock, error) {
	m, ok := s.payloads[at.Name]
	if !ok {
		return nil, fmt.Errorf("missing mock %q", at.Name)
	}
	return m.ShallowCopy(), nil
}

func (s *batchCapableSource) LoadPayloadBatch(ctx context.Context, ats []models.PayloadAt) ([]*models.Mock, error) {
	out := make([]*models.Mock, len(ats))
	for i, at := range ats {
		m, err := s.LoadPayload(ctx, at)
		if err != nil {
			return nil, err
		}
		out[i] = m
	}
	return out, nil
}

func (s *batchCapableSource) SetPayloadMutator(m models.PayloadMutator)           { s.mut = m }
func (s *batchCapableSource) PayloadMutator() models.PayloadMutator               { return s.mut }
func (s *batchCapableSource) SetBatchPayloadMutator(m models.BatchPayloadMutator) { s.batchMut = m }
func (s *batchCapableSource) BatchPayloadMutator() models.BatchPayloadMutator     { return s.batchMut }

type prefetchTestHooks struct {
	TestHooks
	prefetchCalls int
	prefetchSizes []int
	prefetchErr   error
	mutateOrder   []string
	failMockName  string
	prefetched    map[string]bool
}

func (h *prefetchTestHooks) PrefetchMocks(_ context.Context, mocks []*models.Mock) error {
	h.prefetchCalls++
	h.prefetchSizes = append(h.prefetchSizes, len(mocks))
	if h.prefetchErr != nil {
		return h.prefetchErr
	}
	if h.prefetched == nil {
		h.prefetched = make(map[string]bool, len(mocks))
	}
	for _, m := range mocks {
		h.prefetched[m.Name] = true
	}
	return nil
}

func (h *prefetchTestHooks) MutateMockPayload(_ context.Context, m *models.Mock) error {
	if !h.prefetched[m.Name] {
		return fmt.Errorf("mock %q mutated before PrefetchMocks ran", m.Name)
	}
	if h.failMockName != "" && m.Name == h.failMockName {
		return errors.New("decrypt refused")
	}
	h.mutateOrder = append(h.mutateOrder, m.Name)
	return nil
}

func TestReplayer_WiresMockPayloadBatchPrefetcherToBatchSource(t *testing.T) {
	ctx := context.Background()

	t.Run("disk-backed batch calls PrefetchMocks once before MutateMockPayload", func(t *testing.T) {
		hooks := &prefetchTestHooks{}
		r := &Replayer{hookImpl: hooks}
		src := &batchCapableSource{payloads: make(map[string]*models.Mock)}
		stubs := make([]*models.Mock, 5)
		for i := range stubs {
			name := fmt.Sprintf("m-%d", i)
			full := &models.Mock{
				Kind: models.HTTP,
				Name: name,
				Spec: models.MockSpec{
					HTTPReq:  &models.HTTPReq{URL: "http://example.test"},
					HTTPResp: &models.HTTPResp{StatusCode: 200, Body: "ENC:v1:abc"},
				},
			}
			src.payloads[name] = full
			stub := full.ShallowCopy()
			stub.LeavePayloadOnDisk(src, models.PayloadAt{Name: name, Offset: int64(i), Length: 10})
			stubs[i] = stub
		}

		if err := r.mutatePayloads(ctx, src, stubs); err != nil {
			t.Fatalf("mutatePayloads: %v", err)
		}
		if src.BatchPayloadMutator() == nil {
			t.Fatalf("expected BatchPayloadMutator to be registered on batch-capable source")
		}
		if _, _, err := models.WithPayloads(ctx, stubs); err != nil {
			t.Fatalf("WithPayloads: %v", err)
		}
		if hooks.prefetchCalls != 1 || len(hooks.prefetchSizes) != 1 || hooks.prefetchSizes[0] != 5 {
			t.Fatalf("prefetch calls=%d sizes=%v, want 1 call with size 5", hooks.prefetchCalls, hooks.prefetchSizes)
		}
		if len(hooks.mutateOrder) != 5 {
			t.Fatalf("mutated %d mocks, want 5", len(hooks.mutateOrder))
		}
	})

	t.Run("per-mock refusal names the exact failing mock", func(t *testing.T) {
		hooks := &prefetchTestHooks{failMockName: "m-2"}
		r := &Replayer{hookImpl: hooks}
		src := &batchCapableSource{payloads: make(map[string]*models.Mock)}
		stubs := make([]*models.Mock, 4)
		for i := range stubs {
			name := fmt.Sprintf("m-%d", i)
			full := &models.Mock{Kind: models.HTTP, Name: name}
			src.payloads[name] = full
			stub := full.ShallowCopy()
			stub.LeavePayloadOnDisk(src, models.PayloadAt{Name: name, Offset: int64(i), Length: 10})
			stubs[i] = stub
		}
		if err := r.mutatePayloads(ctx, src, stubs); err != nil {
			t.Fatalf("mutatePayloads: %v", err)
		}
		_, _, err := models.WithPayloads(ctx, stubs)
		var ref *models.MockPayloadRefusal
		if !errors.As(err, &ref) {
			t.Fatalf("expected MockPayloadRefusal, got %v", err)
		}
		if ref.Mock != "m-2" {
			t.Fatalf("refusal mock = %q, want %q", ref.Mock, "m-2")
		}
	})

	t.Run("in-memory pool prefetches once before per-mock MutateMockPayload", func(t *testing.T) {
		hooks := &prefetchTestHooks{}
		r := &Replayer{hookImpl: hooks}
		pool := []*models.Mock{
			{Kind: models.HTTP, Name: "mem-0"},
			{Kind: models.HTTP, Name: "mem-1"},
			{Kind: models.HTTP, Name: "mem-2"},
		}
		if err := r.mutatePayloads(ctx, nil, pool); err != nil {
			t.Fatalf("mutatePayloads(in-mem): %v", err)
		}
		if hooks.prefetchCalls != 1 || hooks.prefetchSizes[0] != 3 {
			t.Fatalf("in-mem prefetch calls=%d sizes=%v, want 1 call of 3", hooks.prefetchCalls, hooks.prefetchSizes)
		}
		if len(hooks.mutateOrder) != 3 {
			t.Fatalf("in-mem mutated %d mocks, want 3", len(hooks.mutateOrder))
		}
	})

	t.Run("prefetch error fails closed", func(t *testing.T) {
		wantErr := errors.New("transit key unavailable")
		hooks := &prefetchTestHooks{prefetchErr: wantErr}
		r := &Replayer{hookImpl: hooks}
		src := &batchCapableSource{payloads: map[string]*models.Mock{"m-0": {Kind: models.HTTP, Name: "m-0"}}}
		stub := (&models.Mock{Kind: models.HTTP, Name: "m-0"}).ShallowCopy()
		stub.LeavePayloadOnDisk(src, models.PayloadAt{Name: "m-0", Length: 10})
		if err := r.mutatePayloads(ctx, src, []*models.Mock{stub}); err != nil {
			t.Fatalf("mutatePayloads: %v", err)
		}
		if _, _, err := models.WithPayloads(ctx, []*models.Mock{stub}); !errors.Is(err, wantErr) {
			t.Fatalf("WithPayloads err = %v, want %v", err, wantErr)
		}
	})
}
