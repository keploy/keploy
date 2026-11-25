package conn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// invertedManager is a manager past the startup window (no mock is rescued
// as start-up traffic), with its mocks and mappings on out and maps, and a
// mock its request caused (requested after it) and one requested before it.
func invertedManager(t *testing.T, req time.Time) (mgr *syncMock.SyncMockManager, out chan *models.Mock, maps chan models.TestMockMapping, caused, before *models.Mock) {
	t.Helper()
	mgr = syncMock.New(zap.NewNop())
	out, maps = make(chan *models.Mock, 16), make(chan models.TestMockMapping, 16)
	mgr.SetOutputChannel(out)
	mctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr.SetMappingChannel(mctx, maps)
	mgr.SetFirstRequestSignaled()
	old := time.Now().Add(-time.Minute)
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		mgr.ResolveRange(old, old, "", true, false)
	}
	perTest := models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}
	before = &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: req.Add(-50 * time.Millisecond)}, TestModeInfo: perTest}
	caused = &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: req.Add(10 * time.Millisecond)}, TestModeInfo: perTest}
	mgr.AddMock(before)
	mgr.AddMock(caused)
	return mgr, out, maps, caused, before
}

// invertedMapping waits for the mapping of test and returns its mock ids.
func invertedMapping(t *testing.T, maps chan models.TestMockMapping, test string) []string {
	t.Helper()
	for {
		select {
		case m := <-maps:
			if m.TestName == test {
				return m.MockIDs
			}
		case <-time.After(10 * time.Second): // only turns a hang into a failure
			t.Fatalf("no mapping of %s", test)
			return nil
		}
	}
}

// Capture leaves an inverted HTTP test case (response dated before request)
// out fail-closed so no test case is saved without guaranteed mocks, closes
// its window, counts it in InvertedTestCases, logs populated diagnostics
// without echoing raw client-supplied Keploy-Test-Name headers, and emits
// protocol/ingress/clock at Debug.
func TestCaptureSynchronousFailsClosedOnInvertedTestCase(t *testing.T) {
	req := time.Now().Add(-time.Second)
	res := req.Add(-100 * time.Millisecond)
	mgr, out, maps, _, _ := invertedManager(t, req)
	core, logs := observer.New(zap.WarnLevel)
	mgr.SetLogger(zap.New(core))
	dbgCore, dbgLogs := observer.New(zap.DebugLevel)
	w := mgr.OpenWindow(req, nil)
	tcChan := make(chan *models.TestCase, 1)
	ctx := WithWindowClock(syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w), "proxy-clock")
	httpReq := httptest.NewRequest(http.MethodGet, "http://app.local/inverted/1", nil)
	httpReq.Header.Set("Keploy-Test-Name", "raw-client-header-secret")
	Capture(ctx, zap.New(dbgCore), tcChan, httpReq, windowResp(), req, res, models.IncomingOptions{}, true, true, 8080)
	select {
	case tc := <-tcChan:
		t.Fatalf("inverted test case %q was emitted instead of failing closed", tc.Name)
	default:
	}
	select {
	case m := <-out:
		t.Fatalf("inverted Capture emitted mock %q instead of failing closed", m.Name)
	default:
	}
	select {
	case mp := <-maps:
		t.Fatalf("inverted Capture emitted mapping %+v instead of failing closed", mp)
	default:
	}
	if n := mgr.InvertedTestCases(); n != 1 {
		t.Fatalf("%d inverted test cases counted, want 1", n)
	}
	if n := mgr.OpenWindows(); n != 0 {
		t.Fatalf("%d windows still open, want 0", n)
	}
	warns := logs.All()
	if len(warns) != 1 {
		t.Fatalf("got %d warnings, want 1", len(warns))
	}
	f := warns[0].ContextMap()
	if f["test"] == "" || f["test"] == "raw-client-header-secret" || f["protocol"] != "http" || f["ingress"] != "hooks.Capture" || f["clock"] != "proxy-clock" {
		t.Fatalf("unexpected Capture diagnostic fields: %v", f)
	}
	if f["next_step"] != models.InvertedLeftOutNextStep {
		t.Fatalf("next_step = %v, want %q", f["next_step"], models.InvertedLeftOutNextStep)
	}
	dbgs := dbgLogs.FilterMessage("Test case left out fail-closed: response timestamp precedes request timestamp").All()
	if len(dbgs) != 1 {
		t.Fatalf("got %d debug entries, want 1", len(dbgs))
	}
	df := dbgs[0].ContextMap()
	if df["protocol"] != "http" || df["ingress"] != "hooks.Capture" || df["clock"] != "proxy-clock" {
		t.Fatalf("unexpected Capture debug diagnostic fields: %v", df)
	}
}

