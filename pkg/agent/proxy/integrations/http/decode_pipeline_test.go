package http

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The three requests of the replay pipeline rows: one with no body, one with
// a Content-Length, one chunked, each answered by its own mock.
const (
	pipeGetA     = "GET /a HTTP/1.1\r\nHost: api.example\r\n\r\n"
	pipePostB    = "POST /b HTTP/1.1\r\nHost: api.example\r\nContent-Length: 5\r\n\r\nhello"
	pipeChunkedC = "POST /c HTTP/1.1\r\nHost: api.example\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nworld\r\n0\r\n\r\n"
)

// readAnswers reads n HTTP answers from the app's end of the connection, in
// order, to requests of the given methods, and returns their bodies. The
// bound only turns a replay that never answers into a failure.
func readAnswers(t *testing.T, app net.Conn, methods ...string) []string {
	t.Helper()
	bodies, _ := readAnswersOn(t, app, methods...)
	return bodies
}

// readEveryAnswer is readAnswers on a connection the proxy has closed: no
// byte follows the last answer.
func readEveryAnswer(t *testing.T, app net.Conn, methods ...string) []string {
	t.Helper()
	bodies, br := readAnswersOn(t, app, methods...)
	if extra, _ := io.ReadAll(br); len(extra) > 0 {
		t.Fatalf("after its %d answers %q the app was sent %q, want nothing", len(methods), bodies, extra)
	}
	return bodies
}

// readAnswersOn is readAnswers, and returns the reader it read them on.
func readAnswersOn(t *testing.T, app net.Conn, methods ...string) ([]string, *bufio.Reader) {
	t.Helper()
	_ = app.SetReadDeadline(time.Now().Add(time.Minute))
	br := bufio.NewReader(app)
	var bodies []string
	for i, m := range methods {
		resp, err := http.ReadResponse(br, &http.Request{Method: m})
		if err != nil {
			t.Fatalf("answer %d of %d: %v (answers before it: %q)", i+1, len(methods), err, bodies)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("answer %d of %d: its body: %v", i+1, len(methods), err)
		}
		if resp.StatusCode/100 != 2 {
			t.Fatalf("answer %d of %d is a %d (%q), want a 2xx", i+1, len(methods), resp.StatusCode, body)
		}
		bodies = append(bodies, string(body))
	}
	return bodies, br
}

// An HTTP/1.1 client may pipeline its requests: send the next before the
// answer to the one before has come, often in the same write. Replay took
// all the bytes it had read as one request, answered the first, and dropped
// the rest, then waited for the client's next bytes while the client waited
// for the answers to the requests it had sent: it hung. Each row sends the
// three requests cut a different way; each request gets its own mock's
// answer, in order.
func TestDecodeHTTPAnswersEveryPipelinedRequest(t *testing.T) {
	a := httpMock("a", "GET", "/a")
	a.Spec.HTTPReq.Header = map[string]string{"Host": "api.example"}
	a.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: `{"a":1}`}
	b := recorded("b", "/b", nil, "hello")
	b.Spec.HTTPResp.Body = `{"b":2}`
	c := recorded("c", "/c", nil, "world")
	c.Spec.HTTPResp.Body = `{"c":3}`
	for _, m := range []*models.Mock{a, b, c} {
		m.Spec.HTTPReq.ProtoMajor, m.Spec.HTTPReq.ProtoMinor = 1, 1
		// As a server sends it: the client reads the answer by its length.
		m.Spec.HTTPResp.Header = map[string]string{"Content-Length": strconv.Itoa(len(m.Spec.HTTPResp.Body))}
	}
	answer := map[string]string{pipeGetA: `{"a":1}`, pipePostB: `{"b":2}`, pipeChunkedC: `{"c":3}`}
	method := map[string]string{pipeGetA: "GET", pipePostB: "POST", pipeChunkedC: "POST"}
	headOfB := strings.Index(pipePostB, "Host")
	headOfC := strings.Index(pipeChunkedC, "\r\n\r\n") + 4
	for _, tc := range []struct {
		name     string
		requests []string // in the order the client sends them
		first    string   // the proxy's first read
		then     string   // what the client wrote after it, in one write
	}{
		{
			name:     "all three in the first read",
			requests: []string{pipeGetA, pipePostB, pipeChunkedC},
			first:    pipeGetA + pipePostB + pipeChunkedC,
		},
		{
			name:     "the first read ends inside the second request's head",
			requests: []string{pipeGetA, pipePostB, pipeChunkedC},
			first:    pipeGetA + pipePostB[:headOfB],
			then:     pipePostB[headOfB:] + pipeChunkedC,
		},
		{
			name:     "the second and third in one write after the first",
			requests: []string{pipeGetA, pipePostB, pipeChunkedC},
			first:    pipeGetA,
			then:     pipePostB + pipeChunkedC,
		},
		{
			name:     "a chunked body's last read holds the next requests",
			requests: []string{pipeChunkedC, pipeGetA, pipePostB},
			first:    pipeChunkedC[:headOfC],
			then:     pipeChunkedC[headOfC:] + pipeGetA + pipePostB,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, proxy := tcpPair(t)
			h := &HTTP{Logger: zap.NewNop()}
			runDecode(t, proxy, func(ctx context.Context) error {
				return h.decodeHTTP(ctx, []byte(tc.first), proxy, nil,
					&mockMemDb{mocks: []*models.Mock{a, b, c}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
			})
			var methods, want []string
			for _, r := range tc.requests {
				methods = append(methods, method[r])
				want = append(want, answer[r])
			}
			if _, err := app.Write([]byte(tc.then)); err != nil {
				t.Fatal(err)
			}
			got := readAnswers(t, app, methods...)
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("the pipelined requests were answered %q, want %q", got, want)
				}
			}
		})
	}
}

