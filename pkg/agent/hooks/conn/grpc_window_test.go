package conn

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// grpcCall is a complete call on stream 1, its request at start and its
// answer at end.
func grpcCall(start, end time.Time) *pkg.HTTP2Stream {
	return &pkg.HTTP2Stream{ID: 1,
		GRPCReq:  &models.GrpcReq{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/svc.Echo/Ping"}}, Timestamp: start},
		GRPCResp: &models.GrpcResp{Timestamp: end}}
}

// perTestMock is a per-test egress mock requested at at.
func perTestMock(at time.Time) *models.Mock {
	return &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
}

// heldGRPCManager is a manager past its start-up test cases that buffers (a
// synchronous recording after its first request), with an output and a
// mapping stream, a window opened a second ago, and a per-test mock of it
// held.
func heldGRPCManager(t *testing.T) (*syncMock.SyncMockManager, *syncMock.Window, chan *models.Mock, chan models.TestMockMapping, time.Time, *models.Mock) {
	t.Helper()
	mgr := syncMock.New(zap.NewNop())
	out := make(chan *models.Mock, 64)
	mgr.SetOutputChannel(out)
	maps := make(chan models.TestMockMapping, 16)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr.SetMappingChannel(ctx, maps)
	mgr.SetFirstRequestSignaled()
	start := time.Now().Add(-time.Second)
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		mgr.ResolveRange(start.Add(-time.Minute), start.Add(-time.Minute), "", true, false)
	}
	w := mgr.OpenWindow(start, nil)
	t.Cleanup(w.Close)
	mock := perTestMock(start.Add(time.Millisecond))
	mgr.AddMock(mock)
	return mgr, w, out, maps, start, mock
}

// mappedTo reports the mock ids mapped to name on maps, and whether mock
// went out on out.
func mappedTo(maps chan models.TestMockMapping, out chan *models.Mock, name string, mock *models.Mock) (ids []string, emitted bool) {
	for len(maps) > 0 {
		if mp := <-maps; mp.TestName == name {
			ids = append(ids, mp.MockIDs...)
		}
	}
	for len(out) > 0 {
		if <-out == mock {
			emitted = true
		}
	}
	return ids, emitted
}

// A kept call with --sync is handed on under a name of the manager's, and
// then its window's resolve takes what was held for it, maps it to that name
// and ends the window: no reaper can drop a mock of a call in flight, and none
// is lost while its test case waits for the queue.
func TestCaptureGRPCSynchronousRecordsAKeptCallAndEndsItsWindow(t *testing.T) {
	mgr, w, out, maps, start, mock := heldGRPCManager(t)
	tcChan := make(chan *models.TestCase, 1)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	CaptureGRPC(ctx, zap.NewNop(), tcChan, grpcCall(start, time.Now()), 50051, true, true)
	var tc *models.TestCase
	select {
	case tc = <-tcChan:
	default:
		t.Fatal("the kept call was not handed on")
	}
	if !strings.HasPrefix(tc.Name, "test-") {
		t.Fatalf("the test case is named %q, not by the manager", tc.Name)
	}
	if n := mgr.OpenWindows(); n != 0 {
		t.Fatalf("%d windows open after the kept resolve: it did not end the call's", n)
	}
	ids, emitted := mappedTo(maps, out, tc.Name, mock)
	if !emitted || len(ids) != 1 {
		t.Fatalf("the call's held mock: emitted %v, mapped %v to %s; want it handed on as the call's", emitted, ids, tc.Name)
	}
}

// A call with --sync whose window the manager gave up while it was in flight
// is left out, counted, and nothing is resolved to it.
func TestCaptureGRPCSynchronousLeavesOutAGivenUpCall(t *testing.T) {
	mgr, w := givenUpWindow(t)
	defer w.Close()
	tcChan := make(chan *models.TestCase, 1)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	CaptureGRPC(ctx, zap.NewNop(), tcChan, grpcCall(w.Start(), time.Now()), 50051, true, false)
	select {
	case tc := <-tcChan:
		t.Fatalf("%s was recorded though its call's window was given up", tc.Name)
	default:
	}
	if got := mgr.KeptRequestsLeftOut(); got != 1 {
		t.Fatalf("left out %d calls, want the given-up one counted", got)
	}
}