// Capture without --sync also leaves an inverted test case out fail-closed and
// counts it.
func TestCaptureWithoutSyncFailsClosedOnInvertedTestCase(t *testing.T) {
	req := time.Now().Add(-time.Second)
	mgr, _, _, _, _ := invertedManager(t, req)
	w := mgr.OpenWindow(req, nil)
	tcChan := make(chan *models.TestCase, 1)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	Capture(ctx, zap.NewNop(), tcChan, httptest.NewRequest(http.MethodGet, "http://app.local/inverted/2", nil), windowResp(), req, req.Add(-time.Millisecond), models.IncomingOptions{}, false, false, 8080)
	select {
	case tc := <-tcChan:
		t.Fatalf("inverted test case %q was emitted without --sync instead of failing closed", tc.Name)
	default:
	}
	if n := mgr.InvertedTestCases(); n != 1 {
		t.Fatalf("%d inverted test cases counted, want 1", n)
	}
	if n := mgr.OpenWindows(); n != 0 {
		t.Fatalf("%d windows still open, want 0", n)
	}
}

// A test case in order is resolved over its captured timestamps as usual.
func TestCaptureSynchronousResolvesATestCaseInOrderOverItsStamps(t *testing.T) {
	req := time.Now().Add(-time.Second)
	mgr, _, maps, caused, _ := invertedManager(t, req)
	late := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: req.Add(30 * time.Millisecond)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
	mgr.AddMock(late)
	w := mgr.OpenWindow(req, nil)
	tcChan := make(chan *models.TestCase, 1)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	Capture(ctx, zap.NewNop(), tcChan, httptest.NewRequest(http.MethodGet, "http://app.local/in-order/1", nil), windowResp(), req, req.Add(20*time.Millisecond), models.IncomingOptions{}, true, true, 8080)
	var tc *models.TestCase
	select {
	case tc = <-tcChan:
	default:
		t.Fatal("the test case in order was not recorded")
	}
	if got := invertedMapping(t, maps, tc.Name); len(got) != 1 || got[0] != caused.Name {
		t.Fatalf("mapped %v to the test case in order, want only the mock inside its stamps", got)
	}
	if n := mgr.InvertedTestCases(); n != 0 {
		t.Fatalf("%d inverted test cases counted for one in order", n)
	}
}

// CaptureGRPC leaves an inverted gRPC call out fail-closed, closes its window,
// counts it, and logs populated diagnostics.
func TestCaptureGRPCSynchronousFailsClosedOnInvertedCall(t *testing.T) {
	origHook := GRPCCaptureHook
	GRPCCaptureHook = nil
	defer func() { GRPCCaptureHook = origHook }()
	req := time.Now().Add(-time.Second)
	res := req.Add(-100 * time.Millisecond)
	mgr, _, _, _, _ := invertedManager(t, req)
	core, logs := observer.New(zap.WarnLevel)
	mgr.SetLogger(zap.New(core))
	w := mgr.OpenWindow(req, nil)
	stream := &pkg.HTTP2Stream{
		GRPCReq: &models.GrpcReq{
			Timestamp: req,
			Headers:   models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/orders.OrderService/GetOrder"}},
		},
		GRPCResp: &models.GrpcResp{Timestamp: res},
	}
	queue := make(chan *models.TestCase, 1)
	ctx := WithWindowClock(syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w), "grpc-clock")
	CaptureGRPC(ctx, zap.NewNop(), queue, stream, 9090, true, true)
	select {
	case tc := <-queue:
		t.Fatalf("inverted gRPC test case %q was emitted instead of failing closed", tc.Name)
	default:
	}
	if n := mgr.InvertedTestCases(); n != 1 {
		t.Fatalf("%d inverted test cases counted, want 1", n)
	}
	if n := mgr.OpenWindows(); n != 0 {
		t.Fatalf("%d windows still open, want 0", n)
	}
	warns := logs.All()
	if len(warns) != 1 {
		t.Fatalf("got %d warnings, want 1", len(warns))
	}
	f := warns[0].ContextMap()
	if f["test"] == "" || f["protocol"] != "grpc" || f["ingress"] != "hooks.CaptureGRPC" || f["clock"] != "grpc-clock" {
		t.Fatalf("unexpected CaptureGRPC diagnostic fields: %v", f)
	}
}

