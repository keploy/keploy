//go:build linux

package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	nethttp "net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// readRequestHead reads one request head off c, as the destination does.
func readRequestHead(t *testing.T, c net.Conn) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	r := bufio.NewReader(c)
	var head strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("the destination did not get a whole request head: %q, %v", head.String(), err)
		}
		head.WriteString(line)
		if line == "\r\n" {
			if r.Buffered() != 0 {
				t.Fatalf("the destination got %d bytes past the request head", r.Buffered())
			}
			return head.String()
		}
	}
}

// awaitFIN waits until c's peer has sent its FIN: c reads to its end.
func awaitFIN(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	if rest, err := io.ReadAll(c); err != nil || len(rest) != 0 {
		t.Fatalf("the destination read %d more bytes, %v, before the FIN", len(rest), err)
	}
}

// warnings is what the recording logged at WARN or above.
func (c *relayedConn) warnings() []observer.LoggedEntry {
	return c.logs.Filter(func(e observer.LoggedEntry) bool { return e.Level >= zapcore.WarnLevel }).All()
}

// Through the relay, as the proxy records: a Go http.Client gives up on a call
// (its context ends, as at a timeout) and closes the connection (close(2), not
// a half-close), and the dependency answers once the FIN has reached it. The
// relay forwards the answer, and the client's TCP stack refuses it with a
// reset: the application never read it. It is not recorded as a mock, and
// nothing is left out for it: the call got no answer the application saw, as
// when the dependency does not answer before the connection ends. Before, the
// answer was recorded, a mock for a call whose test case saw a timeout. The
// call's context is cancelled once the dependency has the request, not by a
// deadline, so a slow runner cannot end the call before its request is out.
func TestRecordV2_ThroughTheRelayALateAnswerToACallTheClientGaveUpOnIsNotRecorded(t *testing.T) {
	c := newRelayedConn(t)
	c.start(t)
	client := &nethttp.Client{
		Transport: &nethttp.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return c.app, nil
		}},
	}
	ctx, giveUp := context.WithCancel(context.Background())
	defer giveUp()
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodGet, "http://orders.internal:8443/orders/7", nil)
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		called <- err
	}()
	if head := readRequestHead(t, c.dest); !strings.HasPrefix(head, "GET /orders/7 HTTP/1.1\r\n") {
		t.Fatalf("the destination got %q", head)
	}
	// The client gives up and closes the connection; the relay forwards the
	// FIN.
	giveUp()
	awaitFIN(t, c.dest)
	if err := <-called; !errors.Is(err, context.Canceled) {
		t.Fatalf("the call returned %v, want it given up on", err)
	}
	if _, err := c.dest.Write([]byte(jdkResponse(7))); err != nil {
		t.Fatal(err)
	}
	mocks := c.end(t)
	if len(mocks) != 0 {
		t.Fatalf("recorded %d mock(s), the first %s -> %q; want none: the client had closed, and never read the answer",
			len(mocks), mocks[0].Spec.HTTPReq.URL, mocks[0].Spec.HTTPResp.Body)
	}
	if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
		t.Fatalf("left out %v (%d counted), want nothing: the call got no answer the application read", s, c.mgr.MocksLeftOut())
	}
	if w := c.warnings(); len(w) != 0 {
		t.Fatalf("logged at %v: %q", w[0].Level, w[0].Message)
	}
	// Said apart at Debug, as the one silent end that can hide a response an
	// application read (a half-closed client that reset before its stack
	// acknowledged it).
	if n := c.logs.FilterMessageSnippet("the client closed the connection before its stack acknowledged any of the response").Len(); n != 1 {
		t.Fatalf("%d Debug lines say the client closed before its stack acknowledged any of the response, want one", n)
	}
}

// Through the relay, as the proxy records: a client that half-closes after its
// request (shutdown(SHUT_WR)) and then reads its answer, as an EOF-delimited
// client does. The dependency answers once it has the FIN, the client takes
// the answer and reads it, and it is recorded as before.
func TestRecordV2_ThroughTheRelayAnAnswerAfterAHalfCloseIsRecorded(t *testing.T) {
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, jdkRequest(7))
	relayed(t, c.dest, jdkRequest(7))
	if err := c.app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	awaitFIN(t, c.dest)
	if _, err := c.dest.Write([]byte(jdkResponse(7))); err != nil {
		t.Fatal(err)
	}
	relayed(t, c.app, jdkResponse(7))
	assertPairs(t, c.end(t), 7)
	if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
		t.Fatalf("left out %v (%d counted), want nothing", s, c.mgr.MocksLeftOut())
	}
}