// A call with --sync whose window is given up while its test case waits for
// the recording's queue went out without mocks it may own: it is revoked.
func TestCaptureGRPCSynchronousRevokesATestCaseWhoseWindowWasGivenUpWhileItWaitedForTheQueue(t *testing.T) {
	mgr := syncMock.New(zap.NewNop())
	out := make(chan *models.Mock, 64)
	mgr.SetOutputChannel(out)
	mgr.SetRevokeCapable(true)
	mgr.SetFirstRequestSignaled()
	start := time.Now().Add(-time.Minute)
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		mgr.ResolveRange(start.Add(-time.Second), start.Add(-time.Second), "", true, false)
	}
	w := mgr.OpenWindow(start, nil)
	defer w.Close()
	body := strings.Repeat("b", int(syncMock.MaxHeldBytes/16))
	for i := 0; i <= 16; i++ {
		mgr.AddMock(&models.Mock{Kind: models.HTTP,
			Spec:         models.MockSpec{ReqTimestampMock: start.Add(time.Duration(i) * time.Microsecond), HTTPResp: &models.HTTPResp{Body: body}},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}})
	}
	tcChan := make(chan *models.TestCase, 1)
	tcChan <- &models.TestCase{Name: "earlier"}
	ctx := &handOnSeen{Context: syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w), seen: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		CaptureGRPC(ctx, zap.NewNop(), tcChan, grpcCall(start, time.Now()), 50051, true, false)
	}()
	<-ctx.seen
	n := time.Now()
	mgr.ResolveRange(n, n, "", true, false) // past the stale horizon: held, over the budget
	<-tcChan
	tc := <-tcChan
	<-done
	if !mgr.WasMockDroppedForTC(tc.Name) {
		t.Fatalf("%s went out after its window was given up and is not left out", tc.Name)
	}
	mgr.FlushOwnedWindows()
	revoked := false
	for len(out) > 0 {
		if m := <-out; m.Kind == models.RevokedTests {
			revoked = revoked || strings.Contains(m.Spec.Metadata["revoked_tests"], tc.Name)
		}
	}
	if !revoked {
		t.Fatalf("no revoke of %s reached the mock stream", tc.Name)
	}
}

// A call with --sync whose test case its ingress's stop beats to the queue
// resolves nothing: its mocks are no test case's the recording gets.
func TestCaptureGRPCSynchronousResolvesNothingToATestCaseItCouldNotHandOn(t *testing.T) {
	mgr, w, out, maps, start, mock := heldGRPCManager(t)
	ingress, stop := context.WithCancel(syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w))
	stop()
	CaptureGRPC(ingress, zap.NewNop(), make(chan *models.TestCase), grpcCall(start, time.Now()), 50051, true, true)
	if ids, emitted := mappedTo(maps, out, "test-1", mock); emitted || len(ids) > 0 {
		t.Fatalf("a call never handed on had its mock emitted (%v) or mapped (%v)", emitted, ids)
	}
}

// A duplicate ends its own window before its prune, and is not handed on.
func TestCaptureGRPCSynchronousDuplicateEndsItsWindowBeforeItsPrune(t *testing.T) {
	mgr, w, out, maps, start, mock := heldGRPCManager(t)
	prev := GRPCCaptureHook
	GRPCCaptureHook = func(*models.GrpcReq, *models.GrpcResp) (bool, string) { return true, "" }
	defer func() { GRPCCaptureHook = prev }()
	tcChan := make(chan *models.TestCase, 1)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	CaptureGRPC(ctx, zap.NewNop(), tcChan, grpcCall(start, time.Now()), 50051, true, true)
	if len(tcChan) > 0 {
		t.Fatal("a duplicate was handed on")
	}
	if n := mgr.OpenWindows(); n != 0 {
		t.Fatalf("%d windows open after a duplicate's capture: it did not end its own", n)
	}
	if ids, emitted := mappedTo(maps, out, "test-1", mock); emitted || len(ids) > 0 {
		t.Fatalf("a duplicate's mock was handed on (%v) or mapped (%v)", emitted, ids)
	}
}

