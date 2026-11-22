package http

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

// refusedFraming reports whether err is parseHeaders refusing a message's
// framing.
func refusedFraming(err error) bool {
	return errors.Is(err, errFramingRefused) || errors.Is(err, errBadContentLength)
}

// netHTTPFramingRows are header fields and what net/http makes of them, as
// the response its client reads and as the request its server reads (Host
// after them), each followed by the body "5\r\nhello\r\n0\r\n\r\n": the
// oracle TestParseHeadersFramesOnlyAsNetHTTPFrames holds parseHeaders to, and
// seeds of the fuzz targets. A server of "-" is not asked (its read of the
// body would wait for the client to close).
var netHTTPFramingRows = []struct {
	name, version, fields string
	cl                    int  // -1: no length
	chunked, refused      bool // the framing parseHeaders gives
	client, server        string
}{
	{name: "a signed length", version: "1.1", fields: "Content-Length: +5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a negative length", version: "1.1", fields: "Content-Length: -5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a list of lengths", version: "1.1", fields: "Content-Length: 5, 5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "an empty length", version: "1.1", fields: "Content-Length:\r\nX-A: b\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a length on a line that ends in two CRs", version: "1.1", fields: "Content-Length: 5\r\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "two lengths that differ", version: "1.1", fields: "Content-Length: 5\r\nContent-Length: 6\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "two lengths written differently", version: "1.1", fields: "Content-Length: 5\r\nContent-Length: 05\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a length with white space before its colon", version: "1.1", fields: "Content-Length : 5\r\n",
		refused: true, client: `reads "5\r\nhello\r\n0\r\n\r\n"`, server: "400"},
	{name: "a chunked coding with white space before its colon", version: "1.1", fields: "Transfer-Encoding\t: chunked\r\nContent-Length: 5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a chunked coding with a space before its colon", version: "1.1", fields: "Transfer-Encoding : chunked\r\nContent-Length: 5\r\n",
		refused: true, client: `reads "5\r\nhe"`, server: "400"},
	{name: "chunked among other codings", version: "1.1", fields: "Transfer-Encoding: gzip, chunked\r\n",
		refused: true, client: "refuses", server: "501"},
	{name: "two Transfer-Encoding fields", version: "1.1", fields: "Transfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n",
		refused: true, client: "refuses", server: "501"},
	{name: "a coding that is not chunked", version: "1.1", fields: "Transfer-Encoding: identity\r\nContent-Length: 5\r\n",
		refused: true, client: "refuses", server: "501"},
	{name: "a chunked coding on an HTTP/1.0 message", version: "1.0", fields: "Transfer-Encoding: chunked\r\n",
		refused: true, client: `reads "5\r\nhello\r\n0\r\n\r\n"`, server: `200 ""`},
	{name: "a chunked coding a folded empty line ends in a space", version: "1.1", fields: "Transfer-Encoding: chunked\r\n \r\n",
		refused: true, client: "refuses", server: "501"},
	{name: "a length a folded empty line ends in a space", version: "1.1", fields: "Content-Length: 5\r\n\t\r\n",
		cl: 5, client: `reads "5\r\nhe"`, server: `200 "5\r\nhe"`},
	{name: "two lengths that are the same", version: "1.1", fields: "Content-Length: 5\r\nContent-Length:  5 \r\n",
		cl: 5, client: `reads "5\r\nhe"`, server: `200 "5\r\nhe"`},
	{name: "a length with leading zeros", version: "1.1", fields: "Content-Length: 005\r\n",
		cl: 5, client: `reads "5\r\nhe"`, server: `200 "5\r\nhe"`},
	{name: "a length with a leading zero, read in base 10", version: "1.1", fields: "Content-Length: 010\r\n",
		cl: 10, client: `reads "5\r\nhello\r\n"`, server: `200 "5\r\nhello\r\n"`},
	{name: "a length written in hex", version: "1.1", fields: "Content-Length: 0x5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a length written in octal", version: "1.1", fields: "Content-Length: 0o5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a length written in binary", version: "1.1", fields: "Content-Length: 0b101\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a length with an underscore between its digits", version: "1.1", fields: "Content-Length: 1_0\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a length on an HTTP/1.0 message", version: "1.0", fields: "Content-Length: 5\r\n",
		cl: 5, client: `reads "5\r\nhe"`, server: `200 "5\r\nhe"`},
	{name: "chunked in another case, white space around it", version: "1.1", fields: "Transfer-Encoding:  Chunked \t\r\n",
		cl: -1, chunked: true, client: `reads "hello"`, server: `200 "hello"`},
	{name: "a chunked coding beside a length", version: "1.1", fields: "Content-Length: 5\r\nTransfer-Encoding: chunked\r\n",
		cl: -1, chunked: true, client: `reads "hello"`, server: `200 "hello"`},
	{name: "white space before the colon of a field that frames nothing", version: "1.1", fields: "X-A : b\r\nContent-Length: 5\r\n",
		cl: 5, client: `reads "5\r\nhe"`, server: "400"},
	{name: "a length of 2^63", version: "1.1", fields: "Content-Length: 9223372036854775808\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a length of 2^64-1", version: "1.1", fields: "Content-Length: 18446744073709551615\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a length of 2^63-1", version: "1.1", fields: "Content-Length: 9223372036854775807\r\n",
		cl: 9223372036854775807, client: "refuses the body: unexpected EOF", server: "-"},
	{name: "a line with no colon", version: "1.1", fields: "bogus\r\nContent-Length: 5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a folded line right after the start line", version: "1.1", fields: " X-A: b\r\nContent-Length: 5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a control byte in a value", version: "1.1", fields: "X-A: b\x01c\r\nContent-Length: 5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a DEL in a value", version: "1.1", fields: "X-A: b\x7f\r\nContent-Length: 5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a CR inside a line", version: "1.1", fields: "X-A: b\rc\r\nContent-Length: 5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a byte past ASCII in a value", version: "1.1", fields: "X-A: b\xe9\r\nContent-Length: 5\r\n",
		cl: 5, client: `reads "5\r\nhe"`, server: `200 "5\r\nhe"`},
	{name: "a name byte that is not a token byte", version: "1.1", fields: "X(A: b\r\nContent-Length: 5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "an empty name", version: "1.1", fields: ": b\r\nContent-Length: 5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a tab before the colon of a field that frames nothing", version: "1.1", fields: "X-A\t: b\r\nContent-Length: 5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a bare LF empty line ends the section", version: "1.1", fields: "X-A: b\n\nContent-Length: 5\r\n",
		cl: -1, client: `reads "Content-Length: 5\r\n\r\n5\r\nhello\r\n0\r\n\r\n"`, server: "400"},
	{name: "a Trailer that names a framing field", version: "1.1", fields: "Transfer-Encoding: chunked\r\nTrailer: X-A, content-length\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a Trailer that names the Trailer", version: "1.1", fields: "Transfer-Encoding: chunked\r\nTrailer: Trailer\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a Trailer that names the Transfer-Encoding", version: "1.1", fields: "Trailer: transfer-encoding \r\nTransfer-Encoding: chunked\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a Trailer that names other fields", version: "1.1", fields: "Transfer-Encoding: chunked\r\nTrailer: X-A, X-B\r\n",
		cl: -1, chunked: true, client: `reads "hello"`, server: `200 "hello"`},
	{name: "a Trailer that names a framing field beside a length", version: "1.1", fields: "Content-Length: 5\r\nTrailer: Content-Length\r\n",
		cl: 5, client: `reads "5\r\nhe"`, server: `200 "5\r\nhe"`},
	{name: "chunked written with a Kelvin sign", version: "1.1", fields: "Transfer-Encoding: chun\u212aed\r\n",
		refused: true, client: "refuses", server: "501"},
	{name: "a name byte past ASCII", version: "1.1", fields: "X-\u212a: b\r\nContent-Length: 5\r\n",
		refused: true, client: "refuses", server: "400"},
	{name: "a Trailer that names a framing field with a long s", version: "1.1", fields: "Transfer-Encoding: chunked\r\nTrailer: tran\u017ffer-encoding\r\n",
		cl: -1, chunked: true, client: `reads "hello"`, server: `200 "hello"`},
	{name: "a chunked coding on an HTTP/0.9 message", version: "0.9", fields: "Transfer-Encoding: chunked\r\n",
		refused: true, client: `reads "5\r\nhello\r\n0\r\n\r\n"`, server: "505"},
	{name: "a chunked coding on an HTTP/0.0 message", version: "0.0", fields: "Transfer-Encoding: chunked\r\n",
		cl: -1, chunked: true, client: `reads "hello"`, server: "505"},
	{name: "a chunked coding on an HTTP/1.2 message", version: "1.2", fields: "Transfer-Encoding: chunked\r\n",
		cl: -1, chunked: true, client: `reads "hello"`, server: `200 "hello"`},
	{name: "a chunked coding on an HTTP/2.0 message", version: "2.0", fields: "Transfer-Encoding: chunked\r\n",
		cl: -1, chunked: true, client: `reads "hello"`, server: "505"},
	{name: "a version net/http does not read", version: "1.10", fields: "Content-Length: 5\r\n",
		refused: true, client: "refuses", server: "400"},
}

