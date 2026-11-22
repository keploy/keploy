package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A connection's first bytes say what it carries as soon as they can: a TLS
// record by its header, HTTP/2 by all of its preface (which also begins an
// HTTP/1 request line, of method PRI), an HTTP/1 request by its method and
// space, and anything else by its first byte no method has. At the end (the
// connection's, or all 24 bytes read) the bytes say one kind.
func TestAConnectionsFirstBytesSayWhatItCarries(t *testing.T) {
	sslRequest := []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f} // PostgreSQL's
	for _, c := range []struct {
		b    string
		end  bool
		want prefaceKind
	}{
		{"", false, prefaceUndecided},
		{"", true, prefaceOther},
		{"G", false, prefaceUndecided},
		{"GET", false, prefaceUndecided},
		{"GET", true, prefaceOther},
		{"GET ", false, prefaceHTTP1},
		{"get /x", false, prefaceHTTP1},
		{"M-SEARCH * HTTP/1.1", false, prefaceHTTP1},
		{" GET /", false, prefaceOther},
		{"GET\r\n", false, prefaceOther},
		{"P", false, prefaceUndecided},
		{"PRI ", false, prefaceUndecided},
		{"PRI * HTTP/2.0\r\n", false, prefaceUndecided},
		{"PRI ", true, prefaceHTTP1},
		{"PRI /x HTTP/1.1", false, prefaceHTTP1},
		{clientPreface, false, prefaceH2},
		{clientPreface, true, prefaceH2},
		{strings.Repeat("A", len(clientPreface)), true, prefaceOther},
		{"\x16", false, prefaceUndecided},
		{"\x16\x03", false, prefaceUndecided},
		{"\x16\x03\x01\x02", false, prefaceUndecided},
		{"\x16\x03\x01\x02\x00", false, prefaceTLS},
		{"\x16\x03\x01", true, prefaceOther},
		{"\x16\x04", false, prefaceOther},
		{"\x16\x03\x04", false, prefaceOther},
		{string(sslRequest[:1]), false, prefaceOther},
		{string(sslRequest), false, prefaceOther},
		{"\x01\x00\x00\x00", false, prefaceOther},
	} {
		if got := kindOf([]byte(c.b), c.end); got != c.want {
			t.Errorf("kindOf(%q, end=%v) = %d, want %d", c.b, c.end, got, c.want)
		}
	}
}

// binaryApp is an app on 127.0.0.1 that speaks a protocol that is not HTTP,
// as Gradle's daemon does to its client: it takes a hello of helloLen bytes
// and answers it at once (none: no answer), then takes what comes up to the
// client's end and answers with its length. got has every byte the app was
// sent.
func binaryApp(t *testing.T, helloLen int) (port int, got chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got = make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		hello := make([]byte, helloLen)
		if helloLen > 0 {
			if _, err := io.ReadFull(c, hello); err != nil {
				got <- nil
				return
			}
			if _, err := c.Write([]byte("N")); err != nil {
				got <- nil
				return
			}
		}
		rest, _ := io.ReadAll(c)
		_, _ = fmt.Fprintf(c, "took %d", len(rest))
		got <- append(hello, rest...)
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

// A call the live handler takes (one between the run's own processes: a
// process of its tree to a listener of another) that is not HTTP reaches the
// app unread, both ways, at the address the proxy writes a dual-stack
// socket's destination in ([127.0.0.1]:p, a Java client's): its client's
// hello is answered before it sends more, and every byte it sends, those no
// HTTP request has among them, reaches the app as it was sent. Nothing is
// recorded. The hello is a binary protocol's (its first byte is no
// method's: PostgreSQL's 8-byte SSLRequest), or more bytes than HTTP/2's
// preface, all of them a method's characters (they say what they carry
// once 24 have come), or none: the client sends two such bytes and ends its
// side before they say what they carry. Before, the ingress refused that
// address as not an IP address and closed the call; given another, it
// waited for 24 bytes the client never sent before its answer, and read the
// rest as an HTTP/1 request, dropping it: Gradle's client, to the daemon it
// forks, failed with "The first result from the daemon was empty".
func TestACallBetweenTheRunsOwnProcessesThatIsNotHTTPReachesTheAppUnread(t *testing.T) {
	for _, c := range []struct {
		name        string
		hello, body []byte
	}{
		{"a binary hello", []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}, []byte("\x00\x01binary\r\n\r\nGET / HTTP/1.1\r\n\r\n\xff")},
		{"a hello of a method's characters longer than the preface", []byte(strings.Repeat("A", len(clientPreface)+6)), []byte("rest")},
		{"bytes that end before they say what they carry", nil, []byte("GE")},
	} {
		for _, sync := range []bool{false, true} {
			t.Run(c.name+"/"+map[bool]string{false: "zero-copy", true: "synchronous"}[sync], func(t *testing.T) {
				testANotHTTPCallReachesTheAppUnread(t, sync, c.hello, c.body)
			})
		}
	}
}

