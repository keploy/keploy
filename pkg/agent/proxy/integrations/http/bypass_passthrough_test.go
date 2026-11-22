package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

type passResult struct {
	ended bool
	err   error
}

// passTest runs passOneCallThrough for raw to addr on the proxy's end of a
// pipe and returns the client's end, the proxy's, a reader on the client's,
// and what passOneCallThrough returns. A pipe holds nothing: what the proxy
// writes, the client reads before the write returns.
func passTest(t *testing.T, addr, raw, method string) (client, proxySide net.Conn, br *bufio.Reader, res <-chan passResult) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client, proxySide = net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = proxySide.Close() })
	out := make(chan passResult, 1)
	go func() {
		ended, err := passOneCallThrough(ctx, zap.NewNop(), proxySide, &models.ConditionalDstCfg{Addr: addr}, []byte(raw), method)
		out <- passResult{ended, err}
	}()
	return client, proxySide, bufio.NewReader(client), out
}

// A call a bypassRule names reaches its real server in keploy test, and the
// server's answer reaches the client as the server gave it.
func TestPassOneCallThroughReachesTheServer(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Real", "yes")
		_, _ = io.WriteString(w, r.Method+" "+r.URL.Path+" "+string(body))
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	_, _, br, res := passTest(t, addr, "POST /v1/n HTTP/1.1\r\nHost: "+addr+"\r\nContent-Length: 3\r\n\r\nabc", "POST")
	resp, err := http.ReadResponse(br, &http.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("the client got no answer: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if r := awaitResult(t, res); r.err != nil || r.ended {
		t.Fatalf("passOneCallThrough = %v, %v; want the connection kept for the next call", r.ended, r.err)
	}
	if n := hits.Load(); n != 1 || resp.StatusCode != 200 || resp.Header.Get("X-Real") != "yes" || string(body) != "POST /v1/n abc" {
		t.Fatalf("hits %d, answer %d %q %q; want the real server's answer to the one call", n, resp.StatusCode, resp.Header.Get("X-Real"), body)
	}

	if _, err := passOneCallThrough(context.Background(), zap.NewNop(), nil, &models.ConditionalDstCfg{}, nil, "POST"); err == nil {
		t.Fatal("a call with no destination address was passed through")
	}
}

func awaitResult(t *testing.T, res <-chan passResult) passResult {
	t.Helper()
	return awaitDone(t, res, "passOneCallThrough returning")
}

// awaitDone is what ch gives, or the test fails once a quarter of its time
// has gone by with nothing on ch.
func awaitDone[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	var expired <-chan time.Time
	if dl, ok := t.Deadline(); ok {
		timer := time.NewTimer(time.Until(dl) / 4)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case v := <-ch:
		return v
	case <-expired:
		t.Fatalf("%s never happened", what)
	}
	var zero T
	return zero
}

// rawServer answers each connection's request with answer, written as it is.
// After an upgrade (echo set) it writes back the next line as echo:<line>,
// then closes, or, with hold set, neither reads nor writes again until the
// test ends.
func rawServer(t *testing.T, answer string, echo, hold bool) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{})
	t.Cleanup(func() { close(ended); _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				br := bufio.NewReader(conn)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				_, _ = io.WriteString(conn, answer)
				if !echo {
					return
				}
				if line, err := br.ReadString('\n'); err == nil {
					_, _ = io.WriteString(conn, "echo:"+line)
				}
				if hold {
					<-ended
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// The decode loop answers a client's Expect: 100-continue itself, so the
// server's interim 100 is not relayed: the client gets the final answer as its
// first. An answer that ends the connection (Connection: close, or a body that
// runs to the close) ends the client's too. A 101 hands the connection over to
// the server both ways, and it is over when either side closes it.
func TestPassOneCallThroughRelaysTheFinalAnswerAndEndings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) // the server sends its 100 Continue here
		_, _ = io.WriteString(w, "got "+string(body))
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	_, _, br, res := passTest(t, addr, "POST /up HTTP/1.1\r\nHost: "+addr+"\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\nabc", "POST")
	resp, err := http.ReadResponse(br, &http.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("the client got no answer: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "got abc" {
		t.Fatalf("the client's first answer = %d %q, want the final 200 \"got abc\"", resp.StatusCode, body)
	}
	if r := awaitResult(t, res); r.err != nil || r.ended {
		t.Fatalf("passOneCallThrough = %v, %v; want the connection kept", r.ended, r.err)
	}

	for _, tc := range []struct{ name, answer string }{
		{"Connection: close", "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 4\r\n\r\nbody"},
		{"a body that runs to the close", "HTTP/1.1 200 OK\r\n\r\nbody"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, proxySide, br, res := passTest(t, rawServer(t, tc.answer, false, false), "GET / HTTP/1.1\r\nHost: dep\r\n\r\n", "GET")
			resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
			if err != nil {
				t.Fatalf("the client got no answer: %v", err)
			}
			type read struct {
				body []byte
				err  error
			}
			got := make(chan read, 1)
			go func() { b, err := io.ReadAll(resp.Body); got <- read{b, err} }()
			if r := awaitResult(t, res); r.err != nil || !r.ended {
				t.Fatalf("passOneCallThrough = %v, %v; want the client's connection ended", r.ended, r.err)
			}
			_ = proxySide.Close() // as the proxy does once the decode loop ends
			b := awaitDone(t, got, "the client reading the answer's body")
			if !resp.Close || string(b.body) != "body" || b.err != nil {
				t.Fatalf("answer close=%v body %q (%v), want Connection: close and the body", resp.Close, b.body, b.err)
			}
		})
	}

	const switching = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n"
	for _, clientFirst := range []bool{true, false} {
		t.Run("upgrade, the client closes first "+strconv.FormatBool(clientFirst), func(t *testing.T) {
			// When the client closes first, the server stays, silent.
			client, _, br, res := passTest(t, rawServer(t, switching, true, clientFirst), "GET /ws HTTP/1.1\r\nHost: dep\r\nUpgrade: echo\r\nConnection: Upgrade\r\n\r\n", "GET")
			resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
			if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("the upgrade's answer = %v, %v; want 101", resp, err)
			}
			if _, err := client.Write([]byte("hi\n")); err != nil {
				t.Fatal(err)
			}
			if line, err := br.ReadString('\n'); err != nil || line != "echo:hi\n" {
				t.Fatalf("after the upgrade the client got %q, %v; want the server's echo:hi", line, err)
			}
			if clientFirst {
				_ = client.Close()
			} else {
				// The server closed after its echo: the client's end closes.
				end := make(chan error, 1)
				go func() { _, err := br.ReadByte(); end <- err }()
				if err := awaitDone(t, end, "the client's read ending after the server closed"); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
					t.Fatalf("after the server closed the client read %v, want the end of the connection", err)
				}
			}
			if r := awaitResult(t, res); r.err != nil || !r.ended {
				t.Fatalf("passOneCallThrough after the upgrade = %v, %v; want the connection ended", r.ended, r.err)
			}
		})
	}
}