// Without --sync a duplicate prunes nothing: a mock it made stays buffered
// for whichever resolve takes it.
func TestCaptureGRPCWithoutSyncDuplicatePrunesNothing(t *testing.T) {
	mgr, w, out, maps, start, mock := heldGRPCManager(t)
	prev := GRPCCaptureHook
	GRPCCaptureHook = func(*models.GrpcReq, *models.GrpcResp) (bool, string) { return true, "" }
	defer func() { GRPCCaptureHook = prev }()
	end := time.Now()
	CaptureGRPC(syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w), zap.NewNop(), make(chan *models.TestCase, 1), grpcCall(start, end), 50051, false, true)
	mgr.ResolveRange(start, end, "test-next", true, true)
	if ids, emitted := mappedTo(maps, out, "test-next", mock); !emitted || len(ids) != 1 {
		t.Fatalf("after a duplicate without --sync, the mock in its span: emitted %v, mapped %v; want it left for the next resolve", emitted, ids)
	}
}

// A call over the size cap is not recorded, and nothing is resolved to it.
func TestCaptureGRPCSynchronousResolvesNothingToACallOverTheSizeCap(t *testing.T) {
	mgr, w, out, maps, start, mock := heldGRPCManager(t)
	call := grpcCall(start, time.Now())
	call.GRPCReq.Body.DecodedData = strings.Repeat("x", MaxTestCaseSize+1)
	tcChan := make(chan *models.TestCase, 1)
	CaptureGRPC(syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w), zap.NewNop(), tcChan, call, 50051, true, true)
	if len(tcChan) > 0 {
		t.Fatal("a call over the size cap was handed on")
	}
	if ids, emitted := mappedTo(maps, out, "test-1", mock); emitted || len(ids) > 0 {
		t.Fatalf("a call never recorded had its mock emitted (%v) or mapped (%v)", emitted, ids)
	}
}

// Without --sync a call in flight across its recording's stop is left out;
// one whose window the hold's budget gave up is still recorded (nothing was
// to be resolved to it), and a handed-on call's span is noted under the
// proven switch, but only once it is handed on.
func TestCaptureGRPCWithoutSyncDecidesItsWindow(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(make(chan *models.Mock, 16))
	reqAt := time.Now().Add(-time.Minute)
	w := mgr.OpenWindow(reqAt, nil)
	defer w.Close()
	mgr.NoteStop(time.Now())
	mgr.EndStoppedRecording()
	tcChan := make(chan *models.TestCase, 1)
	CaptureGRPC(syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w), zap.NewNop(), tcChan, grpcCall(reqAt, time.Now()), 50051, false, false)
	if len(tcChan) > 0 {
		t.Fatal("a call in flight across its recording's stop was recorded")
	}

	_, budget := givenUpWindow(t)
	defer budget.Close()
	CaptureGRPC(syncMock.WithWindow(context.Background(), budget), zap.NewNop(), tcChan, grpcCall(reqAt, time.Now()), 50051, false, false)
	if len(tcChan) != 1 {
		t.Fatal("without --sync a call the hold's budget gave up is no longer recorded")
	}
	if tc := <-tcChan; tc.Name != "" {
		t.Fatalf("without --sync the call's test case is named %q by the manager; the recorder names it", tc.Name)
	}

	fresh := syncMock.New(zap.NewNop())
	start := time.Now().Add(-time.Second)
	end := start.Add(10 * time.Millisecond)
	notOn := fresh.OpenWindow(start, nil)
	ingress, stop := context.WithCancel(syncMock.WithWindow(syncMock.NewContext(context.Background(), fresh), notOn))
	stop()
	CaptureGRPC(ingress, zap.NewNop(), make(chan *models.TestCase), grpcCall(start, end), 50051, false, false)
	notOn.Close()
	if span, ok := fresh.TestSpanAt(start.Add(time.Millisecond)); ok {
		t.Fatalf("a call never handed on noted a test span %v", span)
	}
	on := fresh.OpenWindow(start, nil)
	CaptureGRPC(syncMock.WithWindow(syncMock.NewContext(context.Background(), fresh), on), zap.NewNop(), tcChan, grpcCall(start, end), 50051, false, false)
	on.Close()
	if span, ok := fresh.TestSpanAt(start.Add(time.Millisecond)); !ok || !span.End.Equal(end) {
		t.Fatalf("a handed-on call's span: %v %v, want it noted to %v", span, ok, end)
	}
}
