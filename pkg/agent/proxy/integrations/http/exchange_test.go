package http

import (
	"bufio"
	"context"
	"errors"
	"net"
	nethttp "net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
	"golang.org/x/sync/errgroup"
)

// tcpPair returns the two ends of a loopback TCP connection: what the app
// holds and what the proxy holds. Unlike net.Pipe a write does not wait for
// the peer to read it, as on a real socket.
func tcpPair(t *testing.T) (app, proxy net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	app, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	proxy = <-accepted
	if proxy == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = app.Close(); _ = proxy.Close() })
	return app, proxy
}

// readFor reads from c what arrives within d.
func readFor(c net.Conn, d time.Duration) string {
	_ = c.SetReadDeadline(time.Now().Add(d))
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return string(out)
		}
	}
}

// readUntil reads from c until what it has read holds want, or d passes.
func readUntil(c net.Conn, want string, d time.Duration) string {
	deadline := time.Now().Add(d)
	var out []byte
	buf := make([]byte, 4096)
	for !strings.Contains(string(out), want) && time.Now().Before(deadline) {
		_ = c.SetReadDeadline(deadline)
		n, err := c.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	return string(out)
}

const noMockAnswer = "HTTP/1.1 502 Bad Gateway"

// startDecode runs decodeHTTP, as replay does, on the first bytes the proxy
// read (first) and the proxy's end of the app's connection, with no mocks:
// a request that is read whole gets the 502 "no matching mock" answer, and
// decodeHTTP returns ErrMockNotMatched.
func startDecode(t *testing.T, first string, proxy net.Conn) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	done := make(chan error, 1)
	go func() {
		done <- h.decodeHTTP(ctx, []byte(first), proxy, nil, &mockMemDb{}, models.OutgoingOptions{})
	}()
	return done
}

func wantMockMiss(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, ErrMockNotMatched) {
			t.Fatalf("decodeHTTP = %v, want the request read whole and matched (ErrMockNotMatched)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decodeHTTP did not return")
	}
}

// Empty lines in front of a request line are skipped when framing it (RFC 9112
// §2.2) and are not part of the request: net/http.ReadRequest, which replay
// and both record paths parse the framed request with, fails on them with
// `malformed HTTP request ""`. The request is framed and parsed from its
// request line.
func TestARequestAfterEmptyLinesIsParsed(t *testing.T) {
	const req = "POST /v1/echo HTTP/1.1\r\nHost: tlsup\r\nContent-Length: 5\r\n\r\nhello"
	t.Run("replay", func(t *testing.T) {
		app, proxy := tcpPair(t)
		done := startDecode(t, "\r\n\r\n"+req, proxy)
		if got := readUntil(app, noMockAnswer, 2*time.Second); !strings.HasPrefix(got, noMockAnswer) {
			t.Fatalf("the app got %q, want the answer to its request", got)
		}
		wantMockMiss(t, done)
	})
	t.Run("V2 record", func(t *testing.T) {
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		stream, send, _ := makeStream(t, fakeconn.FromClient, 8)
		now := time.Now()
		send([]byte("\r\n"), now, now)
		send([]byte("\r\n"+req), now, now)
		var finalReq []byte
		if err := h.readRequestV2(context.Background(), stream, &finalReq); err != nil || string(finalReq) != req {
			t.Fatalf("readRequestV2 = (%q, %v), want the request from its request line: %q", finalReq, err, req)
		}
		m, err := h.buildHTTPMock(&FinalHTTP{Req: finalReq, Resp: []byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"),
			ReqTimestampMock: now, ResTimestampMock: now}, 443, "test-conn", models.OutgoingOptions{})
		if err != nil || m == nil || m.Spec.HTTPReq == nil || m.Spec.HTTPReq.Body != "hello" {
			t.Fatalf("buildHTTPMock = (%+v, %v), want a mock of the request", m, err)
		}
	})
	t.Run("framed request parses", func(t *testing.T) {
		got, err := runChunkedRequest(t, "\r\n\r\n"+req, newScriptedConn())
		if err != nil || got != req {
			t.Fatalf("HandleChunkedRequests = (%q, %v), want %q", got, err, req)
		}
		if _, err := nethttp.ReadRequest(bufio.NewReader(strings.NewReader(got))); err != nil {
			t.Fatalf("the framed request does not parse: %v", err)
		}
	})
}

