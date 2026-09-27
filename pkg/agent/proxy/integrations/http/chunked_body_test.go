package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// scriptedConn is a client (or upstream) connection whose reads return a fixed
// script — each step is one Read result — and then BLOCK, as a real peer does
// once it has sent a whole request and is waiting for the answer. A reader that
// misses the end of the message therefore hangs here exactly as it hangs on a
// live connection; the tests bound that with a timeout instead of seeing EOF.
type scriptedConn struct {
	mu        sync.Mutex
	steps     []readStep
	reads     int
	closed    chan struct{}
	once      sync.Once
	wrote     bytes.Buffer
	deadlines []time.Time // every read deadline set, in order
}

type readStep struct {
	data []byte
	err  error
}

func newScriptedConn(steps ...readStep) *scriptedConn {
	return &scriptedConn{steps: steps, closed: make(chan struct{})}
}

func (c *scriptedConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	if len(c.steps) == 0 {
		c.mu.Unlock()
		<-c.closed
		return 0, io.EOF
	}
	st := &c.steps[0]
	n := copy(b, st.data)
	st.data = st.data[n:]
	var err error
	if len(st.data) == 0 {
		err = st.err
		c.steps = c.steps[1:]
	}
	c.reads++
	c.mu.Unlock()
	return n, err
}

func (c *scriptedConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wrote.Write(b)
}
func (c *scriptedConn) Close() error                     { c.once.Do(func() { close(c.closed) }); return nil }
func (c *scriptedConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *scriptedConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *scriptedConn) SetDeadline(t time.Time) error    { return c.SetReadDeadline(t) }
func (c *scriptedConn) SetWriteDeadline(time.Time) error { return nil }

// SetReadDeadline records the deadline; reads do not honour it (a script
// step returns a timeout itself).
func (c *scriptedConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadlines = append(c.deadlines, t)
	return nil
}

// timeoutErr is what a Read returns when its deadline passes.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

const chunkedUploadHead = "POST /v1/echo?via=upload HTTP/1.1\r\n" +
	"Host: tlsup.fkjsse.svc.cluster.local\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Transfer-Encoding: chunked\r\n" +
	"\r\n"

// runChunkedRequest runs HandleChunkedRequests (destConn nil, as at replay) on
// first — the bytes already read — and conn, and fails the test if it does not
// return within a second.
func runChunkedRequest(t *testing.T, first string, conn *scriptedConn) (string, error) {
	t.Helper()
	t.Cleanup(func() { _ = conn.Close() })
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	buf := []byte(first)
	done := make(chan error, 1)
	go func() { done <- h.HandleChunkedRequests(context.Background(), &buf, conn, nil) }()
	select {
	case err := <-done:
		return string(buf), err
	case <-time.After(time.Second):
		t.Fatalf("HandleChunkedRequests did not return after the request ended (%d reads); "+
			"it is waiting for bytes the client will never send", conn.reads)
		return "", nil
	}
}

// The replay hang behind the Flipkart-shaped recording's upload test: Java's
// HttpURLConnection writes a chunked-streaming upload's last-chunk "0\r\n" and
// the final "\r\n" separately, so they reach the proxy in different reads.
// Neither read ends in "0\r\n\r\n", so the proxy never saw the request end, the
// app never got a response, and replay timed out the test.
func TestHandleChunkedRequestsTerminatorSplitAcrossReads(t *testing.T) {
	data := strings.Repeat("a", 51552)
	conn := newScriptedConn(
		readStep{data: []byte("c960\r\n" + data + "\r\n")},
		readStep{data: []byte("0\r\n")},
		readStep{data: []byte("\r\n")},
	)
	got, err := runChunkedRequest(t, chunkedUploadHead+"fff8\r\n"+strings.Repeat("b", 0xfff8)+"\r\n", conn)
	if err != nil {
		t.Fatalf("HandleChunkedRequests: %v", err)
	}
	want := chunkedUploadHead + "fff8\r\n" + strings.Repeat("b", 0xfff8) + "\r\n" + "c960\r\n" + data + "\r\n0\r\n\r\n"
	if got != want {
		t.Fatalf("assembled request is %d bytes, want %d", len(got), len(want))
	}
}