// keploy test's decode loop passes each call a bypassRule names (its Host) to
// the server, and serves the next call on the connection as before: one no
// rule names gets the no-mock answer. An answer that ends the connection ends the
// decode loop.
func TestDecodePassesACallARuleNamesThroughAndServesTheNext(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "real "+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	port, _ := strconv.Atoi(addr[strings.LastIndex(addr, ":")+1:])
	opts := models.OutgoingOptions{Rules: []models.BypassRule{{Host: `^dep\.internal$`}}}
	h := &HTTP{Logger: zaptest.NewLogger(t)}

	client, proxy := net.Pipe()
	done := runDecode(t, proxy, func(ctx context.Context) error {
		return h.decodeHTTP(ctx, []byte("GET /a HTTP/1.1\r\nHost: dep.internal\r\n\r\n"), proxy,
			&models.ConditionalDstCfg{Addr: addr, Port: uint(port)}, &mockMemDb{}, opts)
	})
	br := bufio.NewReader(client)
	var resp *http.Response
	var err error
	// Two calls the rule names, the second on the same connection.
	for i, path := range []string{"/a", "/a2"} {
		if i > 0 {
			if _, err := io.WriteString(client, "GET "+path+" HTTP/1.1\r\nHost: dep.internal\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
		}
		resp, err = http.ReadResponse(br, &http.Request{Method: "GET"})
		if err != nil {
			t.Fatalf("the passed-through call %s got no answer: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || string(body) != "real "+path || int(hits.Load()) != i+1 {
			t.Fatalf("call %s: answer %d %q, hits %d; want the real server's answer", path, resp.StatusCode, body, hits.Load())
		}
	}
	if _, err := io.WriteString(client, "GET /b HTTP/1.1\r\nHost: api.example\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, noMockAnswer) {
		t.Fatalf("the next call got %q, %v; want the no-mock answer %q", line, err, noMockAnswer)
	}
	if err := awaitDone(t, done, "decodeHTTP returning"); !errors.Is(err, ErrMockNotMatched) {
		t.Fatalf("decodeHTTP = %v, want ErrMockNotMatched for the call no rule names", err)
	}
	if hits.Load() != 2 {
		t.Fatalf("the server got %d calls, want only the two the rule names", hits.Load())
	}

	// A call that ends the connection ends the decode loop: the server's
	// answer says Connection: close, or the client's call did.
	for _, tc := range []struct{ name, call, answer string }{
		{"the answer closes", "GET /c HTTP/1.1\r\nHost: dep.internal\r\n\r\n", "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 2\r\n\r\nok"},
		{"the call closes", "GET /c HTTP/1.1\r\nHost: dep.internal\r\nConnection: close\r\n\r\n", "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"},
	} {
		client2, proxy2 := net.Pipe()
		dst := &models.ConditionalDstCfg{Addr: rawServer(t, tc.answer, false, false)}
		done2 := runDecode(t, proxy2, func(ctx context.Context) error {
			return h.decodeHTTP(ctx, []byte(tc.call), proxy2, dst, &mockMemDb{}, opts)
		})
		resp, err = http.ReadResponse(bufio.NewReader(client2), &http.Request{Method: "GET"})
		if err != nil {
			t.Fatalf("%s: no answer: %v", tc.name, err)
		}
		if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "ok" {
			t.Fatalf("%s: the answer's body = %q, %v; want ok", tc.name, body, err)
		}
		if err := awaitDone(t, done2, tc.name+": decodeHTTP returning"); err != nil {
			t.Fatalf("%s: decodeHTTP = %v, want nil", tc.name, err)
		}
	}
}