// pipeDep is a dependency that answers one call per connection with
// "real <path>", and sends on each connection's end what the connection
// held after that call: nothing, when the proxy sent it one call only.
func pipeDep(t *testing.T) (addr string, port uint, after <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	out := make(chan string, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					out <- "unreadable: " + err.Error()
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				body := "real " + req.URL.Path
				_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body))
				// The proxy closes the connection once it has the answer.
				rest, _ := io.ReadAll(br)
				out <- string(rest)
			}(c)
		}
	}()
	tcpAddr := ln.Addr().(*net.TCPAddr)
	return tcpAddr.String(), uint(tcpAddr.Port), out
}

// Every path of a replay that answers a call and keeps the connection takes
// the client's next request from what it pipelined after the call: a call a
// bypassRule passes through, one a pass-through egress rule answers, an async
// lane's keep-alive, a miss passed through on-miss. A call passed to its
// dependency is that call alone: the dependency is not sent the next request
// with it.
func TestDecodeHTTPAnswersPipelinedRequestsOnEachPath(t *testing.T) {
	depAddr, depPort, depAfter := pipeDep(t)
	for _, tc := range keepAlivePaths(depAddr, depPort) {
		t.Run(tc.name, func(t *testing.T) {
			app, proxy := tcpPair(t)
			h := &HTTP{Logger: zap.NewNop()}
			if tc.setup != nil {
				tc.setup(t, h)
			}
			runDecode(t, proxy, func(ctx context.Context) error {
				return h.decodeHTTP(ctx, []byte(tc.call+tc.call), proxy, tc.dst, &mockMemDb{}, tc.opts)
			})
			got := readAnswers(t, app, tc.method, tc.method)
			if tc.want != "" && (got[0] != tc.want || got[1] != tc.want) {
				t.Fatalf("the two pipelined calls were answered %q, want %q each", got, tc.want)
			}
			if tc.dep {
				eachCallAlone(t, depAfter, 2)
			}
		})
	}
}

// keepAlivePath is a path of a replay that answers a call and keeps the
// connection, other than a call its mock answers: a call that takes it, and
// how the replay is set up for it.
type keepAlivePath struct {
	name   string
	call   string
	method string
	dst    *models.ConditionalDstCfg
	opts   models.OutgoingOptions
	setup  func(t *testing.T, h *HTTP)
	// want is each call's answer; dep, that the calls went to the dependency.
	want string
	dep  bool
}

