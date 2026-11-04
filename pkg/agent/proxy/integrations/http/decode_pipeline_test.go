package http

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
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
	return bodies
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
	for _, tc := range []struct {
		name   string
		call   string
		method string
		dst    *models.ConditionalDstCfg
		opts   models.OutgoingOptions
		setup  func(t *testing.T, h *HTTP)
		// want is each call's answer; dep, that the calls went to the dependency.
		want string
		dep  bool
	}{
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
	} {
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
			if !tc.dep {
				return
			}
			for i := 0; i < 2; i++ {
				select {
				case rest := <-depAfter:
					if rest != "" {
						t.Fatalf("the dependency was sent %q after the call it answered, want each call on its own", rest)
					}
				case <-time.After(time.Minute): // the bound only turns a hang into a failure
					t.Fatalf("the dependency's connection %d of 2 did not end", i+1)
				}
			}
		})
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