// leftOut is what the recording said of the exchanges it left out for reason:
// the left-out lines (the WARN, or the Debug line its process-wide limit holds
// back) with that reason.
func (c *relayedConn) leftOut(reason string) []observer.LoggedEntry {
	return c.logs.Filter(func(e observer.LoggedEntry) bool {
		return (e.Message == supervisor.LeftOutWarnMsg || e.Message == supervisor.LeftOutDebugMsg) &&
			e.ContextMap()["reason"] == reason
	}).All()
}

// Through the relay, as the proxy records: a client reads a response's head and
// the start of its body, all it has been sent so far, and closes the
// connection (its timeout, a cancelled call, or it wanted only the start); the
// rest of the response reaches the proxy after the client's FIN. The relay
// forwards it, the client refuses it with a reset, and the parser is not given
// it. The exchange is left out, and said so: the call did not complete, and a
// mock of the part the client had would replay a complete-looking shorter
// response to a call that saw a timeout. It is not taken for a response the
// server cut short (a decode error), and recordV2 returns nil. That holds for
// a parser running behind its capture too, which reads the cut response once
// the relay has ended and closed its streams: their end reads io.EOF however
// late it is read. A chunk the capture lost before the cut (a mark on the
// incomplete-mock flag) is the likelier cause of a response that does not
// end, so the exchange is reported under that mark, with its next step, and
// the mark is taken with it. A body the server's close ends (no Content-Length,
// no chunking) is left out too: there the stream's end is the body's end, and
// only the relay's word that the stream ended where the client stopped keeps
// its first 500 bytes from being recorded as a whole, shorter 200.
func TestRecordV2_ThroughTheRelayAResponseTheClientClosedPartwayIsLeftOutAndSaysWhy(t *testing.T) {
	for _, row := range []struct {
		name string
		// behind: recordV2 starts only once the relay has ended.
		behind bool
		// mark: the capture lost a chunk before the client closed, and
		// marked the mock incomplete for it.
		mark string
		// closeDelimited: the body ends with the server's close.
		closeDelimited bool
	}{
		{name: "the parser keeping up"},
		{name: "the parser behind its capture", behind: true},
		{name: "a chunk lost before the client closed", mark: "per_conn_cap"},
		{name: "a body the server's close ends", closeDelimited: true},
		{name: "a body the server's close ends, the parser behind its capture", behind: true, closeDelimited: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := newRelayedConn(t)
			c.startRelay(t)
			if !row.behind {
				c.startParser(t)
			}
			write(t, c.app, c.src, jdkRequest(7))
			relayed(t, c.dest, jdkRequest(7))
			body := strings.Repeat("0123456789", 200)
			head := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2000\r\n\r\n"
			if row.closeDelimited {
				head = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n"
			}
			read := head + body[:500]
			if _, err := c.dest.Write([]byte(read)); err != nil {
				t.Fatal(err)
			}
			relayed(t, c.app, read)
			if row.mark != "" {
				c.sess.MarkMockIncomplete(row.mark)
			}
			if err := c.app.Close(); err != nil {
				t.Fatal(err)
			}
			awaitFIN(t, c.dest)
			if _, err := c.dest.Write([]byte(body[500:])); err != nil {
				t.Fatal(err)
			}
			if row.behind {
				_ = c.dest.Close()
				select {
				case <-c.relayDone:
				case <-time.After(10 * time.Second):
					t.Fatal("the relay did not end once both sides had closed")
				}
				c.startParser(t)
			}
			mocks := c.end(t)
			if len(mocks) != 0 {
				t.Fatalf("recorded %d mock(s), the first a %d with a %d-byte body; want none: the client closed partway through the response",
					len(mocks), mocks[0].Spec.HTTPResp.StatusCode, len(mocks[0].Spec.HTTPResp.Body))
			}
			if n := c.mgr.MocksLeftOut(); n != 1 || len(c.spans.all()) != 1 {
				t.Fatalf("%d mocks left out, spans %v; want the one exchange, spanned", n, c.spans.all())
			}
			reason, step := supervisor.LeftOutClientClosedPartway, "the client closed partway"
			if row.mark != "" {
				reason, step = row.mark+": "+supervisor.LeftOutClientClosedPartway, "reason=per_conn_cap"
			}
			if l := c.leftOut(reason); len(l) != 1 {
				t.Fatalf("%d left-out lines with the reason %q, want one", len(l), reason)
			} else if l[0].Message == supervisor.LeftOutWarnMsg && !strings.Contains(l[0].ContextMap()["next_step"].(string), step) {
				t.Fatalf("the left-out WARN's next_step is %q, want the one of its cause (%q)", l[0].ContextMap()["next_step"], step)
			}
			if m, ok := c.sess.TakeMockIncomplete(); ok {
				t.Fatalf("the mark %q is still set: the exchange reported under it did not take it", m)
			}
			for _, w := range c.warnings() {
				if w.Message != supervisor.LeftOutWarnMsg {
					t.Fatalf("logged at %v: %q", w.Level, w.Message)
				}
			}
		})
	}
}