// keepAlivePaths are those paths, their dependency (pipeDep) at depAddr.
func keepAlivePaths(depAddr string, depPort uint) []keepAlivePath {
	return []keepAlivePath{
		{
			name:   "a call a bypassRule passes through",
			call:   "GET /a HTTP/1.1\r\nHost: dep.internal\r\n\r\n",
			method: "GET",
			dst:    &models.ConditionalDstCfg{Addr: depAddr, Port: depPort},
			opts:   models.OutgoingOptions{Rules: []models.BypassRule{{Host: `^dep\.internal$`}}},
			want:   "real /a",
			dep:    true,
		},
		{
			name:   "a call a pass-through egress rule answers",
			call:   "POST /x HTTP/1.1\r\nHost: app\r\nContent-Length: 2\r\n\r\nhi",
			method: "POST",
			dst:    &models.ConditionalDstCfg{Addr: "127.0.0.1:9999", Port: 9999},
			opts:   models.OutgoingOptions{PassThroughPorts: []models.PassThroughRule{{Port: 9999, Mode: models.PassThroughSkip}}},
			setup: func(t *testing.T, _ *HTTP) {
				t.Setenv(provenswitch.Env, "1") // the switch reads the user's rules
			},
		},
		{
			name:   "an async lane's keep-alive",
			call:   "GET /w HTTP/1.1\r\nHost: cfg.local\r\n\r\n",
			method: "GET",
			setup: func(t *testing.T, h *HTTP) {
				h.SetAsyncEngine(async.NewEngine(zap.NewNop(),
					[]models.AsyncLane{{Name: "w", Type: "http", Match: map[string]string{"pathRegex": "^/w$"}}},
					map[string]async.AsyncParser{"http": h}))
			},
		},
		{
			name:   "a miss passed through on-miss",
			call:   "GET /m HTTP/1.1\r\nHost: dep.local\r\n\r\n",
			method: "GET",
			dst:    &models.ConditionalDstCfg{Addr: depAddr, Port: depPort},
			opts:   models.OutgoingOptions{OnMiss: models.MissPassthrough},
			want:   "real /m",
			dep:    true,
		},
	}
}

// eachCallAlone checks that the dependency's next n connections (pipeDep)
// were each sent one call: nothing after the call it answered.
func eachCallAlone(t *testing.T, depAfter <-chan string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case rest := <-depAfter:
			if rest != "" {
				t.Fatalf("the dependency was sent %q after the call it answered, want each call on its own", rest)
			}
		case <-time.After(time.Minute): // the bound only turns a hang into a failure
			t.Fatalf("the dependency's connection %d of %d did not end", i+1, n)
		}
	}
}

// A request whose Content-Length is negative has no length to frame it by: a
// decode error that ends the connection, as net/http's refusal of it was.
func TestDecodeHTTPRefusesANegativeContentLength(t *testing.T) {
	_, proxy := tcpPair(t)
	h := &HTTP{Logger: zap.NewNop()}
	done := runDecode(t, proxy, func(ctx context.Context) error {
		return h.decodeHTTP(ctx, []byte("POST /n HTTP/1.1\r\nHost: api.example\r\nContent-Length: -5\r\n\r\nhello"), proxy, nil, &mockMemDb{}, models.OutgoingOptions{})
	})
	select {
	case err := <-done:
		if !errors.Is(err, errBadContentLength) {
			t.Fatalf("decodeHTTP = %v, want a bad Content-Length (errBadContentLength)", err)
		}
	case <-time.After(time.Minute): // the bound only turns a hang into a failure
		t.Fatal("decodeHTTP did not return")
	}
}