// CaptureGRPC without --sync (no test-N name assigned) still populates a
// non-empty test identifier from the gRPC path when failing closed.
func TestCaptureGRPCWithoutSyncFailsClosedOnInvertedCall(t *testing.T) {
	origHook := GRPCCaptureHook
	GRPCCaptureHook = nil
	defer func() { GRPCCaptureHook = origHook }()
	req := time.Now().Add(-time.Second)
	res := req.Add(-50 * time.Millisecond)
	mgr, _, _, _, _ := invertedManager(t, req)
	core, logs := observer.New(zap.WarnLevel)
	mgr.SetLogger(zap.New(core))
	w := mgr.OpenWindow(req, nil)
	stream := &pkg.HTTP2Stream{
		GRPCReq: &models.GrpcReq{
			Timestamp: req,
			Headers:   models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/orders.OrderService/Create"}},
		},
		GRPCResp: &models.GrpcResp{Timestamp: res},
	}
	queue := make(chan *models.TestCase, 1)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	CaptureGRPC(ctx, zap.NewNop(), queue, stream, 9090, false, false)
	select {
	case tc := <-queue:
		t.Fatalf("inverted gRPC test case %q was emitted without --sync", tc.Name)
	default:
	}
	if n := mgr.InvertedTestCases(); n != 1 {
		t.Fatalf("%d inverted test cases counted, want 1", n)
	}
	warns := logs.All()
	if len(warns) != 1 {
		t.Fatalf("got %d warnings, want 1", len(warns))
	}
	if got, _ := warns[0].ContextMap()["test"].(string); got == "" {
		t.Fatalf("CaptureGRPC non-sync drop logged empty test field: %v", warns[0].ContextMap())
	}
}

// An inverted gRPC call must fail closed BEFORE GRPCCaptureHook (static dedup)
// and BEFORE NextTestID so an inverted first call never marks a schema as seen
// (which would suppress every later healthy call of that endpoint) and never
// burns a test ID.
func TestCaptureGRPC_InvertedCallSkipsHookAndNextTestID(t *testing.T) {
	origHook := GRPCCaptureHook
	defer func() { GRPCCaptureHook = origHook }()

	seen := map[string]bool{}
	hookCalls := 0
	GRPCCaptureHook = func(req *models.GrpcReq, _ *models.GrpcResp) (bool, string) {
		hookCalls++
		key := req.Headers.PseudoHeaders[":path"]
		if seen[key] {
			return true, key
		}
		seen[key] = true
		return false, key
	}

	req := time.Now().Add(-time.Second)
	mgr, _, _, _, _ := invertedManager(t, req)
	queue := make(chan *models.TestCase, 2)

	// 1. First call to /orders.OrderService/GetOrder is inverted.
	winInv := mgr.OpenWindow(req, nil)
	invStream := &pkg.HTTP2Stream{
		GRPCReq: &models.GrpcReq{
			Timestamp: req,
			Headers:   models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/orders.OrderService/GetOrder"}},
		},
		GRPCResp: &models.GrpcResp{Timestamp: req.Add(-50 * time.Millisecond)},
	}
	ctxInv := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), winInv)
	CaptureGRPC(ctxInv, zap.NewNop(), queue, invStream, 9090, true, true)

	if hookCalls != 0 {
		t.Fatalf("GRPCCaptureHook called %d times on inverted call, want 0", hookCalls)
	}
	if n := mgr.InvertedTestCases(); n != 1 {
		t.Fatalf("InvertedTestCases = %d, want 1", n)
	}

	// 2. Subsequent healthy call to the same endpoint must NOT be treated as a
	// duplicate and must receive test-1 (proving NextTestID was not burned).
	reqOK := time.Now().Add(-500 * time.Millisecond)
	winOK := mgr.OpenWindow(reqOK, nil)
	okStream := &pkg.HTTP2Stream{
		GRPCReq: &models.GrpcReq{
			Timestamp: reqOK,
			Headers:   models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/orders.OrderService/GetOrder"}},
		},
		GRPCResp: &models.GrpcResp{Timestamp: reqOK.Add(10 * time.Millisecond)},
	}
	ctxOK := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), winOK)
	CaptureGRPC(ctxOK, zap.NewNop(), queue, okStream, 9090, true, true)

	if hookCalls != 1 {
		t.Fatalf("GRPCCaptureHook called %d times after healthy call, want 1", hookCalls)
	}
	select {
	case tc := <-queue:
		if tc.Name != "test-1" {
			t.Fatalf("healthy call got Name=%q, want test-1 (inverted call must not burn NextTestID)", tc.Name)
		}
	default:
		t.Fatal("healthy gRPC call after inverted first call was hidden/dropped instead of recorded")
	}
}

