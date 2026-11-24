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
)

// invertedManager is a manager past the startup window (no mock is rescued
// as start-up traffic), with its mocks and mappings on out and maps, and a
// mock its request caused (requested after it) and one requested before it,
// after its response's stamp.
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

// A synchronous capture of a request whose response is dated before it keeps
// its test case, with the mock its request caused: its window ends when the
// capture took the completed exchange (TestCase.WindowEnd), never before its
// request, and the test case carries that end for its hold and its stream's
// checks. Its stamps stay as captured, and it is counted once. Over its
// stamps, a window that ends before it starts, it was recorded with no mock.
func TestCaptureSynchronousKeepsAnInvertedTestCaseWithItsMocks(t *testing.T) {
	req := time.Now().Add(-time.Second)
	res := req.Add(-100 * time.Millisecond)
	mgr, out, maps, caused, _ := invertedManager(t, req)
	w := mgr.OpenWindow(req, nil)
	tcChan := make(chan *models.TestCase, 1)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	captured := time.Now()
	Capture(ctx, zap.NewNop(), tcChan, httptest.NewRequest(http.MethodGet, "http://app.local/inverted/1", nil), windowResp(), req, res, models.IncomingOptions{}, true, true, 8080)
	var tc *models.TestCase
	select {
	case tc = <-tcChan:
	default:
		t.Fatal("the inverted test case was not recorded")
	}
	if !tc.HTTPReq.Timestamp.Equal(req) || !tc.HTTPResp.Timestamp.Equal(res) {
		t.Fatal("the inverted test case's stamps were rewritten")
	}
	if tc.WindowEnd.Before(captured) || tc.WindowEnd.After(time.Now()) {
		t.Fatalf("its window ends at %v, want when the capture took it (from %v)", tc.WindowEnd, captured)
	}
	if got := invertedMapping(t, maps, tc.Name); len(got) != 1 || got[0] != caused.Name {
		t.Fatalf("mapped %v to the inverted test case, want the mock its request caused", got)
	}
	select {
	case got := <-out:
		if got != caused {
			t.Fatal("the inverted test case's resolve handed on another mock")
		}
	default:
		t.Fatal("the inverted test case's mock was not handed on")
	}
	select {
	case got := <-out:
		t.Fatalf("handed on %v too: the mock requested before its request is not its own", got.Spec.ReqTimestampMock)
	default:
	}
	if n := mgr.InvertedTestCases(); n != 1 {
		t.Fatalf("%d inverted test cases counted, want 1", n)
	}
	if n := mgr.OpenWindows(); n != 0 {
		t.Fatalf("%d windows still open", n)
	}
}

// A test case in order is resolved over its stamps, and carries no window
// end of its own.
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
	if !tc.WindowEnd.IsZero() {
		t.Fatalf("a test case in order carries a window end %v", tc.WindowEnd)
	}
	if got := invertedMapping(t, maps, tc.Name); len(got) != 1 || got[0] != caused.Name {
		t.Fatalf("mapped %v to the test case in order, want only the mock inside its stamps", got)
	}
	if n := mgr.InvertedTestCases(); n != 0 {
		t.Fatalf("%d inverted test cases counted for one in order", n)
	}
}

// A synchronous gRPC call whose response is dated before its request is kept
// as an HTTP request is (Capture), with the mock its request caused.
func TestCaptureGRPCSynchronousKeepsAnInvertedCallWithItsMocks(t *testing.T) {
	origHook := GRPCCaptureHook
	GRPCCaptureHook = nil
	defer func() { GRPCCaptureHook = origHook }()
	req := time.Now().Add(-time.Second)
	res := req.Add(-100 * time.Millisecond)
	mgr, _, maps, caused, _ := invertedManager(t, req)
	w := mgr.OpenWindow(req, nil)
	stream := &pkg.HTTP2Stream{GRPCReq: &models.GrpcReq{Timestamp: req}, GRPCResp: &models.GrpcResp{Timestamp: res}}
	queue := make(chan *models.TestCase, 1)
	CaptureGRPC(syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w), zap.NewNop(), queue, stream, 9090, true, true)
	var tc *models.TestCase
	select {
	case tc = <-queue:
	default:
		t.Fatal("the inverted gRPC call was not recorded")
	}
	if s, e := models.TestCaseWindow(tc); !s.Equal(req) || e.Before(req) {
		t.Fatalf("its window is [%v, %v], want from its request to when it was taken", s, e)
	}
	if got := invertedMapping(t, maps, tc.Name); len(got) != 1 || got[0] != caused.Name {
		t.Fatalf("mapped %v to the inverted call, want the mock its request caused", got)
	}
	if n := mgr.InvertedTestCases(); n != 1 {
		t.Fatalf("%d inverted test cases counted, want 1", n)
	}
}