// A 101 Switching Protocols, or a 2xx to a CONNECT, ends the HTTP exchange but
// not the connection: what follows is another protocol, which the record loop
// relays. The legacy record path must not wait for an HTTP body after it.
func TestHandleChunkedResponsesHandsOverAfterAProtocolSwitch(t *testing.T) {
	for _, c := range []struct{ method, head string }{
		{"GET", "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"},
		{"CONNECT", "HTTP/1.1 200 Connection Established\r\n\r\n"},
	} {
		dest := newScriptedConn() // the upgraded stream: nothing more yet
		client := newScriptedConn()
		t.Cleanup(func() { _ = dest.Close(); _ = client.Close() })
		h := &HTTP{Logger: zaptest.NewLogger(t)}
		finalResp := []byte(c.head)
		done := make(chan error, 1)
		go func() {
			done <- h.handleChunkedResponses(context.Background(), &finalResp, client, dest, []byte(c.head), c.method)
		}()
		select {
		case err := <-done:
			if err != nil || string(finalResp) != c.head {
				t.Fatalf("%s %q: handleChunkedResponses = (%q, %v)", c.method, c.head, finalResp, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s %q: handleChunkedResponses waited for a body after the protocol switch", c.method, c.head)
		}
	}
}

// A chunk-size or trailer line has a bounded length: a peer that sends
// megabytes with no line ending is not framing a chunked body, and the reader
// must say so rather than buffer it without end.
func TestChunkedBodyRefusesAnUnendingLine(t *testing.T) {
	var body chunkedBody
	done, err := body.feed([]byte(strings.Repeat("a", maxChunkedLineLen+1)))
	if done || !errors.Is(err, errMalformedChunkedBody) {
		t.Fatalf("feed of a %d-byte line with no ending = (%v, %v), want errMalformedChunkedBody", maxChunkedLineLen+1, done, err)
	}
	if done, err := (&chunkedBody{}).feed([]byte(strings.Repeat("a", maxChunkedLineLen))); done || err != nil {
		t.Fatalf("feed of a %d-byte line still arriving = (%v, %v), want more to be read", maxChunkedLineLen, done, err)
	}
}

// The most common interim response on the V2 record path: an upstream answers
// an "Expect: 100-continue" upload with a 100 Continue as soon as it has the
// headers, in a read of its own, and the final response comes later. The
// recorded response is read through to the final one's end, not stopped at
// the 100's empty line.
func TestReadResponseV2ReadsPastAHundredContinueInItsOwnRead(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	stream, send, _ := makeStream(t, fakeconn.FromDest, 8)
	now := time.Now()
	const final = "HTTP/1.1 201 Created\r\nContent-Length: 5\r\n\r\nhello"
	send([]byte("HTTP/1.1 201 Created\r\nContent-Length: 5\r\n\r\n"), now, now)
	send([]byte("hello"), now, now)
	finalResp := []byte("HTTP/1.1 100 Continue\r\n\r\n") // the first read, already taken by recordV2
	done := make(chan error, 1)
	go func() { _, err := h.readResponseV2(context.Background(), stream, &finalResp, "POST"); done <- err }()
	select {
	case err := <-done:
		if err != nil || string(finalResp) != "HTTP/1.1 100 Continue\r\n\r\n"+final {
			t.Fatalf("readResponseV2 = (%q, %v), want through the final response", finalResp, err)
		}
	case <-time.After(time.Second):
		t.Fatal("readResponseV2 did not return")
	}
}

// encodeHarness runs the legacy record path, encodeHTTP, between an app and an
// upstream on loopback TCP. upstream serves the one connection the proxy
// makes. The mocks it records are forwarded to the returned channel.
func encodeHarness(t *testing.T, first string, upstream func(net.Conn)) (app net.Conn, done <-chan error, mocks <-chan *models.Mock) {
	t.Helper()
	out := make(chan *models.Mock, 4)
	if mgr := syncMock.Get(); mgr != nil {
		mgr.SetOutputChannel(out)
		t.Cleanup(func() { mgr.SetOutputChannel(make(chan<- *models.Mock, 1)) })
	}
	app, proxyClient := tcpPair(t)
	up, proxyDest := tcpPair(t)
	go upstream(up)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, g)
	gctx = context.WithValue(gctx, models.ClientConnectionIDKey, "test-conn")
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	d := make(chan error, 1)
	go func() {
		d <- h.encodeHTTP(gctx, []byte(first), proxyClient, proxyDest, out, models.OutgoingOptions{}, nil)
	}()
	return app, d, out
}

// A mock recorded after an interim response is the final response, and it is
// timed by the final response, not by the interim one that came first.
func TestEncodeHTTPTimesAMockByItsFinalResponse(t *testing.T) {
	var finalSent time.Time
	sent := make(chan struct{})
	app, done, mocks := encodeHarness(t, "GET /x HTTP/1.1\r\nHost: up\r\n\r\n", func(up net.Conn) {
		_ = readUntil(up, "\r\n\r\n", 2*time.Second)
		_, _ = up.Write([]byte("HTTP/1.1 103 Early Hints\r\nLink: </a.css>; rel=preload\r\n\r\n"))
		time.Sleep(200 * time.Millisecond)
		finalSent = time.Now()
		close(sent)
		_, _ = up.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	})
	if got := readUntil(app, "\r\n\r\nok", 2*time.Second); !strings.HasSuffix(got, "\r\n\r\nok") {
		t.Fatalf("the app got %q", got)
	}
	<-sent
	var m *models.Mock
	select {
	case m = <-mocks:
	case <-time.After(time.Second):
		t.Fatal("no mock recorded")
	}
	_ = app.Close()
	<-done
	if m.Spec.HTTPResp == nil || m.Spec.HTTPResp.StatusCode != 200 {
		t.Fatalf("recorded %+v, want the final response", m.Spec.HTTPResp)
	}
	if m.Spec.ResTimestampMock.Before(finalSent) {
		t.Fatalf("mock response time %v is before the final response was sent (%v): it is the interim response's",
			m.Spec.ResTimestampMock, finalSent)
	}
}
