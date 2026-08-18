package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestSetMocksWithWindowAdvancesEngineAfterFirst(t *testing.T) {
	lane := models.AsyncLane{Name: "L", Type: "fake"}
	eng := async.NewEngine(zap.NewNop(), []models.AsyncLane{lane}, nil)
	p := &Proxy{logger: zap.NewNop(), asyncEngine: eng}

	// getMockManager() returns nil for this bare Proxy, so the MockManager
	// path is skipped; only the async advance runs.
	_ = p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, models.BaseTime)
	if got := eng.CompletedForTest(); got != 0 {
		t.Fatalf("after first window completed=%d want 0", got)
	}
	_ = p.SetMocksWithWindow(context.Background(), nil, nil, models.BaseTime, models.BaseTime)
	if got := eng.CompletedForTest(); got != 1 {
		t.Fatalf("after second window completed=%d want 1", got)
	}
}

// fakeAsyncParser is a package-local parser stub: package proxy cannot see the
// async package's test-only fakeParser, so we declare a minimal one here.
type fakeAsyncParser struct{}

func (fakeAsyncParser) MatchesLane(_ *models.Mock, _ models.AsyncLane) bool { return true }
func (fakeAsyncParser) MatchRequestShape(_, _ *models.Mock, _ models.AsyncLane) (bool, string) {
	return true, ""
}
func (fakeAsyncParser) EmptyResponse(_ models.AsyncLane) ([]byte, error)           { return []byte("KA"), nil }
func (fakeAsyncParser) ResponseValueKey(_ *models.Mock, _ models.AsyncLane) string { return "" }

func asyncMock(lane string, seq int, body string) *models.Mock {
	return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
		Async:    &models.AsyncMeta{Lane: lane, Seq: seq, AnchorPos: 0},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Body: body},
	}}
}

// TestLoadAsyncMocksForwardsToEngine proves Proxy.LoadAsyncMocks hands the
// complete corpus to the async engine's run-once Load (the engine's Load
// filter ignores the interleaved non-async mock), and that under the
// value-epoch model two epochs recorded at the same AnchorPos resolve to the
// newest one: both "a" (seq 1) and "b" (seq 2) are effective at completed=0,
// so Decide serves "b" — the last value received at that position.
func TestLoadAsyncMocksForwardsToEngine(t *testing.T) {
	lane := models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10}
	eng := async.NewEngine(zap.NewNop(), []models.AsyncLane{lane}, map[string]async.AsyncParser{"fake": fakeAsyncParser{}})
	p := &Proxy{logger: zap.NewNop(), asyncEngine: eng}

	// mix: one non-async mock must be ignored by the engine's Load filter.
	nonAsync := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{Metadata: map[string]string{}}}
	p.LoadAsyncMocks([]*models.Mock{asyncMock("L", 1, "a"), nonAsync, asyncMock("L", 2, "b")})

	rec, _, _ := eng.Decide(context.Background(), lane, &models.Mock{})
	if rec == nil || rec.Spec.HTTPResp.Body != "b" {
		t.Fatalf("want newest same-AnchorPos epoch 'b', got %v", rec)
	}
}
