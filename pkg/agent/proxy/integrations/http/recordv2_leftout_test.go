package http

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// leftOutRun is what recordV2 did with an exchange it could not record.
type leftOutRun struct {
	err   error
	mocks int
	spans *syncMock.Spans
	mgr   *syncMock.SyncMockManager
	logs  *observer.ObservedLogs
	sess  *supervisor.Session
	// started and returned bound when recordV2 ran: it was called at started
	// and had returned by returned.
	started, returned time.Time
}

// leftOutSession is a session that keeps the spans it cannot record and counts
// the mocks it leaves out on a manager of its own, and a recorder that logs
// what it says.
type leftOutSession struct {
	h                   *HTTP
	sess                *supervisor.Session
	sendReq, sendResp   func([]byte, time.Time, time.Time)
	closeReq, closeResp func()
	mocks               chan *models.Mock
	spans               *syncMock.Spans
	mgr                 *syncMock.SyncMockManager
	logs                *observer.ObservedLogs
}

func newLeftOutSession(t *testing.T) *leftOutSession {
	t.Helper()
	// The left-out WARN is limited process-wide: count only this test's.
	supervisor.ResetLeftOutWarningsForTest()
	core, logs := observer.New(zapcore.DebugLevel)
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
	sess.Logger = zap.New(core)
	spans := &syncMock.Spans{}
	sess.Orphans = spans
	mgr := syncMock.New(nil)
	sess.Mgr = mgr
	return &leftOutSession{
		h: &HTTP{Logger: zap.New(core)}, sess: sess,
		sendReq: sendReq, sendResp: sendResp, closeReq: closeReq, closeResp: closeResp,
		mocks: mocks, spans: spans, mgr: mgr, logs: logs,
	}
}

// done is the run, once recordV2 has returned err.
func (l *leftOutSession) done(err error) leftOutRun {
	close(l.mocks)
	n := 0
	for range l.mocks {
		n++
	}
	return leftOutRun{err: err, mocks: n, spans: l.spans, mgr: l.mgr, logs: l.logs, sess: l.sess}
}

// recordLeftOut runs recordV2 over what feed hands its session (a
// leftOutSession's), until both streams end.
func recordLeftOut(t *testing.T, feed func(sess *supervisor.Session, sendReq, sendResp func([]byte, time.Time, time.Time), closeResp func())) leftOutRun {
	t.Helper()
	l := newLeftOutSession(t)
	feed(l.sess, l.sendReq, l.sendResp, l.closeResp)
	l.closeReq()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	r := l.done(l.h.recordV2(ctx, l.sess))
	r.started, r.returned = started, time.Now()
	return r
}

// nothingReported fails t unless the run left nothing out: no span, no count,
// no left-out WARN, and no mock either.
func (r leftOutRun) nothingReported(t *testing.T) {
	t.Helper()
	if r.mocks != 0 {
		t.Fatalf("%d mocks recorded, want none", r.mocks)
	}
	if c, o := r.spans.Counts(); c != 0 || o != 0 {
		t.Fatalf("the spans hold (closed=%d, open=%d), want none", c, o)
	}
	if n := r.mgr.MocksLeftOut(); n != 0 {
		t.Fatalf("the manager counts %d mocks left out, want none", n)
	}
	if w := r.warned(); len(w) != 0 {
		t.Fatalf("%d left-out WARNs, want none: %v", len(w), w[0].ContextMap())
	}
}

// warned are the left-out WARNs a run logged.
func (r leftOutRun) warned() []observer.LoggedEntry {
	return r.logs.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("was not recorded as a mock").All()
}