// Through the relay, as the proxy records: a client closes partway through a
// response, and the relay ends the destination's stream where the client
// stopped. But before the relay has ended the stream, the session closes the
// parser's stream (its abort, whose fallthrough leaves out the rest of the
// connection under its own reason): the parser's read fails, as a read of a
// closed stream does (ErrClosed), not at the stream's end (io.EOF). Only a
// stream that ran out ended where the client stopped, so the exchange is not
// reported as one the client closed partway: that would name the wrong cause.
func TestRecordV2_ThroughTheRelayAStreamTheSessionClosedIsNotTheClientsClosePartway(t *testing.T) {
	c := newRelayedConn(t)
	// The relay must still be capturing when the session closes the stream:
	// a grace this long keeps it from ending on an idle destination first.
	c.grace = time.Hour
	c.start(t)
	write(t, c.app, c.src, jdkRequest(7))
	relayed(t, c.dest, jdkRequest(7))
	body := strings.Repeat("0123456789", 200)
	read := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2000\r\n\r\n" + body[:500]
	if _, err := c.dest.Write([]byte(read)); err != nil {
		t.Fatal(err)
	}
	relayed(t, c.app, read)
	if err := c.app.Close(); err != nil {
		t.Fatal(err)
	}
	awaitFIN(t, c.dest)
	if _, err := c.dest.Write([]byte(body[500:])); err != nil {
		t.Fatal(err)
	}
	for limit := time.Now().Add(10 * time.Second); !c.sess.EndedWhereClientStopped(); {
		if time.Now().After(limit) {
			t.Fatal("the relay did not end the stream where the client stopped")
		}
		time.Sleep(50 * time.Microsecond)
	}
	select {
	case <-c.relayDone:
		t.Fatal("the relay ended before the session closed the stream")
	default:
	}
	_ = c.sess.DestStream.Close()
	_ = c.sess.ClientStream.Close()
	// What recordV2 returns, and reports, for a stream the session closed
	// under it is the abort's matter, not this row's.
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("recordV2 did not return once the session closed its streams")
	}
	if l := c.leftOut(supervisor.LeftOutClientClosedPartway); len(l) != 0 {
		t.Fatalf("%d left-out lines say the client closed partway, want none: the session closed the stream", len(l))
	}
	if n := c.logs.FilterMessageSnippet("the client closed the connection partway").Len(); n != 0 {
		t.Fatalf("%d lines say the client closed partway, want none", n)
	}
}