// testANotHTTPCallReachesTheAppUnread is one call of
// TestACallBetweenTheRunsOwnProcessesThatIsNotHTTPReachesTheAppUnread: the
// client sends hello and waits for its answer (none for an empty hello),
// then sends body and ends its side.
func testANotHTTPCallReachesTheAppUnread(t *testing.T, sync bool, hello, body []byte) {
	stubIngressPaused(t, func() bool { return false })
	var captured atomic.Int32
	stubCaptureHook(t, func(context.Context, *zap.Logger, chan *models.TestCase, *http.Request, *http.Response, time.Time, time.Time, models.IncomingOptions, bool, bool, uint16) {
		captured.Add(1)
	})
	core, logs := observer.New(zapcore.DebugLevel)
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	cfg.Agent.Synchronous = sync
	pm := New(zap.New(core), nil, cfg)
	t.Cleanup(pm.captures.Wait)
	port, got := binaryApp(t, len(hello))
	client, proxied := loopbackPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.ServeLive(ctx, proxied, fmt.Sprintf("[127.0.0.1]:%d", port), uint16(port))
	}()
	_ = client.SetDeadline(time.Now().Add(20 * time.Second)) // a call that hangs fails, not the run
	if len(hello) > 0 {
		if _, err := client.Write(hello); err != nil {
			t.Fatal(err)
		}
		answer := make([]byte, 1)
		if _, err := io.ReadFull(client, answer); err != nil || string(answer) != "N" {
			t.Fatalf("the hello's answer = %q, %v; want the app's N, before the client sends more", answer, err)
		}
	}
	if _, err := client.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = client.(*net.TCPConn).CloseWrite()
	end, err := io.ReadAll(client)
	if err != nil || string(end) != fmt.Sprintf("took %d", len(body)) {
		t.Fatalf("the app's last answer = %q, %v; want %q", end, err, fmt.Sprintf("took %d", len(body)))
	}
	want := append(append([]byte(nil), hello...), body...)
	if sent := <-got; !bytes.Equal(sent, want) {
		t.Fatalf("the app was sent %q, want the client's %q", sent, want)
	}
	<-served
	pm.captures.Wait()
	if n := captured.Load(); n != 0 {
		t.Fatalf("%d calls captured from a connection that is not HTTP, want none", n)
	}
	if n := logs.FilterMessage("Failed to read client request; ignoring this connection. Verify the client is sending valid HTTP if this persists.").Len(); n != 0 {
		t.Fatal("the connection was read as HTTP/1")
	}
}

// An HTTP call the live handler takes at the address the proxy writes a
// dual-stack socket's destination in ([127.0.0.1]:p) reaches the app and is
// captured. Before, the ingress refused that address as not an IP address
// and port, and closed the call unanswered.
func TestAnHTTPCallToADualStackSocketsIPv4DestinationIsServedAndCaptured(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	var captured atomic.Int32
	stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase, req *http.Request, _ *http.Response, _, _ time.Time, _ models.IncomingOptions, _, _ bool, _ uint16) {
		if req.URL.Path == "/ping" {
			captured.Add(1)
		}
	})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	t.Cleanup(app.Close)
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	pm := New(zap.NewNop(), nil, cfg)
	t.Cleanup(pm.captures.Wait)
	port := app.Listener.Addr().(*net.TCPAddr).Port
	client, proxied := loopbackPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.ServeLive(ctx, proxied, fmt.Sprintf("[127.0.0.1]:%d", port), uint16(port))
	}()
	_ = client.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := client.Write([]byte("GET /ping HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("the call was not answered: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "pong" {
		t.Fatalf("the call's answer = %d %q, want the app's 200 pong", resp.StatusCode, b)
	}
	_ = client.Close()
	<-served
	pm.captures.Wait()
	if n := captured.Load(); n != 1 {
		t.Fatalf("%d captures of the call, want 1", n)
	}
}

// An HTTP/1.0 request with no header, shorter than HTTP/2's 24-byte
// preface, is answered: the ingress no longer waits for 24 bytes its
// client, waiting for the answer, never sends.
func TestAnHTTPRequestShorterThanTheHTTP2PrefaceIsAnswered(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	stubCaptureHook(t, func(context.Context, *zap.Logger, chan *models.TestCase, *http.Request, *http.Response, time.Time, time.Time, models.IncomingOptions, bool, bool, uint16) {
	})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("short"))
	}))
	t.Cleanup(app.Close)
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	pm := New(zap.NewNop(), nil, cfg)
	t.Cleanup(pm.captures.Wait)
	client, proxied := loopbackPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.handleConnection(ctx, proxied, app.Listener.Addr().String(), pm.logger, make(chan *models.TestCase, 1), make(chan struct{}, 1), 8080)
	}()
	_ = client.SetDeadline(time.Now().Add(20 * time.Second))
	req := "GET /s HTTP/1.0\r\n\r\n"
	if len(req) >= len(clientPreface) {
		t.Fatalf("the request is %d bytes, want fewer than the preface's %d", len(req), len(clientPreface))
	}
	if _, err := client.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("the short request was not answered: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "short" {
		t.Fatalf("the answer's body = %q, want the app's", b)
	}
	_ = client.Close()
	<-served
}

// A recorded connection that ends before its first byte is closed, and the
// app is not dialed for it: there is nothing to pass on or record, and the
// connection is ended as its client ended it.
func TestAConnectionThatEndsBeforeItsFirstByteIsClosedWithoutDialingTheApp(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	dialed := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
			dialed <- fmt.Errorf("the app was dialed (from %v) for a connection that sent nothing", c.RemoteAddr())
			return
		}
		dialed <- nil
	}()
	stubIngressPaused(t, func() bool { return false })
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true // the path that reads a call's first bytes
	pm := New(zap.NewNop(), nil, cfg)
	t.Cleanup(pm.captures.Wait)
	client, proxied := loopbackPair(t)
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		pm.ServeLive(ctx, proxied, ln.Addr().String(), uint16(ln.Addr().(*net.TCPAddr).Port))
	}()
	select {
	case err := <-dialed:
		t.Fatal(err)
	case <-served:
	}
	// The handler has returned, so a dial it made is in the listener's
	// queue, where Accept takes it at once.
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second))
	if err := <-dialed; err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	if n, err := client.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("the client's read = %d, %v; want its connection ended (EOF)", n, err)
	}
}