// The window of an inverted test case ends when its ingress had the exchange
// complete (WithArrival), not when its capture ran: a call made after that,
// while the capture waited, is not its own.
func TestCaptureEndsAnInvertedWindowWhereItsIngressHadItComplete(t *testing.T) {
	req := time.Now().Add(-time.Second)
	res := req.Add(-100 * time.Millisecond)
	mgr, _, maps, caused, _ := invertedManager(t, req)
	arrived := req.Add(20 * time.Millisecond)
	later := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: req.Add(30 * time.Millisecond)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
	mgr.AddMock(later)
	w := mgr.OpenWindow(req, nil)
	tcChan := make(chan *models.TestCase, 1)
	ctx := WithArrival(syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w), arrived)
	Capture(ctx, zap.NewNop(), tcChan, httptest.NewRequest(http.MethodGet, "http://app.local/inverted/2", nil), windowResp(), req, res, models.IncomingOptions{}, true, true, 8080)
	var tc *models.TestCase
	select {
	case tc = <-tcChan:
	default:
		t.Fatal("the inverted test case was not recorded")
	}
	if !tc.WindowEnd.Equal(arrived) {
		t.Fatalf("its window ends at %v, want where its ingress had it complete, %v", tc.WindowEnd, arrived)
	}
	if got := invertedMapping(t, maps, tc.Name); len(got) != 1 || got[0] != caused.Name {
		t.Fatalf("mapped %v to the inverted test case, want only the mock requested before it was complete", got)
	}
}

// Without --sync nothing is resolved: an inverted test case is handed on with
// its window's end, and counted once it is.
func TestCaptureWithoutSyncHandsOnAnInvertedTestCaseWithItsWindowEnd(t *testing.T) {
	req := time.Now().Add(-time.Second)
	mgr, _, _, _, _ := invertedManager(t, req)
	w := mgr.OpenWindow(req, nil)
	defer w.Close()
	tcChan := make(chan *models.TestCase, 1)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	Capture(ctx, zap.NewNop(), tcChan, httptest.NewRequest(http.MethodGet, "http://app.local/inverted/3", nil), windowResp(), req, req.Add(-time.Millisecond), models.IncomingOptions{}, false, false, 8080)
	var tc *models.TestCase
	select {
	case tc = <-tcChan:
	default:
		t.Fatal("the inverted test case was not handed on")
	}
	if s, e := models.TestCaseWindow(tc); !s.Equal(req) || !e.After(req) {
		t.Fatalf("its window is [%v, %v], want from its request to when it was had complete", s, e)
	}
	if n := mgr.InvertedTestCases(); n != 1 {
		t.Fatalf("%d inverted test cases counted, want 1", n)
	}
}

// An inverted request left out (its window given up) is not counted: it is no
// kept test case.
func TestCaptureSynchronousDoesNotCountALeftOutInvertedRequest(t *testing.T) {
	mgr, w := givenUpWindow(t)
	tcChan := make(chan *models.TestCase, 1)
	req := time.Now().Add(-time.Second)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	Capture(ctx, zap.NewNop(), tcChan, httptest.NewRequest(http.MethodGet, "http://app.local/inverted/4", nil), windowResp(), req, req.Add(-time.Millisecond), models.IncomingOptions{}, true, false, 8080)
	w.Close()
	select {
	case <-tcChan:
		t.Fatal("a given-up request was recorded")
	default:
	}
	if n := mgr.InvertedTestCases(); n != 0 {
		t.Fatalf("%d inverted test cases counted for one left out", n)
	}
}

// The synchronous ingress yields an inverted test case's window where it
// ends, its arrival (Window.Yield, models.InvertedWindowEnd), and the capture
// resolves it over that window: it keeps the mock its request caused, though
// a longer request in flight since before it has not yielded. Yielded at its
// response's stamp, before its request, its resolve left the mock to that
// longer request.
func TestCaptureSynchronousKeepsTheMocksOfAnInvertedWindowThatYielded(t *testing.T) {
	req := time.Now().Add(-time.Second)
	res := req.Add(-100 * time.Millisecond)
	mgr, _, maps, caused, _ := invertedManager(t, req)
	// A longer request in flight since before it (a stream), unyielded.
	longer := mgr.OpenWindow(req.Add(-time.Second), nil)
	defer longer.Close()
	w := mgr.OpenWindow(req, nil)
	captured := time.Now()
	// As the synchronous ingress yields it: where its window ends.
	w.Yield(models.InvertedWindowEnd(req, res, captured))
	tcChan := make(chan *models.TestCase, 1)
	ctx := WithArrival(syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w), captured)
	Capture(ctx, zap.NewNop(), tcChan, httptest.NewRequest(http.MethodGet, "http://app.local/inverted/5", nil), windowResp(), req, res, models.IncomingOptions{}, true, true, 8080)
	var tc *models.TestCase
	select {
	case tc = <-tcChan:
	default:
		t.Fatal("the inverted test case was not recorded")
	}
	if got := invertedMapping(t, maps, tc.Name); len(got) != 1 || got[0] != caused.Name {
		t.Fatalf("mapped %v to the inverted test case, want the mock its request caused", got)
	}
	if n := mgr.InvertedTestCases(); n != 1 {
		t.Fatalf("%d inverted test cases counted, want 1", n)
	}
}
