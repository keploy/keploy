package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestRecordV2_AHangAbortLeavesOutTheAnswerItCut: the supervisor retires a
// parser that hangs with part of a close-delimited answer in hand. Its abort
// closes the parser's streams, so the recorder reads fakeconn.ErrClosed where
// the rest of the body would be, and builds a mock from what it holds. The
// supervisor cancels the parser's context only after the abort returns, and
// this test holds the abort until the parser has returned, as a supervisor
// goroutine that loses the CPU between the two does. The answer was cut by the
// abort, not ended by the server: it is left out, and said so, never recorded
// as a whole response.
func TestRecordV2_AHangAbortLeavesOutTheAnswerItCut(t *testing.T) {
	supervisor.ResetLeftOutWarningsForTest()
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	h := &HTTP{Logger: logger}
	sess, sendReq, _, sendResp, _, _ := newTestSession(t)
	sess.Logger = logger
	spans := &syncMock.Spans{}
	sess.Orphans = spans
	mgr := syncMock.New(nil)
	recorded := make(chan *models.Mock, 4)
	mgr.SetOutputChannel(recorded)
	sess.Mgr = mgr
	sess.RouteMocksViaSyncMock = true

	reqAt := time.Unix(1_700_000_000, 0)
	respAt := reqAt.Add(5 * time.Millisecond)
	sendReq([]byte("GET /report HTTP/1.1\r\nHost: ex.com\r\n\r\n"), reqAt, reqAt)
	// No Content-Length and not chunked: the body runs to the connection's
	// close, which has not come.
	sendResp([]byte("HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Type: text/plain\r\n\r\nthe first half of the rep"), respAt, respAt)

	sv := supervisor.New(supervisor.Config{Logger: logger, HangBudget: 50 * time.Millisecond})
	parserReturned := make(chan struct{})
	aborted := make(chan struct{})
	sv.SessionOnAbort = func() {
		close(aborted)
		_ = sess.ClientStream.Close()
		_ = sess.DestStream.Close()
		// The supervisor's goroutine is held between the close and the
		// parser's cancel until the parser has done with what it read.
		<-parserReturned
	}
	// The parser has an exchange in hand: the watchdog may fire once it
	// makes no progress.
	sv.MarkPendingWork()
	var parserErr error
	res := sv.Run(context.Background(), func(ctx context.Context, s *supervisor.Session) error {
		defer close(parserReturned)
		parserErr = h.recordV2(ctx, s)
		return parserErr
	}, sess)
	<-parserReturned

	select {
	case <-aborted:
	default:
		t.Fatalf("the supervisor did not abort the parser (status %s)", res.Status)
	}
	if res.Status != supervisor.StatusHung {
		t.Fatalf("status = %s, want hung", res.Status)
	}
	select {
	case m := <-recorded:
		body := ""
		if m.Spec.HTTPResp != nil {
			body = m.Spec.HTTPResp.Body
		}
		t.Fatalf("the answer the abort cut was recorded as a whole response (body %q, fault %v)", body, m.Spec.Fault)
	default:
	}
	if n := mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("mocks left out = %d, want 1: the exchange the abort cut is counted", n)
	}
	if over, _ := spans.Overlaps(reqAt, respAt); !over {
		t.Fatalf("no span covers the exchange the abort cut: test cases over it would be saved without it")
	}
	var reason string
	for _, e := range logs.FilterMessage(supervisor.LeftOutWarnMsg).All() {
		reason = e.ContextMap()["reason"].(string)
	}
	// The text before the first colon is the cause the WARN is limited by.
	if !strings.HasPrefix(reason, "parser_hung:") {
		t.Fatalf("left-out reason = %q, want it to say the parser was retired for its hang (parser_hung)", reason)
	}
}