// An old client sends a CRLF after a POST body (RFC 9112 §2.2 lets a server
// ignore empty lines before a request line), and a client may send empty
// lines between requests. They are no part of a request: replay drops them,
// as the recorder does (nextRequestV2), and reads the connection on, so a
// client that closes after them ends the connection as any keep-alive close
// does, with nothing logged as an error. Kept as the next request's start,
// they held replay waiting for a request head, and the close was logged as
// "failed to read the request message from the client: EOF".
func TestDecodeHTTPDropsEmptyLinesAfterARequest(t *testing.T) {
	const post = "POST /p HTTP/1.1\r\nHost: api.example\r\nContent-Length: 2\r\n\r\nhi"
	m := recorded("p", "/p", nil, "hi")
	m.Spec.HTTPReq.ProtoMajor, m.Spec.HTTPReq.ProtoMinor = 1, 1
	m.Spec.HTTPResp.Header = map[string]string{"Content-Length": strconv.Itoa(len(m.Spec.HTTPResp.Body))}
	for _, tc := range []struct {
		name    string
		first   string // the proxy's first read
		then    string // what the client writes after it, before its answers
		after   string // what it writes once answered, before it closes
		answers int
	}{
		{name: "a CRLF after the body, then close", first: post + "\r\n", answers: 1},
		{name: "an LF after the body, then close", first: post + "\n", answers: 1},
		{name: "two CRLFs after the body, then close", first: post + "\r\n\r\n", answers: 1},
		{name: "a CRLF in its own write once answered, then close", first: post, after: "\r\n", answers: 1},
		{name: "a CRLF after the body, then the next request", first: post + "\r\n", then: post, answers: 2},
		{name: "a CRLF and the next request in one read", first: post + "\r\n" + post, answers: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app, proxy := tcpPair(t)
			core, logs := observer.New(zapcore.DebugLevel)
			h := &HTTP{Logger: zap.New(core)}
			done := runDecode(t, proxy, func(ctx context.Context) error {
				return h.decodeHTTP(ctx, []byte(tc.first), proxy, nil,
					&mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
			})
			if _, err := app.Write([]byte(tc.then)); err != nil {
				t.Fatal(err)
			}
			methods := make([]string, tc.answers)
			for i := range methods {
				methods[i] = "POST"
			}
			readAnswers(t, app, methods...)
			if _, err := app.Write([]byte(tc.after)); err != nil {
				t.Fatal(err)
			}
			_ = app.Close()
			select {
			case err := <-done:
				var errs []string
				for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
					errs = append(errs, e.Message+": "+fmt.Sprint(e.ContextMap()["error"]))
				}
				if err != nil || len(errs) > 0 {
					t.Fatalf("decodeHTTP = %v, error logs %q; want nil and none: the client closed once answered", err, errs)
				}
			case <-time.After(time.Minute): // the bound only turns a hang into a failure
				t.Fatal("decodeHTTP did not return")
			}
		})
	}
}

// scriptedClient is the proxy's end of a connection (a TCP one, which takes
// the answers) whose reader gives steps: each step's bytes, split across
// Reads as their buffers take them, the last of them with the step's error,
// as crypto/tls gives a TLS 1.2 client's last bytes with the io.EOF of its
// close_notify. With no step left a Read gives io.EOF. A Read after an
// io.EOF has been given gives io.EOF again, and is counted: a replay that
// honours the client's end reads no more.
type scriptedClient struct {
	net.Conn
	mu       sync.Mutex
	steps    []readStep
	ended    bool
	afterEOF int
}

func (c *scriptedClient) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		c.afterEOF++
		return 0, io.EOF
	}
	if len(c.steps) == 0 {
		c.ended = true
		return 0, io.EOF
	}
	st := &c.steps[0]
	n := copy(p, st.data)
	if st.data = st.data[n:]; len(st.data) > 0 {
		return n, nil
	}
	err := st.err
	c.steps = c.steps[1:]
	c.ended = errors.Is(err, io.EOF)
	return n, err
}

func (c *scriptedClient) readsAfterEOF() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.afterEOF
}

// errorLogs are the messages, with their errors, of logs' error entries.
func errorLogs(logs *observer.ObservedLogs) []string {
	var errs []string
	for _, e := range logs.FilterLevelExact(zapcore.ErrorLevel).All() {
		errs = append(errs, e.Message+": "+fmt.Sprint(e.ContextMap()["error"]))
	}
	return errs
}

// The calls of the read-with-its-error rows: a POST answered by mock p, and
// one with a body over a small read (pUtil.WaitReadSize), answered by mock
// big.
const (
	eofPost = "POST /p HTTP/1.1\r\nHost: api.example\r\nContent-Length: 2\r\n\r\nhi"
	bigBody = 3000
)

var eofBigPost = "POST /big HTTP/1.1\r\nHost: api.example\r\nContent-Length: " + strconv.Itoa(bigBody) + "\r\n\r\n" + strings.Repeat("x", bigBody)

