package conn

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func windowResp() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}
}

// givenUpWindow returns a window its manager has given up: it was the oldest
// open window when the hold went past syncMock.MaxHeldBytes.
func givenUpWindow(t *testing.T) (*syncMock.SyncMockManager, *syncMock.Window) {
	t.Helper()
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(make(chan *models.Mock, 16))
	mgr.SetFirstRequestSignaled()
	start := time.Now().Add(-time.Minute)
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		mgr.ResolveRange(start.Add(-time.Second), start.Add(-time.Second), "", true, false)
	}
	w := mgr.OpenWindow(start, nil)
	// Seventeen mocks of a sixteenth of the hold's budget each (the body is
	// shared; each mock is sized with it).
	body := strings.Repeat("b", int(syncMock.MaxHeldBytes/16))
	for i := 0; i <= 16; i++ {
		mgr.AddMock(&models.Mock{Kind: models.HTTP,
			Spec:         models.MockSpec{ReqTimestampMock: start.Add(time.Duration(i) * time.Microsecond), HTTPResp: &models.HTTPResp{Body: body}},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}})
	}
	n := time.Now()
	mgr.ResolveRange(n, n, "", true, false) // past the stale horizon: held, over the budget
	return mgr, w
}

// A synchronous capture whose request's window the manager gave up leaves the
// test case out: mocks it may own were let go, and it must not be recorded
// without them. Its verdict prunes nothing: the request ran for long (that is
// why it was given up), and a prune over its whole span would drop the mocks
// of every request that ran beside it and is not decided yet, such as a gRPC
// call, which opens no window. What nothing claims goes at the stale cutoff,
// as ever.
func TestCaptureSynchronousLeavesOutAGivenUpRequest(t *testing.T) {
	mgr, w := givenUpWindow(t)
	out := make(chan *models.Mock, 16)
	mgr.SetOutputChannel(out)
	reqAt := time.Now().Add(-time.Second)
	// Made inside the left-out request's span by a call still in flight: its
	// own resolve comes after the left-out request's verdict.
	other := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: reqAt.Add(time.Millisecond)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
	mgr.AddMock(other)
	tcChan := make(chan *models.TestCase, 1)
	req := httptest.NewRequest(http.MethodGet, "http://app.local/stream/1", nil)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	Capture(ctx, zap.NewNop(), tcChan, req, windowResp(), reqAt.Add(-time.Minute), time.Now(), models.IncomingOptions{}, true, false, 8080)
	select {
	case tc := <-tcChan:
		t.Fatalf("a given-up request was recorded: %s", tc.HTTPReq.URL)
	default:
	}
	w.Close() // as the ingress does once the hook has returned
	if n := mgr.OpenWindows(); n != 0 {
		t.Fatalf("%d windows still open", n)
	}
	// The other call is decided kept: its mock is still there for it.
	mgr.ResolveRange(reqAt, reqAt.Add(10*time.Millisecond), "grpc-1", true, false)
	select {
	case got := <-out:
		if got != other {
			t.Fatalf("the call beside the left-out request was handed another mock")
		}
	default:
		t.Fatal("the left-out request's verdict dropped the mock of a call that ran beside it")
	}
	if _, _, _, n := mgr.GetDropStats(); n != 0 {
		t.Fatalf("%d mocks still buffered", n)
	}
}

// A synchronous capture whose request's window was not given up records it on
// the manager its window is on (the ctx's), and ends the window before handing
// the mocks on: a send can wait on a full output channel, and a window open
// meanwhile pins the hold. OnDrop runs inside those sends.
func TestCaptureSynchronousRecordsAKeptRequestAndEndsItsWindow(t *testing.T) {
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(make(chan *models.Mock)) // never read: every send waits out its budget
	mgr.SetFirstRequestSignaled()
	start := time.Now().Add(-time.Second)
	w := mgr.OpenWindow(start, nil)
	mgr.AddMock(&models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: start.Add(time.Millisecond)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}})
	openDuringSend := -1
	mgr.OnDrop(func(*models.Mock) {
		if openDuringSend < 0 {
			openDuringSend = mgr.OpenWindows()
		}
	})
	tcChan := make(chan *models.TestCase, 1)
	req := httptest.NewRequest(http.MethodGet, "http://app.local/kept/1", nil)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	Capture(ctx, zap.NewNop(), tcChan, req, windowResp(), start, time.Now(), models.IncomingOptions{}, true, false, 8080)
	select {
	case <-tcChan:
	default:
		t.Fatal("a kept request was not recorded")
	}
	if openDuringSend != 0 {
		t.Fatalf("%d windows open while the kept request's mocks were sent (-1: none was sent on its manager)", openDuringSend)
	}
	if n := mgr.OpenWindows(); n != 0 {
		t.Fatalf("%d windows still open", n)
	}
}
