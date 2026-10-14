package proxy

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
// complete corpus to the async engine's Load (the engine's Load
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

// LoadAsyncMocks must flush the previous test-set's verdict at the boundary.
//
// Not "before Load" — Load touches no tally state, so that ordering is not a
// real property and asserting it would be theatre. What matters is that the
// flush happens AT ALL on this seam.
//
// This is the only seam that runs once per test-set on EVERY replay path. The
// SetGracefulShutdown seam does not: replay.go gates its per-test-set notify on
// `r.instrument && !serveTest`, and serveTest is true for docker-compose replay
// with mocking on — the DEFAULT — so on that path only the run-level notify
// fires and sets 2..N would stay invisible. Without this test, reverting the
// flush leaves every other test green.
func TestLoadAsyncMocksFlushesTheVerdictAtTheBoundary(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	lane := models.AsyncLane{Name: "L", Type: "fake", ThrottleMs: 10}
	eng := async.NewEngine(zap.NewNop(), []models.AsyncLane{lane},
		map[string]async.AsyncParser{"fake": fakeAsyncParser{}})
	p := &Proxy{logger: zap.New(core), asyncEngine: eng}

	verdicts := func() int { return len(logs.FilterMessage("async egress verdict").All()) }

	// First set: nothing served yet, so the flush must stay silent.
	p.LoadAsyncMocks([]*models.Mock{asyncMock("L", 1, "SET-A")})
	if got := verdicts(); got != 0 {
		t.Fatalf("loading the first set emitted %d verdict lines, want 0", got)
	}

	if rec, _, _ := eng.Decide(context.Background(), lane, &models.Mock{}); rec == nil {
		t.Fatal("precondition: set A must serve")
	}

	// Second set: the boundary must publish set A's verdict.
	p.LoadAsyncMocks([]*models.Mock{asyncMock("L", 1, "SET-B")})
	entries := logs.FilterMessage("async egress verdict").All()
	if len(entries) != 1 {
		t.Fatalf("the test-set boundary emitted %d verdict lines, want 1: without the "+
			"flush in LoadAsyncMocks, every set after the first is invisible on the "+
			"docker-compose and --keep-app-alive paths", len(entries))
	}
	var served int64 = -1
	for _, f := range entries[0].Context {
		if f.Key == "served" {
			served = f.Integer
		}
	}
	if served != 1 {
		t.Fatalf("boundary verdict served=%d, want 1 (set A's tally)", served)
	}

	// The flush must not have skipped or reordered the Load itself.
	rec, _, _ := eng.Decide(context.Background(), lane, &models.Mock{})
	if rec == nil || rec.Spec.HTTPResp.Body != "SET-B" {
		t.Fatalf("after the boundary the engine served %v, want SET-B: the flush "+
			"displaced the corpus replacement", rec)
	}

	// The verdict is a running total, and it prints inside RunTestSet — which
	// the replayer has already announced as the NEXT set — so it must say so.
	var scope string
	for _, f := range entries[0].Context {
		if f.Key == "scope" {
			scope = f.String
		}
	}
	if scope != "cumulative-for-run" {
		t.Fatalf("boundary verdict scope=%q, want cumulative-for-run: without it a "+
			"reader attributes the previous set's numbers to the set just announced",
			scope)
	}
}
