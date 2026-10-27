package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The async lanes count the windows of tests (async.Engine.AdvanceWindow):
// keploy test stages a test set's mocks from models.BaseTime before any test
// runs, which opens no window, and then opens one per test. An epoch received
// after test k started is effective from test k+1. Counting the set's staging
// as the first window served it to test k (test 8, GET /value 7, of a
// recording answered with the value the config flipped to after it).
func TestSetMocksWithWindowCountsTestWindowsOnly(t *testing.T) {
	lane := models.AsyncLane{Name: "L", Type: "fake"}
	eng := async.NewEngine(zap.NewNop(), []models.AsyncLane{lane}, map[string]async.AsyncParser{"fake": fakeAsyncParser{}})
	p := &Proxy{logger: zap.NewNop(), asyncEngine: eng}
	ctx := context.Background()
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	afterTest1 := asyncMock("L", 2, "after-test-1")
	afterTest1.Spec.Async.AnchorPos = 1
	p.LoadAsyncMocks([]*models.Mock{asyncMock("L", 1, "boot"), afterTest1})
	served := func() string {
		rec, _, _ := eng.Decide(ctx, lane, &models.Mock{})
		if rec == nil {
			return "<keep-alive>"
		}
		return rec.Spec.HTTPResp.Body
	}

	// getMockManager() returns nil for this bare Proxy, so the MockManager
	// path is skipped; only the async advance runs.
	_ = p.SetMocksWithWindow(ctx, nil, nil, models.BaseTime, time.Now())
	_ = p.SetMocksWithWindow(ctx, nil, nil, models.BaseTime, time.Now()) // staged again (an agent replaced)
	if got := eng.CompletedForTest(); got != 0 {
		t.Fatalf("the test set's stagings completed %d tests, want 0", got)
	}
	_ = p.SetMocksWithWindow(ctx, nil, nil, t0, t0.Add(time.Second))
	if got, body := eng.CompletedForTest(), served(); got != 0 || body != "boot" {
		t.Fatalf("test 1 runs with completed=%d, served %q; want 0 and the boot value", got, body)
	}
	_ = p.SetMocksWithWindow(ctx, nil, nil, t0.Add(time.Minute), t0.Add(time.Minute+time.Second))
	if got, body := eng.CompletedForTest(), served(); got != 1 || body != "after-test-1" {
		t.Fatalf("test 2 runs with completed=%d, served %q; want 1 and the value received after test 1", got, body)
	}
	// A test recorded without times opens a window all the same.
	_ = p.SetMocksWithWindow(ctx, nil, nil, time.Time{}, time.Time{})
	if got := eng.CompletedForTest(); got != 2 {
		t.Fatalf("test 3, recorded without times, runs with completed=%d, want 2", got)
	}
}

// keploy mock replay opens a test (SetScopeMocks, open) and ends it (!open):
// only the opening is a window.
func TestSetScopeMocksCountsTheTestsBegun(t *testing.T) {
	lane := models.AsyncLane{Name: "L", Type: "fake"}
	eng := async.NewEngine(zap.NewNop(), []models.AsyncLane{lane}, nil)
	p := &Proxy{logger: zap.NewNop(), asyncEngine: eng}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		require.NoError(t, p.SetScopeMocks(ctx, nil, nil, true))
		require.NoError(t, p.SetScopeMocks(ctx, nil, nil, false))
	}
	require.Equal(t, 2, eng.CompletedForTest(), "three tests begun and ended: the third runs after two completed")
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

// Under exact serving (KEPLOY_PROVEN) keploy test opens each test case
// with ServeExactTest instead of a per-test SetMocksWithWindow: the async
// lanes advance just as they did, so an epoch recorded as effective after
// test 1 is served once test 1 completed (round-3 review).
func TestExactServingAdvancesTheAsyncLanesAsPerTestWindowsDid(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	run := func(exact bool) (int, string) {
		lane := models.AsyncLane{Name: "L", Type: "fake"}
		eng := async.NewEngine(zap.NewNop(), []models.AsyncLane{lane}, map[string]async.AsyncParser{"fake": fakeAsyncParser{}})
		p := &Proxy{logger: zap.NewNop(), asyncEngine: eng, dnsCache: newDNSCache()}
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		p.setMockManager(mm)
		later := asyncMock("L", 2, "after-test-1")
		later.Spec.Async.AnchorPos = 1
		p.LoadAsyncMocks([]*models.Mock{asyncMock("L", 1, "boot"), later})
		if exact {
			require.NoError(t, p.ServeExactPool(models.PoolKeyNone, nil))
		}
		require.NoError(t, p.SetMocksWithWindow(ctx, nil, nil, models.BaseTime, time.Now()))
		for i, tc := range []string{"test-1", "test-2", "test-3"} {
			if exact {
				require.NoError(t, p.ServeExactTest(tc, nil))
				require.NoError(t, p.ServeExactPool(models.PoolKeyNone, nil))
				continue
			}
			at := t0.Add(time.Duration(i) * time.Minute)
			require.NoError(t, p.SetMocksWithWindow(ctx, nil, nil, at, at.Add(time.Second)))
		}
		rec, _, _ := eng.Decide(ctx, lane, &models.Mock{})
		body := "<keep-alive>"
		if rec != nil {
			body = rec.Spec.HTTPResp.Body
		}
		return eng.CompletedForTest(), body
	}
	wantN, wantBody := run(false)
	gotN, gotBody := run(true)
	require.Equal(t, "after-test-1", wantBody, "the per-test windows control")
	require.Equal(t, wantN, gotN, "exact serving advanced the lanes a different number of times")
	require.Equal(t, wantBody, gotBody)
}