// parseHeaders frames a message only as net/http's reader frames it, and
// refuses the framing of one net/http refuses or reads otherwise. Each row
// holds what net/http makes of the same fields, as the response its client
// reads and as the request its server reads: the oracle. keploy took the last
// of two lengths, "+5" as 5 and "-5" as a body that ended before it began, a
// framing field written with white space before its colon as that field, any
// Transfer-Encoding naming chunked as chunked, and one on an HTTP/1.0 message
// too.
func TestParseHeadersFramesOnlyAsNetHTTPFrames(t *testing.T) {
	const body = "5\r\nhello\r\n0\r\n\r\n"
	srv := newEchoServer(t)
	for _, row := range netHTTPFramingRows {
		t.Run(row.name, func(t *testing.T) {
			resp := "HTTP/" + row.version + " 200 OK\r\n" + row.fields + "\r\n"
			req := "POST /a HTTP/" + row.version + "\r\n" + row.fields + "Host: up\r\n\r\n"
			for i, msg := range []string{resp, req} {
				// The header section messageHead finds, as readRequestV2 and
				// readResponseWrites read it.
				section, _, _, ok := messageHead([]byte(msg+body), i == 0)
				if !ok {
					t.Fatalf("messageHead(%q) found no header section", msg+body)
				}
				head := string(section)
				cl, chunked, err := parseHeaders(section, i == 0)
				if refusedFraming(err) != row.refused || err != nil && !refusedFraming(err) {
					t.Fatalf("parseHeaders(%q): %v, want refused %v", head, err, row.refused)
				}
				if !row.refused && (cl != row.cl || chunked != row.chunked) {
					t.Errorf("parseHeaders(%q) = (%d, %v), want (%d, %v)", head, cl, chunked, row.cl, row.chunked)
				}
				if row.refused && (cl != -1 || chunked) {
					t.Errorf("parseHeaders(%q) = (%d, %v) with its refusal, want no framing", head, cl, chunked)
				}
			}
			if got := goClientReads(resp + body); got != row.client {
				t.Errorf("net/http's client on %q: %s, the row says %s", resp, got, row.client)
			}
			if row.server == "-" {
				return
			}
			if got := srv.reads(t, req+body); got != row.server {
				t.Errorf("net/http's server on %q: %s, the row says %s", req, got, row.server)
			}
		})
	}
}