// A parser that returns on an exchange it cannot record reports it before it
// returns, as every parser reports the exchange it stops on
// (Session.ReportStoppedOn): its span, from its request to the stop, so the
// test cases over it, and over what the connection carried that the parser had
// not read yet, are left out; a count, for the recording's summary; and a WARN
// with why. It used to mark the incomplete-mock flag and return: no later
// EmitMock took the mark, so the exchange was lost with nothing reporting it,
// and the test cases over it were saved and failed replay with no_mocks. Each
// way the HTTP recorder stops on an exchange it captured and cannot record.
func TestRecordV2_ReportsAnExchangeItStopsOn(t *testing.T) {
	var (
		reqAt   = time.Unix(1_700_007_000, 0)
		respAt  = reqAt.Add(5 * time.Millisecond)
		laterAt = respAt.Add(2 * time.Second)
	)
	for _, tc := range []struct {
		name    string
		feed    func(sess *supervisor.Session, sendReq, sendResp func([]byte, time.Time, time.Time), closeResp func())
		end     time.Time // the last of the exchange the recorder read
		wantErr bool
		reason  string
	}{
		{
			name: "its request does not decode",
			feed: func(_ *supervisor.Session, sendReq, _ func([]byte, time.Time, time.Time), closeResp func()) {
				sendReq([]byte("POST /x HTTP/1.1\r\nHost: ex\r\nContent-Length: NOTANUMBER\r\n\r\n"), reqAt, reqAt)
				closeResp()
			},
			end: reqAt, wantErr: true, reason: "request read failed",
		},
		{
			name: "its response's first read fails",
			feed: func(sess *supervisor.Session, sendReq, _ func([]byte, time.Time, time.Time), _ func()) {
				sendReq(canonicalRequest, reqAt, reqAt)
				_ = sess.DestStream.SetReadDeadline(time.Now().Add(-time.Second))
			},
			end: reqAt, wantErr: true, reason: "initial response read failed",
		},
		{
			name: "its response does not decode",
			feed: func(_ *supervisor.Session, sendReq, sendResp func([]byte, time.Time, time.Time), closeResp func()) {
				sendReq(canonicalRequest, reqAt, reqAt)
				sendResp([]byte("HTTP/1.1 200 OK\r\nContent-Length: NOTANUMBER\r\n\r\n"), respAt, respAt)
				closeResp()
			},
			end: respAt, wantErr: true, reason: "response read failed",
		},
		{
			// The response's first chunk frames and its tail does not: the
			// span reaches the tail's write, the response's last, and on to
			// the stop. A test case recorded over the tail is over the
			// exchange too.
			name: "its response stops decoding past its first chunk",
			feed: func(_ *supervisor.Session, sendReq, sendResp func([]byte, time.Time, time.Time), closeResp func()) {
				sendReq(canonicalRequest, reqAt, reqAt)
				sendResp([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n"), respAt, respAt)
				sendResp([]byte("ZZZZ\r\n"), laterAt.Add(-time.Millisecond), laterAt)
				closeResp()
			},
			end: laterAt, wantErr: true, reason: "response read failed",
		},
		{
			name: "its mock cannot be built",
			feed: func(_ *supervisor.Session, sendReq, sendResp func([]byte, time.Time, time.Time), closeResp func()) {
				// A gzip body that is not gzip: the request frames, and
				// does not decompress.
				sendReq([]byte("POST /x HTTP/1.1\r\nHost: ex\r\nContent-Encoding: gzip\r\nContent-Length: 3\r\n\r\nabc"), reqAt, reqAt)
				sendResp(canonicalResponse, respAt, respAt)
				closeResp()
			},
			end: respAt, wantErr: true, reason: "decompress request body",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := recordLeftOut(t, tc.feed)
			if (r.err != nil) != tc.wantErr {
				t.Fatalf("recordV2 returned %v, want an error: %v", r.err, tc.wantErr)
			}
			if r.mocks != 0 {
				t.Fatalf("%d mocks recorded, want none", r.mocks)
			}
			if c, o := r.spans.Counts(); c != 1 || o != 0 {
				t.Fatalf("the spans hold (closed=%d, open=%d), want the one exchange", c, o)
			}
			if over, _ := r.spans.Overlaps(reqAt, reqAt); !over {
				t.Fatal("a test case over the exchange is not left out")
			}
			if over, _ := r.spans.Overlaps(tc.end, tc.end); !over {
				t.Fatal("the span stops short of the exchange's end: a test case over its end is not left out")
			}
			// The stop came after recordV2 was called, past every byte it
			// read, and no later than its return.
			if over, _ := r.spans.Overlaps(r.started, r.started); !over {
				t.Fatal("the span stops short of the stop: a test case over what the connection carried that the parser had not read yet is not left out")
			}
			if over, _ := r.spans.Overlaps(r.returned.Add(time.Millisecond), r.returned.Add(time.Second)); over {
				t.Fatal("the span reaches past the stop")
			}
			if over, _ := r.spans.Overlaps(reqAt.Add(-time.Second), reqAt.Add(-time.Millisecond)); over {
				t.Fatal("the span starts before the exchange")
			}
			if n := r.mgr.MocksLeftOut(); n != 1 {
				t.Fatalf("the manager counts %d mocks left out, want the one exchange", n)
			}
			warns := r.warned()
			if len(warns) != 1 {
				t.Fatalf("%d left-out WARNs, want one", len(warns))
			}
			fields := warns[0].ContextMap()
			if reason, _ := fields["reason"].(string); !strings.Contains(reason, tc.reason) {
				t.Fatalf("the WARN's reason is %q, want one that says %q", reason, tc.reason)
			}
			if fields["kind"] != string(models.HTTP) {
				t.Fatalf("the WARN's kind is %v, want %s", fields["kind"], models.HTTP)
			}
			if r.sess.IsMockIncomplete() {
				t.Fatal("the incomplete-mock flag is set: a mark nothing will take")
			}
		})
	}
}

// A chunk the relay lost on the connection (its mark on the incomplete-mock
// flag) is the likelier cause of an exchange the parser cannot read, and the
// mark is the exchange's to report: the parser returns, and no later mock
// takes it. The exchange is reported once, under the mark's cause, so its
// WARN carries the setting that fits the loss.
func TestRecordV2_ReportsAnExchangeItStopsOnUnderTheRelaysMark(t *testing.T) {
	reqAt := time.Unix(1_700_008_000, 0)
	respAt := reqAt.Add(5 * time.Millisecond)
	r := recordLeftOut(t, func(sess *supervisor.Session, sendReq, sendResp func([]byte, time.Time, time.Time), closeResp func()) {
		sendReq(canonicalRequest, reqAt, reqAt)
		sess.MarkMockIncomplete("per_conn_cap")
		sendResp([]byte("HTTP/1.1 200 OK\r\nContent-Length: NOTANUMBER\r\n\r\n"), respAt, respAt)
		closeResp()
	})
	if r.err == nil {
		t.Fatal("recordV2 returned nil on a response that does not decode")
	}
	if n := r.mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("the manager counts %d mocks left out, want the one exchange", n)
	}
	if over, _ := r.spans.Overlaps(reqAt, respAt); !over {
		t.Fatal("a test case over the exchange is not left out")
	}
	warns := r.warned()
	if len(warns) != 1 {
		t.Fatalf("%d left-out WARNs, want one", len(warns))
	}
	fields := warns[0].ContextMap()
	reason, _ := fields["reason"].(string)
	if !strings.HasPrefix(reason, "per_conn_cap:") || !strings.Contains(reason, "response read failed") {
		t.Fatalf("the WARN's reason is %q, want the mark's cause, then what the parser saw", reason)
	}
	if next, _ := fields["next_step"].(string); !strings.Contains(next, "maxMemoryPerConnection") {
		t.Fatalf("the WARN's next_step is %q, want the setting for per_conn_cap", next)
	}
	if r.sess.IsMockIncomplete() {
		t.Fatal("the mark was not taken")
	}
}