// Even when every captured HTTP or gRPC test case is inverted, Capture and
// CaptureGRPC still sweep stale out-of-window mocks from the buffer so memory
// stays bounded.
func TestCaptureAndCaptureGRPC_InvertedStillSweepStaleMocks(t *testing.T) {
	origHook := GRPCCaptureHook
	GRPCCaptureHook = nil
	defer func() { GRPCCaptureHook = origHook }()

	now := time.Now()
	staleAt := now.Add(-(syncMock.StaleHorizon + 3*time.Second))
	perTest := models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}

	// HTTP Capture sweep check.
	mgrHTTP := syncMock.New(zap.NewNop())
	outHTTP := make(chan *models.Mock, 16)
	mgrHTTP.SetOutputChannel(outHTTP)
	mgrHTTP.SetFirstRequestSignaled()
	old := now.Add(-time.Minute)
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		mgrHTTP.ResolveRange(old, old, "", true, false)
	}
	mgrHTTP.AddMock(&models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: staleAt}, TestModeInfo: perTest})
	if _, _, _, buf := mgrHTTP.GetDropStats(); buf != 1 {
		t.Fatalf("HTTP setup buffer = %d, want 1 stale mock", buf)
	}
	reqHTTP := now.Add(-time.Second)
	winHTTP := mgrHTTP.OpenWindow(reqHTTP, nil)
	ctxHTTP := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgrHTTP), winHTTP)
	Capture(ctxHTTP, zap.NewNop(), make(chan *models.TestCase, 1), httptest.NewRequest(http.MethodGet, "http://app.local/stale-sweep", nil), windowResp(), reqHTTP, reqHTTP.Add(-10*time.Millisecond), models.IncomingOptions{}, true, false, 8080)
	if _, _, _, buf := mgrHTTP.GetDropStats(); buf != 0 {
		t.Fatalf("HTTP buffer size after inverted Capture = %d, want 0 (stale mock must be swept)", buf)
	}

	// gRPC CaptureGRPC sweep check.
	mgrGRPC := syncMock.New(zap.NewNop())
	outGRPC := make(chan *models.Mock, 16)
	mgrGRPC.SetOutputChannel(outGRPC)
	mgrGRPC.SetFirstRequestSignaled()
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		mgrGRPC.ResolveRange(old, old, "", true, false)
	}
	mgrGRPC.AddMock(&models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: staleAt}, TestModeInfo: perTest})
	if _, _, _, buf := mgrGRPC.GetDropStats(); buf != 1 {
		t.Fatalf("gRPC setup buffer = %d, want 1 stale mock", buf)
	}
	reqGRPC := now.Add(-time.Second)
	winGRPC := mgrGRPC.OpenWindow(reqGRPC, nil)
	ctxGRPC := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgrGRPC), winGRPC)
	stream := &pkg.HTTP2Stream{
		GRPCReq: &models.GrpcReq{
			Timestamp: reqGRPC,
			Headers:   models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/svc.Test/StaleSweep"}},
		},
		GRPCResp: &models.GrpcResp{Timestamp: reqGRPC.Add(-10 * time.Millisecond)},
	}
	CaptureGRPC(ctxGRPC, zap.NewNop(), make(chan *models.TestCase, 1), stream, 9090, true, false)
	if _, _, _, buf := mgrGRPC.GetDropStats(); buf != 0 {
		t.Fatalf("gRPC buffer size after inverted CaptureGRPC = %d, want 0 (stale mock must be swept)", buf)
	}
}