// goClientReads is what net/http's client makes of resp, the bytes a server
// sent before it closed the connection.
func goClientReads(resp string) string {
	r, err := http.ReadResponse(bufio.NewReader(strings.NewReader(resp)), nil)
	if err != nil {
		return "refuses"
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return "refuses the body: " + err.Error()
	}
	return fmt.Sprintf("reads %q", b)
}

type echoServer struct{ addr string }

// newEchoServer is a net/http server that answers each request with its body.
func newEchoServer(t *testing.T) *echoServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, _ = w.Write(b)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &echoServer{addr: ln.Addr().String()}
}

// reads is what the server answers to req: its status, and the body it read
// when it took the request.
func (s *echoServer) reads(t *testing.T, req string) string {
	c, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("the server's answer: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprint(resp.StatusCode)
	}
	return fmt.Sprintf("%d %q", resp.StatusCode, b)
}

// recordV2 records no exchange whose framing net/http refuses or reads
// otherwise: it stops on it with a decode error, and the connection is passed
// through whole. keploy recorded the response by a length net/http's client
// does not read, and the request by one net/http's server refuses.
func TestRecordV2PassesThroughAFramingNetHTTPReadsOtherwise(t *testing.T) {
	for _, row := range []struct {
		name      string
		req, resp []string
	}{
		{name: "a response length with a space before its colon",
			req:  []string{"GET /a HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 200 OK\r\nContent-Length : 5\r\n\r\n", "hello world"}},
		{name: "a chunked HTTP/1.0 response",
			req:  []string{"GET /a HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.0 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n", "5\r\nhello\r\n0\r\n\r\n"}},
		{name: "a request length with a space before its colon",
			req:  []string{"POST /a HTTP/1.1\r\nHost: up\r\nContent-Length : 3\r\n\r\nabc"},
			resp: []string{"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"}},
		{name: "a request with two codings",
			req:  []string{"POST /a HTTP/1.1\r\nHost: up\r\nTransfer-Encoding: gzip, chunked\r\n\r\n", "3\r\nabc\r\n0\r\n\r\n"},
			resp: []string{"HTTP/1.1 501 Not Implemented\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"}},
		{name: "a 304 with two lengths that differ",
			req:  []string{"GET /a HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 304 Not Modified\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n"}},
		{name: "a response with a line with no colon",
			req:  []string{"GET /a HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 200 OK\r\nbogus\r\nContent-Length: 5\r\n\r\n", "hello"}},
		{name: "a final response with a control byte behind an interim",
			req:  []string{"GET /a HTTP/1.1\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 103 Early Hints\r\nLink: </a>\r\n\r\nHTTP/1.1 200 OK\r\nX-A: b\x01\r\nContent-Length: 5\r\n\r\n", "hello"}},
		{name: "a request with a control byte",
			req:  []string{"POST /a HTTP/1.1\r\nHost: up\r\nX-A: \x01\r\nContent-Length: 3\r\n\r\nabc"},
			resp: []string{"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"}},
		{name: "a request with two Host fields",
			req:  []string{"GET /a HTTP/1.1\r\nHost: up\r\nHost: up\r\n\r\n"},
			resp: []string{"HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := &HTTP{Logger: zaptest.NewLogger(t)}
			sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
			now := time.Now()
			for _, b := range row.req {
				sendReq([]byte(b), now, now)
			}
			for _, b := range row.resp {
				sendResp([]byte(b), now, now)
			}
			closeReq()
			closeResp()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := h.recordV2(ctx, sess); !refusedFraming(err) {
				t.Fatalf("recordV2: %v, want its framing refused", err)
			}
			close(mocks)
			for m := range mocks {
				t.Errorf("recorded %s %s -> %d %q, want nothing", m.Spec.HTTPReq.Method, m.Spec.HTTPReq.URL, m.Spec.HTTPResp.StatusCode, m.Spec.HTTPResp.Body)
			}
		})
	}
}