// eofMocks are mocks p and big, each answered with a Content-Length.
func eofMocks() []*models.Mock {
	p := recorded("p", "/p", nil, "hi")
	big := recorded("big", "/big", nil, strings.Repeat("x", bigBody))
	for _, m := range []*models.Mock{p, big} {
		m.Spec.HTTPReq.ProtoMajor, m.Spec.HTTPReq.ProtoMinor = 1, 1
		m.Spec.HTTPResp.Header = map[string]string{"Content-Length": strconv.Itoa(len(m.Spec.HTTPResp.Body))}
	}
	return []*models.Mock{p, big}
}

// A reader may give the client's last bytes in the same Read as the error
// that ends the connection: crypto/tls gives a TLS 1.2 client's last request
// with the io.EOF of the close_notify behind it. Replay serves what those
// bytes hold, wherever the read falls (the next request's start, the rest of
// a body over a small read, the end of the first request's body or head),
// then ends the connection without reading it again, on each path that
// answers a call and keeps the connection. Each reader of a request ended
// the connection at the error and dropped the bytes: the request went
// unanswered.
func TestDecodeHTTPServesARequestReadWithItsEOF(t *testing.T) {
	depAddr, depPort, depAfter := pipeDep(t)
	type row struct {
		keepAlivePath
		first   string     // the proxy's first read (call when empty)
		steps   []readStep // what the client's reads give after it
		answers int        // the calls answered
		errLog  string     // the one error logged, if any
	}
	matched := func(name string) keepAlivePath {
		return keepAlivePath{name: name, call: eofPost, method: "POST"}
	}
	eof := func(b string) []readStep { return []readStep{{data: []byte(b), err: io.EOF}} }
	cut := len(eofPost) - 1
	rows := []row{
		{keepAlivePath: matched("a call read with its EOF"), steps: eof(eofPost), answers: 2},
		{keepAlivePath: matched("two calls read with their EOF"), steps: eof(eofPost + eofPost), answers: 3},
		{keepAlivePath: matched("an empty line read with the EOF"), steps: eof("\r\n"), answers: 1},
		{keepAlivePath: matched("a call over a small read, its body's end read with its EOF"), steps: eof(eofBigPost), answers: 2},
		{keepAlivePath: matched("the first call's body ends with the EOF"), first: eofPost[:cut], steps: eof(eofPost[cut:]), answers: 1},
		{keepAlivePath: matched("the first call's head ends with the EOF"), first: eofPost[:10], steps: eof(eofPost[10:]), answers: 1},
		// A timeout ends only its read: the body's end it came with is
		// served, and the client's next call read after it.
		{keepAlivePath: matched("a body's end read with a timeout, then the next call"), first: eofPost[:cut],
			steps: []readStep{{data: []byte(eofPost[cut:]), err: os.ErrDeadlineExceeded}, {data: []byte(eofPost)}}, answers: 2},
		// A request cut by the EOF is no request: the one before it is
		// answered, and the cut one is logged, without another read.
		{keepAlivePath: matched("part of a call read with the EOF"), steps: eof(eofPost + eofPost[:10]), answers: 2,
			errLog: "failed to read the request message from the client: EOF"},
	}
	for _, p := range keepAlivePaths(depAddr, depPort) {
		rows = append(rows, row{keepAlivePath: p, steps: eof(p.call), answers: 2})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			app, proxy := tcpPair(t)
			client := &scriptedClient{Conn: proxy, steps: tc.steps}
			core, logs := observer.New(zapcore.DebugLevel)
			h := &HTTP{Logger: zap.New(core)}
			if tc.setup != nil {
				tc.setup(t, h)
			}
			first := tc.first
			if first == "" {
				first = tc.call
			}
			done := runDecode(t, proxy, func(ctx context.Context) error {
				return h.decodeHTTP(ctx, []byte(first), client, tc.dst,
					&mockMemDb{mocks: eofMocks(), updateUnFilteredReturn: true}, tc.opts)
			})
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("decodeHTTP = %v, want nil: the client closed", err)
				}
			case <-time.After(time.Minute): // the bound only turns a hang into a failure
				t.Fatal("decodeHTTP did not return")
			}
			// The connection has ended: what the app reads is every answer
			// it was given.
			_ = proxy.Close()
			methods := make([]string, tc.answers)
			for i := range methods {
				methods[i] = tc.method
			}
			got := readEveryAnswer(t, app, methods...)
			for _, a := range got {
				if tc.want != "" && a != tc.want {
					t.Fatalf("the calls were answered %q, want %q each", got, tc.want)
				}
			}
			var wantErrs []string
			if tc.errLog != "" {
				wantErrs = []string{tc.errLog}
			}
			if errs := errorLogs(logs); strings.Join(errs, "\n") != strings.Join(wantErrs, "\n") {
				t.Fatalf("error logs %q, want %q", errs, wantErrs)
			}
			if n := client.readsAfterEOF(); n != 0 {
				t.Fatalf("the client's connection was read %d more times after its EOF, want none: the EOF is honoured once its bytes are served", n)
			}
			if tc.dep {
				eachCallAlone(t, depAfter, tc.answers)
			}
		})
	}
}

