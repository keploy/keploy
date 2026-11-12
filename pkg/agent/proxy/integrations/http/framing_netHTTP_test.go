package http

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

// net/http's ReadRequest refuses a request with two Host fields, and its
// server answers it 400; a response may carry two.
func TestParseHeadersRefusesARequestWithTwoHostFields(t *testing.T) {
	const fields = "Host: a\r\nhost: a\r\nContent-Length: 5\r\n\r\n"
	if _, err := http.ReadRequest(bufio.NewReader(strings.NewReader("POST /a HTTP/1.1\r\n" + fields))); err == nil {
		t.Fatal("net/http's ReadRequest read two Host fields")
	}
	if _, _, err := parseHeaders([]byte("POST /a HTTP/1.1\r\n"+fields), false); !refusedFraming(err) {
		t.Errorf("parseHeaders of a request with two Host fields: %v, want it refused", err)
	}
	if cl, _, err := parseHeaders([]byte("HTTP/1.1 200 OK\r\n"+fields), true); err != nil || cl != 5 {
		t.Errorf("parseHeaders of a response with two Host fields = (%d, %v), want (5, nil)", cl, err)
	}
	if got := newEchoServer(t).reads(t, "POST /a HTTP/1.1\r\n"+fields+"hello"); got != "400" {
		t.Errorf("net/http's server: %s, want 400", got)
	}
}

// A response read takes the framing net/http takes: a length up to 2^63-1, and
// a no-body answer's fields when they are ones net/http reads (it ends at its
// head).
func TestReadResponseTakesAFramingNetHTTPTakes(t *testing.T) {
	for head, method := range map[string]string{
		"HTTP/1.1 200 OK\r\nContent-Length: 9223372036854775807\r\n\r\n":                  "GET",
		"HTTP/1.1 304 Not Modified\r\nContent-Length: 5\r\nContent-Length: 5\r\n\r\n":     "GET",
		"HTTP/1.1 204 No Content\r\nTransfer-Encoding: chunked\r\n\r\n":                   "GET",
		"HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\n":                                    "HEAD",
		"HTTP/1.1 103 Early Hints\r\nLink: </a>\r\n\r\nHTTP/1.1 304 Not Modified\r\n\r\n": "GET",
	} {
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		sess, _, closeReq, sendResp, closeResp, _ := newTestSession(t)
		sendResp([]byte(head), time.Now(), time.Now())
		closeResp() // a body it waits for ends at the stream's end
		finalResp := []byte(nil)
		_, err := h.readResponseV2(sess.DestStream, &finalResp, method)
		if refusedFraming(err) {
			t.Errorf("readResponseV2(%q) for a %s: %v, want its framing taken", head, method, err)
		}
		if bodied := !strings.Contains(head, " 304 ") && !strings.Contains(head, " 204 ") && method != "HEAD"; bodied == (err == nil) {
			t.Errorf("readResponseV2(%q) for a %s: %v, want a body read to the stream's end: %v", head, method, err, bodied)
		}
		closeReq()
	}
}

