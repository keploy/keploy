package conn

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// handOnSeen is a capture's ctx that says when HandOn waits on it: HandOn is
// the only part of Capture that waits on its ctx's Done.
type handOnSeen struct {
	context.Context
	once sync.Once
	seen chan struct{}
}

func (c *handOnSeen) Done() <-chan struct{} {
	c.once.Do(func() { close(c.seen) })
	return c.Context.Done()
}

// A synchronous capture hands its test case on before it claims its
// request's window, and resolves the window's mocks to it (ResolveKept) only
// once it is in the recording's queue: a claimed window is never given up, so
// one claimed while its test case waited on a full queue (a slow stream) kept
// every mock captured meanwhile, past the hold's bound. Here the hold passes
// its bound while the test case waits: the window is given up, the hold lets
// go of its mocks, and the test case, which went out without them, is left
// out after all (syncMock.RevokeTestCase): the stream suppresses it, a CLI
// that negotiated the revoke deletes it, and the loss is counted
// (KeptRequestsLeftOut). Claimed before the wait, the window kept the hold
// over its bound, and its test case was recorded.
func TestCaptureSynchronousLeavesOutATestCaseWhoseWindowWasGivenUpWhileItWaitedForTheQueue(t *testing.T) {
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
	defer w.Close() // as the ingress does once the hook has returned
	// Seventeen mocks of a sixteenth of the hold's budget each, all made
	// inside the request's span.
	body := strings.Repeat("b", int(syncMock.MaxHeldBytes/16))
	for i := 0; i <= 16; i++ {
		mgr.AddMock(&models.Mock{Kind: models.HTTP,
			Spec:         models.MockSpec{ReqTimestampMock: start.Add(time.Duration(i) * time.Microsecond), HTTPResp: &models.HTTPResp{Body: body}},
			TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}})
	}

	// The recording's queue is full: the stream has not taken the last test
	// case yet.
	tcChan := make(chan *models.TestCase, 1)
	tcChan <- &models.TestCase{Name: "earlier"}
	ctx := &handOnSeen{Context: syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w), seen: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Capture(ctx, zap.NewNop(), tcChan, httptest.NewRequest(http.MethodGet, "http://app.local/kept/1", nil), windowResp(), start, time.Now(), models.IncomingOptions{}, true, false, 8080)
	}()
	<-ctx.seen // the test case waits for the queue

	// Traffic captured meanwhile: past the stale horizon the mocks are held
	// for the request in flight, over the hold's budget.
	n := time.Now()
	mgr.ResolveRange(n, n, "", true, false)

	<-tcChan // the stream takes the earlier test case
	var tc *models.TestCase
	select {
	case tc = <-tcChan:
	case <-done:
		select {
		case tc = <-tcChan:
		default:
			t.Fatal("the test case was left out before it was handed on: the window was given up before the capture waited for the queue")
		}
	}
	<-done
	if !mgr.WasMockDroppedForTC(tc.Name) {
		t.Fatalf("%s went out after its window was given up and is not left out: the stream sends it without the mocks the hold let go", tc.Name)
	}
	if got := mgr.KeptRequestsLeftOut(); got != 1 {
		t.Fatalf("left out %d kept requests, want the one whose window was given up counted", got)
	}
	mgr.FlushOwnedWindows()
	revoked := false
	for len(out) > 0 {
		m := <-out
		switch {
		case m.Kind == models.RevokedTests:
			revoked = revoked || strings.Contains(m.Spec.Metadata["revoked_tests"], tc.Name)
		case m.Spec.HTTPResp != nil && m.Spec.HTTPResp.Body == body:
			t.Fatalf("a mock of %s's request was handed on after its window was given up", tc.Name)
		}
	}
	if !revoked {
		t.Fatalf("no revoke of %s reached the mock stream: a CLI that saved it keeps it without its mocks", tc.Name)
	}
}

// A synchronous capture whose test case its ingress, stopping, could not hand
// on (HandOn counted it) resolves nothing to it: the recording never gets the
// test case, so its mocks and their mapping would name a test that is not
// there. The window is left to the ingress to end, as a left-out request's.
func TestCaptureSynchronousResolvesNothingToATestCaseItCouldNotHandOn(t *testing.T) {
	mgr := syncMock.New(zap.NewNop())
	out := make(chan *models.Mock, 4)
	mgr.SetOutputChannel(out)
	mgr.SetFirstRequestSignaled()
	start := time.Now().Add(-time.Second)
	w := mgr.OpenWindow(start, nil)
	mgr.AddMock(&models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: start.Add(time.Millisecond)},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}})
	stopped, stop := context.WithCancel(syncMock.WithWindow(syncMock.NewContext(context.Background(), mgr), w))
	stop() // the ingress is stopping
	Capture(stopped, zap.NewNop(), make(chan *models.TestCase), httptest.NewRequest(http.MethodGet, "http://app.local/kept/1", nil), windowResp(), start, time.Now(), models.IncomingOptions{}, true, true, 8080)
	if got := mgr.NotHandedOn(); got != 1 {
		t.Fatalf("counted %d test cases not handed on, want 1", got)
	}
	if len(out) != 0 {
		t.Fatalf("%d mocks were resolved to a test case the recording never got", len(out))
	}
	if got := mgr.OpenWindows(); got != 1 {
		t.Fatalf("%d windows open after the capture, want its request's left for the ingress to end", got)
	}
	w.Close()
}