// queuedConn is the proxy's TCP end under its TLS server. It is read as it
// is until queue, which reads all the app sends until its TCP close; Reads
// then give those bytes. A TLS server's read of a record takes what follows
// it with it as far as its buffer goes, as on a socket that holds both, so
// the read of the last call's record takes the alert behind it.
type queuedConn struct {
	net.Conn
	mu     sync.Mutex
	queued io.Reader
}

func (c *queuedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	q := c.queued
	c.mu.Unlock()
	if q == nil {
		return c.Conn.Read(p)
	}
	return q.Read(p)
}

func (c *queuedConn) queue(t *testing.T) {
	t.Helper()
	_ = c.Conn.SetReadDeadline(time.Now().Add(time.Minute)) // the bound only turns a hang into a failure
	all, err := io.ReadAll(c.Conn)
	if err != nil {
		t.Fatalf("reading what the app sent until its close: %v", err)
	}
	_ = c.Conn.SetReadDeadline(time.Time{})
	c.mu.Lock()
	c.queued = bytes.NewReader(all)
	c.mu.Unlock()
}

// A TLS 1.2 client that sends its last call and then its close_notify
// (tls.Conn.CloseWrite) has the call answered: crypto/tls gives the call's
// last bytes with io.EOF when the alert is read with them, whether the call
// fits in replay's small read or runs past it. TLS 1.3 sends the alert as
// application data, and gives the bytes alone.
func TestDecodeHTTPAnswersTheLastCallOfATLS12ClientThatClosed(t *testing.T) {
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	cert := ts.TLS.Certificates[0]
	ts.Close()
	for _, tc := range []struct{ name, last string }{
		{"a call", eofPost},
		{"a call over a small read", eofBigPost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			appTCP, proxyTCP := tcpPair(t)
			under := &queuedConn{Conn: proxyTCP}
			app := tls.Client(appTCP, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12}) //nolint:gosec // a test's own certificate
			proxy := tls.Server(under, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
			t.Cleanup(func() { _ = appTCP.Close() })
			shook := make(chan error, 1)
			go func() { shook <- proxy.Handshake() }()
			if err := app.Handshake(); err != nil {
				t.Fatal(err)
			}
			if err := <-shook; err != nil {
				t.Fatal(err)
			}
			// The last call, the alert behind it and the app's TCP close
			// (it still reads its answers) are all queued before replay
			// reads, so the Read that ends the call reads the alert.
			if _, err := app.Write([]byte(tc.last)); err != nil {
				t.Fatal(err)
			}
			if err := app.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if err := appTCP.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			under.queue(t)
			core, logs := observer.New(zapcore.DebugLevel)
			h := &HTTP{Logger: zap.New(core)}
			done := runDecode(t, proxyTCP, func(ctx context.Context) error {
				return h.decodeHTTP(ctx, []byte(eofPost), proxy, nil,
					&mockMemDb{mocks: eofMocks(), updateUnFilteredReturn: true}, models.OutgoingOptions{})
			})
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("decodeHTTP = %v, want nil: the client closed", err)
				}
			case <-time.After(time.Minute): // the bound only turns a hang into a failure
				t.Fatal("decodeHTTP did not return")
			}
			_ = proxyTCP.Close()
			readEveryAnswer(t, app, "POST", "POST")
			if errs := errorLogs(logs); len(errs) > 0 {
				t.Fatalf("error logs %q, want none", errs)
			}
		})
	}
}