// The framing headers come from the header section only. Java sends the
// headers and the first 64 KiB of a chunked-streaming upload in one read, so a
// multipart part header in that read ("Content-Length: 10") was taken for the
// request's own and the request was cut after it.
func TestHandleChunkedRequestsIgnoresHeaderLikeBodyLines(t *testing.T) {
	part := "Content-Length: 10\r\n\r\n0123456789"
	conn := newScriptedConn(readStep{data: []byte(part[22:] + "\r\n0\r\n\r\n")})
	got, err := runChunkedRequest(t, chunkedUploadHead+"20\r\n"+part[:22], conn)
	if err != nil {
		t.Fatalf("HandleChunkedRequests: %v", err)
	}
	if want := chunkedUploadHead + "20\r\n" + part + "\r\n0\r\n\r\n"; got != want {
		t.Fatalf("request cut short at a body line:\n got %q\nwant %q", got, want)
	}
}

// The read deadline that bounds each body read must not outlive the request:
// the connection is kept alive, and a 5 s deadline left on it dropped a client
// that paused more than 5 s before its next request. Both body readers — the
// chunked one and the Content-Length one — set a deadline per read and clear
// it when the body is read.
func TestHandleChunkedRequestsClearsItsReadDeadline(t *testing.T) {
	for _, c := range []struct{ name, head, body string }{
		{"chunked", chunkedUploadHead, "5\r\nhello\r\n0\r\n\r\n"},
		{"content-length", "POST /v1/echo HTTP/1.1\r\nHost: tlsup\r\nContent-Length: 5\r\n\r\n", "hello"},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn := newScriptedConn(readStep{data: []byte(c.body)})
			got, err := runChunkedRequest(t, c.head, conn)
			if err != nil {
				t.Fatalf("HandleChunkedRequests: %v", err)
			}
			if got != c.head+c.body {
				t.Fatalf("request = %q, want %q", got, c.head+c.body)
			}
			conn.mu.Lock()
			defer conn.mu.Unlock()
			if len(conn.deadlines) == 0 || conn.deadlines[0].IsZero() {
				t.Fatalf("no read deadline bounded the body reads (deadlines set: %v)", conn.deadlines)
			}
			if last := conn.deadlines[len(conn.deadlines)-1]; !last.IsZero() {
				t.Fatalf("read deadline %v left on the connection", last)
			}
		})
	}
}

// Empty lines before a request line are skipped (RFC 9112 §2.2): an old
// client sends a CRLF after a POST body, so a keep-alive connection's next
// request can start with two. The first "\r\n\r\n" is then no empty header
// section, and the request's body is framed by its own headers. The empty
// lines are not part of the request and are dropped from it.
func TestHandleChunkedRequestsSkipsEmptyLinesBeforeTheRequestLine(t *testing.T) {
	// The request line, the rest of the header section and the body arrive
	// in separate reads.
	const line, fields = "\r\n\r\nPOST /v1/echo HTTP/1.1\r\n", "Host: tlsup\r\nContent-Length: 5\r\n\r\n"
	const head = "POST /v1/echo HTTP/1.1\r\n" + fields
	t.Run("replay request", func(t *testing.T) {
		got, err := runChunkedRequest(t, line, newScriptedConn(readStep{data: []byte(fields)}, readStep{data: []byte("hello")}))
		if err != nil || got != head+"hello" {
			t.Fatalf("HandleChunkedRequests = (%q, %v), want its body read too: %q", got, err, head+"hello")
		}
	})
	t.Run("V2 request", func(t *testing.T) {
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		stream, send, _ := makeStream(t, fakeconn.FromClient, 8)
		now := time.Now()
		send([]byte(line), now, now)
		send([]byte(fields), now, now)
		send([]byte("hello"), now, now)
		var finalReq []byte
		done := make(chan error, 1)
		go func() { done <- h.readRequestV2(context.Background(), stream, &finalReq) }()
		select {
		case err := <-done:
			if err != nil || string(finalReq) != head+"hello" {
				t.Fatalf("readRequestV2 = (%q, %v), want its body read too: %q", finalReq, err, head+"hello")
			}
		case <-time.After(time.Second):
			t.Fatal("readRequestV2 did not return")
		}
	})
	t.Run("messageHead", func(t *testing.T) {
		for _, c := range []struct {
			msg      string
			bodyFrom int
			ok       bool
		}{
			{"\r\n\r\nGET / HTTP/1.1\r\n\r\n", len("\r\n\r\nGET / HTTP/1.1\r\n\r\n"), true},
			{"\r\n\r\nGET / HTTP/1.1\r\n", 0, false},
			{"\r\n\r\n", 0, false},
		} {
			_, bodyStart, _, ok := messageHead([]byte(c.msg), false)
			if ok != c.ok || (ok && bodyStart != c.bodyFrom) {
				t.Errorf("messageHead(%q) = body at %d, ok %v; want %d, %v", c.msg, bodyStart, ok, c.bodyFrom, c.ok)
			}
		}
	})
}

