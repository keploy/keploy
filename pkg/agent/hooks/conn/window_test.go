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
// the manager its window is on (the ctx's), through ResolveKept: it takes
// what was held for the request and ends the window, before it hands the
// mocks on (a window open while its mocks are handed on pins the hold;
// syncMock's TestResolveKeptEndsTheWindowBeforeItsSends holds ResolveKept to
// that order). Here the request's mock reaches that manager's channel, and
// the window is ended though nothing else closed it.
//
// It used to look from OnDrop, on a channel never read: the send waited its
// 200 ms budget and dropped the mock inside the call. A send no longer waits
// or decides anything in the call when the channel is full: it queues the
// mock for a drainer, so that look saw no send at all.
func TestCaptureSynchronousRecordsAKeptRequestAndEndsItsWindow(t *testing.T) {
	mgr := syncMock.New(zap.NewNop())
	out := make(chan *models.Mock, 1)
	mgr.SetOutputChannel(out)
	mgr.SetFirstRequestSignaled()
	start := time.Now().Add(-time.Second)
	w := mgr.OpenWindow(start, nil)
	mock := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: start.Add(time.Millisecond)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
	mgr.AddMock(mock)
	tcChan := make(chan *models.TestCase, 1)
	req := httptest.NewRequest(http.MethodGet, "http://app.local/kept/1", nil)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	Capture(ctx, zap.NewNop(), tcChan, req, windowResp(), start, time.Now(), models.IncomingOptions{}, true, false, 8080)
	select {
	case <-tcChan:
	default:
		t.Fatal("a kept request was not recorded")
	}
	select {
	case got := <-out:
		if got != mock {
			t.Fatal("the kept request's manager handed on another mock")
		}
	default:
		t.Fatal("the kept request's mock was not sent on its window's manager")
	}
	if n := mgr.OpenWindows(); n != 0 {
		t.Fatalf("%d windows still open: the kept request's window was not ended", n)
	}
}

// A capture without --sync, which resolves nothing, still leaves out a request
// that was in flight across its recording's stop once that recording ended
// (syncMock.LostToStop): the recording it began in ended without it, and it is
// no request of the next one, which would record it with the next recording's
// leftovers and without the calls it made before the stop. A request the
// manager gave up for any other cause is recorded without --sync, as before.
func TestCaptureLeavesOutARequestInFlightAcrossItsRecordingsStop(t *testing.T) {
	mgr := syncMock.New(zap.NewNop())
	mgr.SetOutputChannel(make(chan *models.Mock, 16))
	reqAt := time.Now().Add(-time.Minute)
	w := mgr.OpenWindow(reqAt, nil)
	defer w.Close()
	mgr.NoteStop(time.Now())  // the recording's stop
	mgr.EndStoppedRecording() // and its end
	tcChan := make(chan *models.TestCase, 1)
	req := httptest.NewRequest(http.MethodGet, "http://app.local/poll", nil)
	ctx := syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w)
	Capture(ctx, zap.NewNop(), tcChan, req, windowResp(), reqAt, time.Now(), models.IncomingOptions{}, false, false, 8080)
	select {
	case tc := <-tcChan:
		t.Fatalf("a request in flight across its recording's stop was recorded: %s", tc.HTTPReq.URL)
	default:
	}

	_, budget := givenUpWindow(t)
	defer budget.Close()
	ctx = syncMock.WithWindow(context.Background(), budget)
	Capture(ctx, zap.NewNop(), tcChan, httptest.NewRequest(http.MethodGet, "http://app.local/stream/1", nil), windowResp(), reqAt, time.Now(), models.IncomingOptions{}, false, false, 8080)
	select {
	case <-tcChan:
	default:
		t.Fatal("without --sync a request the hold's budget gave up is no longer recorded")
	}
}
