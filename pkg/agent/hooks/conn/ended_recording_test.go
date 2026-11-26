package conn

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// watchedBody is a body that says whether it was read and closed.
type watchedBody struct {
	io.Reader
	read, closed bool
}

func (b *watchedBody) Read(p []byte) (int, error) { b.read = true; return b.Reader.Read(p) }
func (b *watchedBody) Close() error               { b.closed = true; return nil }

// endedOn is a context whose capture's manager has ended its recording (its
// client ended its streams), as the agent's is once a sidecar's recording
// stopped while its app serves on.
func endedOn(t *testing.T) (context.Context, *syncMock.SyncMockManager, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)
	m := syncMock.New(nil)
	m.SetLogger(zap.New(core))
	m.SetFirstRequestSignaled()
	m.EndRecording()
	return syncMock.NewContext(context.Background(), m), m, logs
}

// notCaptured is the count of requests the agent said it left uncaptured
// after m's recording ended.
func notCaptured(t *testing.T, m *syncMock.SyncMockManager, logs *observer.ObservedLogs) any {
	t.Helper()
	m.SayNotCaptured()
	said := logs.FilterMessageSnippet("no recording took the application's traffic").All()
	if len(said) != 1 {
		t.Fatalf("what the agent left uncaptured was said %d times, want once", len(said))
	}
	return said[0].ContextMap()["requests_not_captured"]
}

// An exchange the app serves after its recording ended is no test case:
// nothing of it is read or handed on, its bodies are closed so the proxied
// connection's readers do not leak, and no test name is spent on it. With
// --sync too, where the name is drawn before the test case is handed on.
func TestAnExchangeServedAfterTheRecordingEndedIsNotCaptured(t *testing.T) {
	for _, sync := range []bool{false, true} {
		ctx, m, logs := endedOn(t)
		reqBody := &watchedBody{Reader: strings.NewReader(`{"k":1}`)}
		respBody := &watchedBody{Reader: strings.NewReader(`{"ok":true}`)}
		req := httptest.NewRequest(http.MethodPost, "http://app:8080/q", reqBody)
		resp := &http.Response{StatusCode: http.StatusOK, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}, Body: respBody}
		tcs := make(chan *models.TestCase, 1)
		at := time.Now()
		Capture(ctx, zap.NewNop(), tcs, req, resp, at, at.Add(time.Millisecond), models.IncomingOptions{}, sync, true, 8080)
		if len(tcs) != 0 {
			t.Fatalf("sync=%v: a request served after the recording ended was handed on", sync)
		}
		if reqBody.read || respBody.read {
			t.Fatalf("sync=%v: the exchange was read (request %v, response %v)", sync, reqBody.read, respBody.read)
		}
		if !reqBody.closed || !respBody.closed {
			t.Fatalf("sync=%v: a body was left open (request %v, response %v)", sync, reqBody.closed, respBody.closed)
		}
		if id := m.NextTestID(); id != 1 {
			t.Fatalf("sync=%v: the uncaptured request spent test names: the next is test-%d", sync, id)
		}
		if n := notCaptured(t, m, logs); n != uint64(1) {
			t.Fatalf("sync=%v: the agent says it left %v requests uncaptured, want the 1 it served", sync, n)
		}
	}
}

// A gRPC call the app serves after its recording ended is no test case
// either: it is not judged a duplicate or not, and spends no test name.
func TestAGRPCCallServedAfterTheRecordingEndedIsNotCaptured(t *testing.T) {
	orig := GRPCCaptureHook
	defer func() { GRPCCaptureHook = orig }()
	judged := 0
	GRPCCaptureHook = func(*models.GrpcReq, *models.GrpcResp) (bool, string) { judged++; return false, "" }
	ctx, m, logs := endedOn(t)
	tcs := make(chan *models.TestCase, 1)
	CaptureGRPC(ctx, zap.NewNop(), tcs, &pkg.HTTP2Stream{GRPCReq: &models.GrpcReq{}, GRPCResp: &models.GrpcResp{}}, 9090, true, true, models.IncomingOptions{})
	if len(tcs) != 0 || judged != 0 {
		t.Fatalf("a call served after the recording ended was handed on (%d) or judged (%d times)", len(tcs), judged)
	}
	if id := m.NextTestID(); id != 1 {
		t.Fatalf("the uncaptured call spent test names: the next is test-%d", id)
	}
	if n := notCaptured(t, m, logs); n != uint64(1) {
		t.Fatalf("the agent says it left %v requests uncaptured, want the 1 call it served", n)
	}
}

// A test case built as its recording ended (its capture began before the
// end) is not handed on: no stream of the ended recording reads the queue
// past its end marker.
func TestATestCaseBuiltAsItsRecordingEndedIsNotHandedOn(t *testing.T) {
	ctx, m, logs := endedOn(t)
	tcs := make(chan *models.TestCase, 1)
	if HandOn(ctx, tcs, &models.TestCase{Name: "test-1"}) || len(tcs) != 0 {
		t.Fatal("a test case was handed on after its recording ended")
	}
	if n := notCaptured(t, m, logs); n != uint64(1) {
		t.Fatalf("the agent says it left %v requests uncaptured, want the 1 not handed on", n)
	}
	live := syncMock.NewContext(context.Background(), syncMock.New(nil))
	if !HandOn(live, tcs, &models.TestCase{Name: "test-1"}) || len(tcs) != 1 {
		t.Fatal("a recording's test case was not handed on")
	}
}