// A body the RFC framing rejects (here a bare LF after chunk data): at replay
// there is nothing to relay and the request cannot be answered, so it fails;
// in record mode the proxy is in the app's live path and must keep relaying,
// ending the message as it did before this framing existed.
func TestChunkedRequestUnframeableBody(t *testing.T) {
	body := "5\r\nhello\n0\r\n\r\n"
	t.Run("replay", func(t *testing.T) {
		if _, err := runChunkedRequest(t, chunkedUploadHead+body, newScriptedConn()); !errors.Is(err, errMalformedChunkedBody) {
			t.Fatalf("err = %v, want errMalformedChunkedBody", err)
		}
	})
	t.Run("record", func(t *testing.T) {
		client := newScriptedConn(readStep{data: []byte(body)})
		dest := newScriptedConn()
		t.Cleanup(func() { _ = client.Close(); _ = dest.Close() })
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		buf := []byte(chunkedUploadHead)
		done := make(chan error, 1)
		go func() { done <- h.HandleChunkedRequests(context.Background(), &buf, client, dest) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("HandleChunkedRequests: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("record-mode relay stopped on an unframeable body")
		}
		if got := dest.wrote.String(); got != body {
			t.Fatalf("relayed %q, want %q", got, body)
		}
	})
}

// Interim 1xx responses carry no body and can arrive in the same read as the
// final chunked response; the body starts after the FINAL response's headers.
func TestChunkedResponseAfterAnInterimResponse(t *testing.T) {
	head := "HTTP/1.1 103 Early Hints\r\nLink: </a.css>; rel=preload\r\n\r\n" +
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n"
	dest := newScriptedConn()
	t.Cleanup(func() { _ = dest.Close() })
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	finalResp := []byte(head)
	done := make(chan error, 1)
	go func() {
		done <- h.handleChunkedResponses(context.Background(), &finalResp, newScriptedConn(), dest, []byte(head), "GET")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleChunkedResponses: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handleChunkedResponses did not end a complete response that followed a 103")
	}
	if string(finalResp) != head {
		t.Fatalf("finalResp = %q", finalResp)
	}
}

// The answer to a HEAD, a 204 and a 304 have no body even when they say
// "Transfer-Encoding: chunked"; waiting for a last-chunk hung the connection.
func TestChunkedResponsesWithoutABody(t *testing.T) {
	for _, tc := range []struct{ method, status string }{{"HEAD", "200 OK"}, {"GET", "204 No Content"}, {"GET", "304 Not Modified"}} {
		t.Run(tc.method+" "+tc.status, func(t *testing.T) {
			head := "HTTP/1.1 " + tc.status + "\r\nTransfer-Encoding: chunked\r\n\r\n"
			dest := newScriptedConn()
			t.Cleanup(func() { _ = dest.Close() })
			h := &HTTP{Logger: zaptest.NewLogger(t)}
			finalResp := []byte(head)
			done := make(chan error, 2)
			go func() {
				done <- h.handleChunkedResponses(context.Background(), &finalResp, newScriptedConn(), dest, []byte(head), tc.method)
			}()
			stream, send, _ := makeStream(t, fakeconn.FromDest, 8)
			send([]byte(head), time.Now(), time.Now())
			var v2Resp []byte
			go func() {
				_, err := h.readResponseV2(context.Background(), stream, &v2Resp, tc.method)
				done <- err
			}()
			for i := 0; i < 2; i++ {
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("err: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("a response without a body was waited on for one")
				}
			}
		})
	}
}

// The V2 record loop frames each answer by its request's method. The answer to
// a HEAD carries the Content-Length a GET would get and no body, so framed
// without the method, the next response on the keep-alive connection was read
// as its body, and the next request was left without an answer.
func TestRecordV2FramesTheAnswerToAHEADByItsMethod(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
	now := time.Now()
	sendReq([]byte("HEAD /file HTTP/1.1\r\nHost: up\r\n\r\n"), now, now)
	sendResp([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\n"), now, now)
	sendReq([]byte("GET /file HTTP/1.1\r\nHost: up\r\n\r\n"), now, now)
	sendResp([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello"), now, now)
	closeReq()
	closeResp()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.recordV2(ctx, sess); err != nil {
		t.Fatalf("recordV2: %v", err)
	}
	close(mocks)
	var got []string
	for m := range mocks {
		got = append(got, fmt.Sprintf("%s %s -> %d %q", m.Spec.HTTPReq.Method, m.Spec.HTTPReq.URL, m.Spec.HTTPResp.StatusCode, m.Spec.HTTPResp.Body))
	}
	want := []string{`HEAD /file -> 200 ""`, `GET /file -> 200 "hello"`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("recorded\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// Chunk DATA that ends in "0\r\n\r\n" at a read boundary is not the end of the
// body — a multipart upload's part header "Content-Length: 10\r\n\r\n" is
// exactly that. The suffix test took it for the terminator and cut the request.
func TestHandleChunkedRequestsDataEndingLikeTerminator(t *testing.T) {
	part := "Content-Length: 10\r\n\r\n0123456789"
	conn := newScriptedConn(
		readStep{data: []byte("20\r\n" + part[:22])},
		readStep{data: []byte(part[22:] + "\r\n0\r\n\r\n")},
	)
	got, err := runChunkedRequest(t, chunkedUploadHead, conn)
	if err != nil {
		t.Fatalf("HandleChunkedRequests: %v", err)
	}
	if want := chunkedUploadHead + "20\r\n" + part + "\r\n0\r\n\r\n"; got != want {
		t.Fatalf("request cut short at a data boundary:\n got %q\nwant %q", got, want)
	}
}

// A trailer section sits between the last-chunk and the final CRLF, so a
// message with trailers never ends in "0\r\n\r\n".
func TestHandleChunkedRequestsTrailer(t *testing.T) {
	conn := newScriptedConn(readStep{data: []byte("5\r\nhello\r\n0\r\nX-Checksum: 5d41\r\n\r\n")})
	got, err := runChunkedRequest(t, chunkedUploadHead, conn)
	if err != nil {
		t.Fatalf("HandleChunkedRequests: %v", err)
	}
	if !strings.HasSuffix(got, "X-Checksum: 5d41\r\n\r\n") {
		t.Fatalf("trailer missing from the assembled request: %q", got)
	}
}

// A read that returns bytes and then hits the read deadline hands both back
// (pUtil.ReadBytes returns what it collected with the timeout). Those bytes are
// part of the request; dropping them with the timeout lost the terminator.
func TestHandleChunkedRequestsKeepsBytesReadBeforeATimeout(t *testing.T) {
	body := "400\r\n" + strings.Repeat("z", 1024) + "\r\n0\r\n\r\n"
	conn := newScriptedConn(
		readStep{data: []byte(body[:1024])},
		readStep{err: timeoutErr{}},
		readStep{data: []byte(body[1024:])},
	)
	got, err := runChunkedRequest(t, chunkedUploadHead, conn)
	if err != nil {
		t.Fatalf("HandleChunkedRequests: %v", err)
	}
	if got != chunkedUploadHead+body {
		t.Fatalf("assembled request is %d bytes, want %d", len(got), len(chunkedUploadHead+body))
	}
}

// Record-mode relay of a chunked RESPONSE: the same split terminator must end
// the response, and every byte must have been relayed to the client.
func TestChunkedResponseTerminatorSplitAcrossReads(t *testing.T) {
	head := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	dest := newScriptedConn(
		readStep{data: []byte("5\r\nhello\r\n0\r\n")},
		readStep{data: []byte("\r\n")},
	)
	t.Cleanup(func() { _ = dest.Close() })
	client := newScriptedConn()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	finalResp := []byte(head)
	done := make(chan error, 1)
	go func() {
		done <- h.handleChunkedResponses(context.Background(), &finalResp, client, dest, []byte(head), "GET")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleChunkedResponses: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handleChunkedResponses did not return after the response ended")
	}
	if got, want := string(finalResp), head+"5\r\nhello\r\n0\r\n\r\n"; got != want {
		t.Fatalf("finalResp = %q, want %q", got, want)
	}
	if got := client.wrote.String(); got != "5\r\nhello\r\n0\r\n\r\n" {
		t.Fatalf("relayed to client %q", got)
	}
}

// The DaemonSet/V2 record path reads one captured chunk at a time and tested
// the WHOLE buffer's suffix: it could not miss a split terminator, but it cut a
// body whose data ends like one at a chunk boundary, and never ended one with a
// trailer.
func TestReadRequestV2ChunkedFraming(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pieces []string
	}{
		{"data ending like the terminator", []string{
			chunkedUploadHead + "20\r\nContent-Length: 10\r\n\r\n",
			"0123456789\r\n0\r\n\r\n",
		}},
		{"trailer", []string{chunkedUploadHead + "5\r\nhello\r\n0\r\n", "X-Checksum: 5d41\r\n", "\r\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &HTTP{Logger: zaptest.NewLogger(t)}
			stream, send, _ := makeStream(t, fakeconn.FromClient, 8)
			now := time.Now()
			for _, p := range tc.pieces {
				send([]byte(p), now, now)
			}
			var finalReq []byte
			done := make(chan error, 1)
			go func() { done <- h.readRequestV2(context.Background(), stream, &finalReq) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("readRequestV2: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("readRequestV2 did not return after the request ended")
			}
			if got, want := string(finalReq), strings.Join(tc.pieces, ""); got != want {
				t.Fatalf("finalReq = %q, want %q", got, want)
			}
		})
	}
}

func TestReadResponseV2ChunkedTrailer(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	stream, send, _ := makeStream(t, fakeconn.FromDest, 8)
	pieces := []string{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n", "X-Checksum: 5d41\r\n\r\n"}
	now := time.Now()
	for _, p := range pieces {
		send([]byte(p), now, now)
	}
	var finalResp []byte
	done := make(chan error, 1)
	go func() { _, err := h.readResponseV2(context.Background(), stream, &finalResp, "GET"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("readResponseV2: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("readResponseV2 did not return after the response ended")
	}
	if got, want := string(finalResp), strings.Join(pieces, ""); got != want {
		t.Fatalf("finalResp = %q, want %q", got, want)
	}
}

func TestChunkedBodyFeed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		done    bool
		end     int
		wantErr bool
	}{
		{name: "one chunk", body: "5\r\nhello\r\n0\r\n\r\n", done: true, end: 15},
		{name: "chunk extension and padding", body: "5;name=v \r\nhello\r\n0\r\n\r\n", done: true, end: 23},
		{name: "bare LF line ending", body: "5\nhello\r\n0\r\n\r\n", wantErr: true},
		{name: "bare LF in the trailer", body: "0\r\nA: b\n\r\n", wantErr: true},
		{name: "trailer", body: "0\r\nA: b\r\n\r\n", done: true, end: 11},
		{name: "bytes past the end", body: "0\r\n\r\nGET / HTTP/1.1\r\n", done: true, end: 5},
		{name: "terminator split", body: "5\r\nhello\r\n0\r\n"},
		{name: "partial size line", body: "5"},
		{name: "partial data", body: "5\r\nhel"},
		{name: "data resembles terminator", body: "a\r\nXY0\r\n\r\nZZZ\r\n"},
		{name: "bad size", body: "zz\r\n", wantErr: true},
		{name: "empty size", body: "\r\n", wantErr: true},
		{name: "no CRLF after data", body: "5\r\nhelloXX", wantErr: true},
		{name: "size overflow", body: "7fffffffffffffff\r\n", wantErr: true},
		{name: "size one above the cap", body: "80000000\r\n", wantErr: true},
		// At the cap the chunk is only incomplete. A 32-bit int cannot hold
		// it together with the size line in front of it, so there it is
		// rejected.
		{name: "size at the cap", body: "7fffffff\r\n", wantErr: strconv.IntSize == 32},
		// On a 32-bit build the chunk must end, with its CRLF, at or below
		// the largest int: 10 bytes of size line, the data, then 2.
		{name: "32-bit: largest chunk whose end fits", body: "7ffffff3\r\n"},
		{name: "32-bit: one past it", body: "7ffffff4\r\n", wantErr: strconv.IntSize == 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c chunkedBody
			done, err := c.feed([]byte(tc.body))
			if tc.wantErr {
				if !errors.Is(err, errMalformedChunkedBody) {
					t.Fatalf("feed err = %v, want errMalformedChunkedBody", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("feed: %v", err)
			}
			if done != tc.done || (done && c.end != tc.end) {
				t.Fatalf("feed = (done %v, end %d), want (%v, %d)", done, c.end, tc.done, tc.end)
			}
		})
	}
}

// feed resumes where it stopped: fed one byte at a time, a body frames exactly
// as it does whole.
func TestChunkedBodyFeedIncremental(t *testing.T) {
	body := "3\r\nabc\r\n10\r\n0123456789abcdef\r\n0\r\nT: 1\r\n\r\nNEXT"
	var c chunkedBody
	for i := 1; i <= len(body); i++ {
		done, err := c.feed([]byte(body[:i]))
		if err != nil {
			t.Fatalf("feed(%d bytes): %v", i, err)
		}
		if want := i >= len(body)-4; done != want {
			t.Fatalf("feed(%d bytes) done = %v, want %v", i, done, want)
		}
	}
	if c.end != len(body)-4 {
		t.Fatalf("end = %d, want %d", c.end, len(body)-4)
	}
}

// The mock of an exchange whose response came after an interim 1xx (a 100
// Continue to an "Expect: 100-continue" upload, a 103 Early Hints) records the
// FINAL response: net/http's ReadResponse stops at the 1xx, so the mock was a
// 100 with an empty body, which replay then served as the answer.
func TestMockOfAResponseAfterAnInterimResponse(t *testing.T) {
	req := []byte("POST /v1/upload HTTP/1.1\r\nHost: tlsup\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\nabc")
	for _, interim := range []string{
		"HTTP/1.1 100 Continue\r\n\r\n",
		"HTTP/1.1 103 Early Hints\r\nLink: </a.css>; rel=preload\r\n\r\n",
		"HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 103 Early Hints\r\nLink: </a.css>; rel=preload\r\n\r\n",
	} {
		resp := []byte(interim + "HTTP/1.1 201 Created\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n")
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		v2, err := h.buildHTTPMock(&FinalHTTP{Req: req, Resp: resp, ReqTimestampMock: time.Now(), ResTimestampMock: time.Now()},
			443, "test-conn", models.OutgoingOptions{})
		if err != nil || v2 == nil {
			t.Fatalf("%q: buildHTTPMock = (%v, %v)", interim, v2, err)
		}
		legacy := runLegacyParseFinalHTTP(t, h, req, resp, 443, "test-conn")
		for path, m := range map[string]*models.Mock{"V2 record": v2, "legacy record": legacy} {
			if got := m.Spec.HTTPResp; got == nil || got.StatusCode != 201 || got.Body != "hello" {
				t.Errorf("%q, %s: mock response = %+v, want 201 %q", interim, path, got, "hello")
			}
		}
	}
}

// A message with both "Transfer-Encoding: chunked" and a Content-Length is
// framed by its chunks (RFC 9112 §6.3); framing it by the Content-Length cut
// it short and left the rest to be read as the next message.
func TestTransferEncodingOverridesContentLength(t *testing.T) {
	const both = "POST /v1/echo HTTP/1.1\r\nHost: tlsup\r\nContent-Length: 4\r\nTransfer-Encoding: chunked\r\n\r\n"
	body := []string{"5\r\nhel", "lo\r\n0\r\n\r\n"}
	if cl, te := parseHeaders([]byte(both)); cl != "" || te != "chunked" {
		t.Errorf("parseHeaders = (%q, %q), want (\"\", \"chunked\")", cl, te)
	}

	t.Run("replay request", func(t *testing.T) {
		got, err := runChunkedRequest(t, both+body[0], newScriptedConn(readStep{data: []byte(body[1])}))
		if err != nil || got != both+strings.Join(body, "") {
			t.Fatalf("HandleChunkedRequests = (%q, %v)", got, err)
		}
	})
	t.Run("V2 request", func(t *testing.T) {
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		stream, send, _ := makeStream(t, fakeconn.FromClient, 8)
		now := time.Now()
		send([]byte(both+body[0]), now, now)
		send([]byte(body[1]), now, now)
		var finalReq []byte
		done := make(chan error, 1)
		go func() { done <- h.readRequestV2(context.Background(), stream, &finalReq) }()
		select {
		case err := <-done:
			if err != nil || string(finalReq) != both+strings.Join(body, "") {
				t.Fatalf("readRequestV2 = (%q, %v)", finalReq, err)
			}
		case <-time.After(time.Second):
			t.Fatal("readRequestV2 did not return")
		}
	})
	t.Run("record response", func(t *testing.T) {
		head := "HTTP/1.1 200 OK\r\nContent-Length: 4\r\nTransfer-Encoding: chunked\r\n\r\n" + body[0]
		dest := newScriptedConn(readStep{data: []byte(body[1])})
		client := newScriptedConn()
		t.Cleanup(func() { _ = dest.Close(); _ = client.Close() })
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		finalResp := []byte(head)
		done := make(chan error, 1)
		go func() {
			done <- h.handleChunkedResponses(context.Background(), &finalResp, client, dest, []byte(head), "GET")
		}()
		select {
		case err := <-done:
			if err != nil || string(finalResp) != head+body[1] {
				t.Fatalf("handleChunkedResponses = (%q, %v)", finalResp, err)
			}
		case <-time.After(time.Second):
			t.Fatal("handleChunkedResponses did not return")
		}
	})
}

// A 101 Switching Protocols (WebSocket) ends the HTTP exchange but not the
// connection: what follows is the other protocol. The V2 record path reads it
// to the close as one response, as before, instead of taking the upgraded
// stream for the next HTTP request.
func TestReadResponseV2ReadsAnUpgradedConnectionToItsClose(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	stream, send, closeStream := makeStream(t, fakeconn.FromDest, 8)
	now := time.Now()
	head := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
	send([]byte("\x81\x05hello"), now, now)
	finalResp := []byte(head) // the first chunk, already read by recordV2
	done := make(chan error, 1)
	go func() { _, err := h.readResponseV2(context.Background(), stream, &finalResp, "GET"); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("readResponseV2 returned (%v) at the 101's head, with the connection still open", err)
	case <-time.After(100 * time.Millisecond):
	}
	closeStream()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("readResponseV2 did not return at the close")
	}
	if string(finalResp) != head+"\x81\x05hello" {
		t.Fatalf("finalResp = %q", finalResp)
	}
}

// RFC 9112 2.2: a server ignores empty lines before a request line; so must
// the method that decides whether the response has a body.
func TestRequestMethodSkipsLeadingEmptyLines(t *testing.T) {
	if got := requestMethod([]byte("\r\nHEAD /x HTTP/1.1\r\nHost: a\r\n\r\n")); got != "HEAD" {
		t.Fatalf("requestMethod = %q, want HEAD", got)
	}
}

// A response that opens with an interim 1xx is recorded as its final
// response; its response time must be the final response's too.
func TestStartsWithInterimResponse(t *testing.T) {
	for resp, want := range map[string]bool{
		"HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\n": true,
		"HTTP/1.1 103 Early Hints\r\n":                     true,
		"HTTP/1.1 101 Switching Protocols\r\n\r\n":         false,
		"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n":     false,
	} {
		if got := startsWithInterimResponse([]byte(resp)); got != want {
			t.Errorf("startsWithInterimResponse(%q) = %v, want %v", resp, got, want)
		}
	}
}

// Header names are case-insensitive, and so is the 100-continue expectation
// (RFC 9110 5.1, 10.1.1): a lowercase "expect: 100-continue" (Node's http
// client sends it so) must get its 100 Continue, or the client waits for it
// while the proxy waits for the body.
func TestExpectsContinueIgnoresCase(t *testing.T) {
	for req, want := range map[string]bool{
		"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\n\r\n":                  true,
		"POST /u HTTP/1.1\r\nhost: a\r\nexpect: 100-Continue\r\n\r\n":                  true,
		"POST /u HTTP/1.1\r\nHost: a\r\nContent-Length: 3\r\n\r\nExpect: 100-continue": false, // a body line
		"POST /u HTTP/1.1\r\nHost: a\r\n\r\n":                                          false,
	} {
		if got := expectsContinue([]byte(req)); got != want {
			t.Errorf("expectsContinue(%q) = %v, want %v", req, got, want)
		}
	}
}