// A request the server's stream ends without answering is not an exchange
// lost: there is no response, so there is no mock to record. Most often it is
// the keep-alive idle-close race, in which the upstream closes a pooled
// connection as idle just as the app sends its next request on it; the app's
// HTTP client retries the request on a new connection, and that is recorded.
// In an observe-only capture it is the client that gave up and closed. Or the
// session closed the stream: the supervisor's abort, whose fallthrough leaves
// out the rest of the connection, or the recording's stop. Reporting it would
// leave out every test case in flight then, on any route
// (TestRecordViaSupervisor_AnUpstreamIdleCloseLeavesNothingOut).
//
// A mark on the incomplete-mock flag does not make it one either. Response
// bytes the capture lost stopped the connection's capture (the HTTP parser
// cannot re-align after a hole), and the capture leaves out the test cases the
// connection carries from the loss on itself. And the relay marks a request
// it could not write to the upstream (write_error): the same race, when the
// app writes its request in more than one piece.
func TestRecordV2_ReportsNothingWhenTheServersStreamEndsWithoutAResponse(t *testing.T) {
	reqAt := time.Unix(1_700_009_000, 0)
	for _, end := range []struct {
		name string
		end  func(sess *supervisor.Session, sendResp func([]byte, time.Time, time.Time), closeResp func())
	}{
		{"the server closed", func(_ *supervisor.Session, _ func([]byte, time.Time, time.Time), closeResp func()) { closeResp() }},
		{"the stream ends with an empty chunk", func(_ *supervisor.Session, sendResp func([]byte, time.Time, time.Time), _ func()) {
			sendResp(nil, reqAt, reqAt)
		}},
		{"its session closed the stream", func(sess *supervisor.Session, _ func([]byte, time.Time, time.Time), _ func()) {
			_ = sess.DestStream.Close()
		}},
	} {
		for _, mark := range []string{"", "per_conn_cap", "write_error"} {
			name := end.name
			if mark != "" {
				name += ", under the relay's mark " + mark
			}
			t.Run(name, func(t *testing.T) {
				r := recordLeftOut(t, func(sess *supervisor.Session, sendReq, sendResp func([]byte, time.Time, time.Time), closeResp func()) {
					sendReq(canonicalRequest, reqAt, reqAt)
					if mark != "" {
						sess.MarkMockIncomplete(mark)
					}
					end.end(sess, sendResp, closeResp)
				})
				if r.err != nil {
					t.Fatalf("recordV2 returned %v", r.err)
				}
				r.nothingReported(t)
			})
		}
	}
}