// The legacy record path's response read refuses a no-body answer's framing
// net/http refuses, as readResponseV2 does.
func TestHandleChunkedResponsesRefusesANoBodyAnswersFraming(t *testing.T) {
	for _, head := range []string{
		"HTTP/1.1 304 Not Modified\r\nContent-Length: +5\r\n\r\n",
		"HTTP/1.1 103 Early Hints\r\n\r\nHTTP/1.1 204 No Content\r\nbogus\r\n\r\n",
	} {
		dest, client := newScriptedConn(), newScriptedConn()
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		finalResp := []byte(head)
		done := make(chan error, 1)
		go func() {
			done <- h.handleChunkedResponses(context.Background(), &finalResp, client, dest, []byte(head), "GET")
		}()
		select {
		case err := <-done:
			if !refusedFraming(err) {
				t.Errorf("handleChunkedResponses(%q): %v, want its framing refused", head, err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("handleChunkedResponses(%q) did not return", head)
		}
		_ = dest.Close()
		_ = client.Close()
	}
}

// awaitsContinue is true only for a request net/http's server answers with a
// 100 (Continue): HTTP/1.1 and later HTTP/1.x only (no 1xx to an HTTP/1.0
// client), the first Expect field read as a token list, and a request it
// reads. Each row is checked against a real net/http server. keploy answered
// an HTTP/1.0 client with a 100, read "100-continue, foo" as no expectation
// and a later Expect field as one, and took a request its server refuses.
func TestAwaitsContinueDecidesAsNetHTTPsServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Connection", "close")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	sends100 := func(req string) bool {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(c, req); err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(c) // the server closes after its answer
		if err != nil {
			t.Fatalf("the server's answer to %q: %v", req, err)
		}
		return strings.HasPrefix(string(out), "HTTP/1.1 100 ")
	}
	const cl5 = "Content-Length: 5\r\n\r\nhello"
	for _, row := range []struct {
		req  string // its header section, then its body
		want bool
	}{
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\n" + cl5, true},
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-Continue\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n", true},
		{"POST /u HTTP/1.0\r\nHost: a\r\nExpect: 100-continue\r\n" + cl5, false},
		{"POST /u HTTP/1.2\r\nHost: a\r\nExpect: 100-continue\r\n" + cl5, true},
		{"POST /u HTTP/2.0\r\nHost: a\r\nExpect: 100-continue\r\n" + cl5, false},
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-continue, foo\r\n" + cl5, true},
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect: foo,100-continue\r\n" + cl5, true},
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-continuex\r\n" + cl5, false},
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect: foo\r\nExpect: 100-continue\r\n" + cl5, false},
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nExpect: foo\r\n" + cl5, true},
		{"POST /u HTTP/1.1\r\nHost: a\r\nbogus\r\nExpect: 100-continue\r\n" + cl5, false},
		{"POST /u HTTP/1.1\r\nExpect: 100-continue\r\n" + cl5, false},
		{"POST /u HTTP/1.1\r\nHost: a/b\r\nExpect: 100-continue\r\n" + cl5, false},
		{"POST /u HTTP/1.1\r\nHost: a\r\nX-A : b\r\nExpect: 100-continue\r\n" + cl5, false},
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect : 100-continue\r\n" + cl5, false},
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nContent-Length: 0\r\n\r\n", false},
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nContent-Length: +5\r\n\r\nhello", false},
		{"CONNECT a:443 HTTP/1.1\r\nExpect: 100-continue\r\n" + cl5, true},
		{"POST /u HTTP/2.1\r\nHost: a\r\nExpect: 100-continue\r\n" + cl5, false},
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect: x100-continue\r\n" + cl5, false},
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect: foo\t100-continue\r\n" + cl5, true},
		{"POST /u HTTP/1.1\r\nHost: a\r\nExpect: foo 100-continue\r\n" + cl5, true},
	} {
		head, _, _ := strings.Cut(row.req, "\r\n\r\n")
		head += "\r\n\r\n"
		server := sends100(row.req)
		if server != row.want {
			t.Errorf("net/http's server on %q: a 100 %v, the row says %v", row.req, server, row.want)
		}
		if got := awaitsContinue([]byte(head)); got != server {
			t.Errorf("awaitsContinue(%q) = %v, net/http's server sends a 100: %v", head, got, server)
		}
	}
}

// parseHeaders refuses a start line net/http's reader refuses, and takes one
// it takes: each row is checked against ReadResponse or ReadRequest.
func TestParseHeadersReadsAStartLineAsNetHTTPDoes(t *testing.T) {
	for _, row := range []struct {
		start    string
		response bool
		refused  bool
	}{
		{"HTTP/1.1", true, true},
		{"HTTP/1.1 20 OK", true, true},
		{"HTTP/1.1 2x0 OK", true, true},
		{"HTTP/1.1 -20 OK", true, true},
		{"HTTP/1.1 2000 OK", true, true},
		{"HTTP/1.1 0200 OK", true, true},
		{"HTTP/1.1 1000 OK", true, true},
		{"HTTP/1.10 200 OK", true, true},
		{"HTTP/1.1\t200 OK", true, true},
		{"HTTP/1.1  200 OK", true, false},
		{"HTTP/1.1 +20 OK", true, false},
		{"HTTP/1.1 200", true, false},
		{"POST /a", false, true},
		{"POST /a  HTTP/1.1", false, true},
		{"POST /a HTTP/1.1 x", false, true},
		{"PO(ST /a HTTP/1.1", false, true},
		{"POST a HTTP/1.1", false, true},
		{"POST /a HTTP/1.1", false, false},
		{"OPTIONS * HTTP/1.1", false, false},
		{"POST http://a/b?c HTTP/1.0", false, false},
		{"CONNECT 127.0.0.1:443 HTTP/1.1", false, false},
		{"CONNECT /a HTTP/1.1", false, false},
	} {
		msg := row.start + "\r\nHost: a\r\nContent-Length: 5\r\n\r\n"
		var goErr error
		if row.response {
			_, goErr = http.ReadResponse(bufio.NewReader(strings.NewReader(msg)), nil)
		} else {
			_, goErr = http.ReadRequest(bufio.NewReader(strings.NewReader(msg)))
		}
		if (goErr != nil) != row.refused {
			t.Errorf("net/http on %q: %v, the row says refused %v", row.start, goErr, row.refused)
		}
		cl, _, err := parseHeaders([]byte(msg), row.response)
		if refusedFraming(err) != row.refused || !row.refused && cl != 5 {
			t.Errorf("parseHeaders(%q) = (%d, %v), want refused %v", msg, cl, err, row.refused)
		}
	}
}