// A response read refuses the framing from the head alone: it waits for no
// body it would frame where its peer does not. keploy waited for the larger
// of two lengths, and for chunks of a coding net/http refuses.
func TestReadResponseRefusesAFramingBeforeItsBody(t *testing.T) {
	for head, method := range map[string]string{
		"HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Length: 100\r\n\r\n":                   "GET",
		"HTTP/1.1 200 OK\r\nContent-Length: +5\r\n\r\n":                                         "GET",
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip, chunked\r\n\r\n":                           "GET",
		"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTransfer-Encoding: chunked\r\n\r\n":   "GET",
		"HTTP/1.1 200 OK\r\nContent-Length: 9223372036854775808\r\n\r\n":                        "GET",
		"HTTP/1.1 200 OK\r\nContent-Length: 18446744073709551615\r\n\r\n":                       "GET",
		"HTTP/1.1 200 OK\r\nbogus\r\nContent-Length: 5\r\n\r\n":                                 "GET",
		"HTTP/1.1 304 Not Modified\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n":           "GET",
		"HTTP/1.1 304 Not Modified\r\nTransfer-Encoding: gzip, chunked\r\n\r\n":                 "GET",
		"HTTP/1.1 204 No Content\r\nContent-Length: +5\r\n\r\n":                                 "GET",
		"HTTP/1.1 200 OK\r\nContent-Length: -1\r\n\r\n":                                         "HEAD",
		"HTTP/1.1 103 Early Hints\r\n\r\nHTTP/1.1 200 OK\r\nbogus\r\nContent-Length: 5\r\n\r\n": "GET",
	} {
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		sess, _, closeReq, sendResp, closeResp, _ := newTestSession(t)
		sendResp([]byte(head), time.Now(), time.Now())
		finalResp := []byte(nil)
		done := make(chan error, 1)
		go func() {
			_, err := h.readResponseV2(sess.DestStream, &finalResp, method)
			done <- err
		}()
		select {
		case err := <-done:
			if !refusedFraming(err) {
				t.Errorf("readResponseV2(%q) for a %s: %v, want its framing refused", head, method, err)
			}
		case <-time.After(time.Second):
			t.Errorf("readResponseV2(%q) is waiting for a body", head)
			closeResp()
			<-done
		}
		closeReq()
		closeResp()
	}
}