// Through the relay, as the proxy records, with the parser behind its capture
// (it starts only once the relay has ended): a client takes a whole response,
// sends its next request, and closes the connection before its answer, which
// it refuses. The parser reads the first response knowing the stream ended
// where the client stopped, and records it all the same: it is whole, and only
// a response the stream ends in the middle of is left out. The second request
// got none of its answer, so it is not recorded, and nothing is left out.
func TestRecordV2_ThroughTheRelayAWholeResponseBeforeTheClientStoppedIsRecorded(t *testing.T) {
	c := newRelayedConn(t)
	c.startRelay(t)
	write(t, c.app, c.src, jdkRequest(7))
	relayed(t, c.dest, jdkRequest(7))
	if _, err := c.dest.Write([]byte(jdkResponse(7))); err != nil {
		t.Fatal(err)
	}
	relayed(t, c.app, jdkResponse(7))
	write(t, c.app, c.src, jdkRequest(8))
	relayed(t, c.dest, jdkRequest(8))
	if err := c.app.Close(); err != nil {
		t.Fatal(err)
	}
	awaitFIN(t, c.dest)
	if _, err := c.dest.Write([]byte(jdkResponse(8))); err != nil {
		t.Fatal(err)
	}
	_ = c.dest.Close()
	select {
	case <-c.relayDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not end once both sides had closed")
	}
	if !c.sess.EndedWhereClientStopped() {
		t.Fatal("the relay did not say the destination's stream ended where the client stopped")
	}
	c.startParser(t)
	assertPairs(t, c.end(t), 7)
	if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
		t.Fatalf("left out %v (%d counted), want nothing", s, c.mgr.MocksLeftOut())
	}
	if w := c.warnings(); len(w) != 0 {
		t.Fatalf("logged at %v: %q", w[0].Level, w[0].Message)
	}
}

// resetBeforeWriteReturns is the proxy's end of the client connection whose
// Write returns only once the connection has been reset (or after 5s): a reset
// the client sends for what it was written is in before the relay can look at
// the socket again, every time, as it is by chance on a busy host when the
// client runs between the relay's write and its next step.
type resetBeforeWriteReturns struct {
	net.Conn
}

func (c *resetBeforeWriteReturns) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil {
		for limit := time.Now().Add(5 * time.Second); time.Now().Before(limit) && !isReset(c.Conn); {
			time.Sleep(50 * time.Microsecond)
		}
	}
	return n, err
}

func (c *resetBeforeWriteReturns) NetConn() net.Conn { return c.Conn }

// isReset reports whether the TCP socket under c is reset (TCP_CLOSE).
func isReset(c net.Conn) bool {
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		return false
	}
	var reset bool
	_ = rc.Control(func(fd uintptr) {
		if info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); err == nil {
			reset = info.State == unix.BPF_TCP_CLOSE
		}
	})
	return reset
}

// Through the relay, as the proxy records: a client reads a response's head and
// closes the connection with its body unread, so its stack resets instead of
// sending a FIN (an abortive close). The relay had written it the whole
// response before the reset, and the response is recorded as the server sent
// it, as before: no FIN reached the proxy before it was written, so nothing is
// refused, whenever the reset comes in.
func TestRecordV2_ThroughTheRelayAResponseTheClientResetWithItsBodyUnreadIsRecorded(t *testing.T) {
	for _, row := range []struct {
		name string
		wrap func(net.Conn) net.Conn
	}{
		{name: "the reset in its own time"},
		{name: "the reset in before the relay looks again", wrap: func(c net.Conn) net.Conn { return &resetBeforeWriteReturns{Conn: c} }},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := newRelayedConn(t)
			c.wrapSrc = row.wrap
			c.start(t)
			write(t, c.app, c.src, jdkRequest(7))
			relayed(t, c.dest, jdkRequest(7))
			body := strings.Repeat("0123456789", 200)
			head := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2000\r\n\r\n"
			want := uint64(received(t, c.app)) + uint64(len(head)+len(body))
			if _, err := c.dest.Write([]byte(head + body)); err != nil {
				t.Fatal(err)
			}
			relayed(t, c.app, head)
			for limit := time.Now().Add(5 * time.Second); received(t, c.app) < want; {
				if time.Now().After(limit) {
					t.Fatal("the client did not receive the whole response in 5s")
				}
				time.Sleep(50 * time.Microsecond)
			}
			if err := c.app.Close(); err != nil {
				t.Fatal(err)
			}
			mocks := c.end(t)
			if len(mocks) != 1 {
				t.Fatalf("recorded %d mocks, want the one exchange", len(mocks))
			}
			if r := mocks[0].Spec.HTTPResp; r.StatusCode != 200 || r.Body != body {
				t.Fatalf("recorded the response %d with a %d-byte body, want 200 with the %d bytes the server sent", r.StatusCode, len(r.Body), len(body))
			}
			if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
				t.Fatalf("left out %v (%d counted), want nothing", s, c.mgr.MocksLeftOut())
			}
			if w := c.warnings(); len(w) != 0 {
				t.Fatalf("logged at %v: %q", w[0].Level, w[0].Message)
			}
		})
	}
}