// A recording's stop cancels the parser's context, and the streams end or
// fail after it: nothing the parser was in the middle of is reported, though
// the relay had marked the connection. Each subtest blocks the parser where
// it reads, with nothing for it (Waiting), then stops the recording, then
// hands it what ends its read.
func TestRecordV2_ReportsNothingAtARecordingStop(t *testing.T) {
	reqAt := time.Unix(1_700_009_500, 0)
	respAt := reqAt.Add(5 * time.Millisecond)
	for _, tc := range []struct {
		name    string
		feed    func(l *leftOutSession)
		blocked func(sess *supervisor.Session) *fakeconn.FakeConn
		wake    func(l *leftOutSession)
	}{
		{
			name: "its request's body is still to come",
			feed: func(l *leftOutSession) {
				l.sendReq([]byte("POST /x HTTP/1.1\r\nHost: ex\r\nContent-Length: 10\r\n\r\nab"), reqAt, reqAt)
			},
			blocked: func(sess *supervisor.Session) *fakeconn.FakeConn { return sess.ClientStream },
			wake:    func(l *leftOutSession) { l.sendReq([]byte("cd"), reqAt, reqAt) },
		},
		{
			name:    "its response's first read fails",
			feed:    func(l *leftOutSession) { l.sendReq(canonicalRequest, reqAt, reqAt) },
			blocked: func(sess *supervisor.Session) *fakeconn.FakeConn { return sess.DestStream },
			wake:    func(l *leftOutSession) { _ = l.sess.DestStream.SetReadDeadline(time.Now().Add(-time.Second)) },
		},
		{
			name:    "the server's stream ends before its response",
			feed:    func(l *leftOutSession) { l.sendReq(canonicalRequest, reqAt, reqAt) },
			blocked: func(sess *supervisor.Session) *fakeconn.FakeConn { return sess.DestStream },
			wake:    func(l *leftOutSession) { l.closeResp() },
		},
		{
			name:    "the server's stream ends with an empty chunk",
			feed:    func(l *leftOutSession) { l.sendReq(canonicalRequest, reqAt, reqAt) },
			blocked: func(sess *supervisor.Session) *fakeconn.FakeConn { return sess.DestStream },
			wake:    func(l *leftOutSession) { l.sendResp(nil, respAt, respAt) },
		},
		{
			name: "its response's body is still to come",
			feed: func(l *leftOutSession) {
				l.sendReq(canonicalRequest, reqAt, reqAt)
				l.sendResp([]byte("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nab"), respAt, respAt)
			},
			blocked: func(sess *supervisor.Session) *fakeconn.FakeConn { return sess.DestStream },
			wake:    func(l *leftOutSession) { l.sendResp([]byte("cd"), respAt, respAt) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLeftOutSession(t)
			// Fed before the parser starts, so the first read it blocks on
			// is the one under test.
			tc.feed(l)
			l.sess.MarkMockIncomplete("per_conn_cap")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- l.h.recordV2(ctx, l.sess) }()

			stream := tc.blocked(l.sess)
			for deadline := time.Now().Add(10 * time.Second); ; {
				if waiting, _ := stream.Waiting(); waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the parser never blocked on the read under test")
				}
				runtime.Gosched()
			}
			cancel()
			tc.wake(l)

			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("recordV2 did not return after the stop")
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("recordV2 returned %v, want nil or the stop's context.Canceled", err)
			}
			l.done(err).nothingReported(t)
		})
	}
}