// The other readers of a message's framing refuse it the same way: the replay
// proxy's (and the legacy record path's) read of the app's request, and the
// legacy record path's read of the destination's response.
func TestChunkedReadersRefuseAFramingNetHTTPReadsOtherwise(t *testing.T) {
	t.Run("replay request", func(t *testing.T) {
		const req = "POST /a HTTP/1.1\r\nHost: up\r\nContent-Length: +3\r\n\r\n"
		if _, err := runChunkedRequest(t, req, newScriptedConn()); !refusedFraming(err) {
			t.Fatalf("HandleChunkedRequests: %v, want its framing refused", err)
		}
	})
	t.Run("record response", func(t *testing.T) {
		head := "HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip, chunked\r\n\r\n3\r\nabc"
		dest := newScriptedConn(readStep{data: []byte("\r\n0\r\n\r\n")})
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
			if !refusedFraming(err) {
				t.Fatalf("handleChunkedResponses: %v, want its framing refused", err)
			}
		case <-time.After(time.Second):
			t.Fatal("handleChunkedResponses did not return")
		}
	})
}

// A client that sent "Expect: 100-continue" with a framing net/http refuses is
// not waiting for a 100: net/http's server answers it with an error, not a
// 100.
func TestAwaitsContinueReadsNoFramingNetHTTPRefuses(t *testing.T) {
	for req, want := range map[string]bool{
		"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nContent-Length: 5\r\n\r\n":                      true,
		"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nTransfer-Encoding: chunked\r\n\r\n":             true,
		"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nContent-Length: +5\r\n\r\n":                     false,
		"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nTransfer-Encoding: gzip, chunked\r\n\r\n":       false,
		"POST /u HTTP/1.1\r\nHost: a\r\nExpect: 100-continue\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n": false,
	} {
		if got := awaitsContinue([]byte(req)); got != want {
			t.Errorf("awaitsContinue(%q) = %v, want %v", req, got, want)
		}
	}
}

// A Content-Length of 0 is a length: the response ends at its head, and the
// next one on the connection is its own exchange.
func TestRecordV2FramesAZeroLengthAsALength(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
	now := time.Now()
	sendReq([]byte("GET /a HTTP/1.1\r\nHost: up\r\n\r\n"), now, now)
	sendResp([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"), now, now)
	sendReq([]byte("GET /b HTTP/1.1\r\nHost: up\r\n\r\n"), now, now)
	sendResp([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nworld"), now, now)
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
	if want := "GET /a -> 200 \"\"\nGET /b -> 200 \"world\""; strings.Join(got, "\n") != want {
		t.Fatalf("recorded\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.ReplaceAll(want, "\n", "\n  "))
	}
}

// contentLengthValue takes every length an int holds, math.MaxInt the largest,
// and refuses the one past it and all those past that, on 64-bit and 32-bit
// alike. Its error names the value by its length only.
func TestContentLengthValueTakesAnIntAndNothingPastIt(t *testing.T) {
	for _, c := range []struct {
		v    string
		want int
		ok   bool
	}{
		{v: "0", ok: true},
		{v: "5", want: 5, ok: true},
		{v: strconv.Itoa(math.MaxInt), want: math.MaxInt, ok: true},
		{v: strconv.FormatUint(uint64(math.MaxInt)+1, 10)},
		{v: "18446744073709551615"}, // the largest uint64
		{v: "18446744073709551616"}, // past a uint64
		{v: "+5"},
		{v: "-5"},
		{v: ""},
	} {
		n, err := contentLengthValue(c.v)
		if c.ok {
			if err != nil || n != c.want {
				t.Errorf("contentLengthValue(%q) = (%d, %v), want (%d, <nil>)", c.v, n, err, c.want)
			}
			continue
		}
		if !errors.Is(err, errBadContentLength) || n != 0 {
			t.Errorf("contentLengthValue(%q) = (%d, %v), want (0, %v)", c.v, n, err, errBadContentLength)
			continue
		}
		if want := fmt.Sprintf("(a %d-byte value)", len(c.v)); !strings.HasSuffix(err.Error(), want) {
			t.Errorf("contentLengthValue(%q): error %q does not end %q", c.v, err, want)
		}
		if c.v != "" && strings.Contains(err.Error(), c.v) {
			t.Errorf("contentLengthValue(%q): error %q names the value", c.v, err)
		}
	}
}
