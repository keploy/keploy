package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/internal/leakcheck"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The ingress lives as long as the recording, a long native or docker one's
// for days, and serves every connection the app is sent in that time. A
// connection it is done with leaves nothing behind it: no goroutine of its
// handler parked on the ingress's context, and no registration on that
// context. One goroutine was left, per connection, by the zero-copy HTTP/1.1
// loop and the raw passthrough alike, parked on the context and holding the
// closed connection's objects until the agent stopped: a soak's 10
// connections a second left 600 goroutines, about 3 MB, a minute. The close
// on shutdown each handler now registers instead must be stopped as the
// handler returns, or it leaves the same growth, a registration and a
// closure holding the connection per connection, with no goroutine to show.
func TestIngressHandlersLeaveNothingBehindAClosedConnection(t *testing.T) {
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 64)}
	// The ingress's context: live for the whole test, as an agent's is for
	// every connection it serves.
	ctx := leakcheck.NewContext(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	defer ctx.Cancel()

	const conns = 50
	for i := 0; i < conns; i++ {
		upConn, err := net.Dial("tcp4", strings.TrimPrefix(upstream.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		server, client := tcpPair(t)
		done := make(chan struct{})
		go func() {
			defer close(done)
			pm.handleHttp1ZeroCopy(ctx, server, upConn, pm.logger, pm.tcChan, 8080, i%2 == 0, func() {})
		}()
		if _, err := fmt.Fprintf(client, "GET /c%d HTTP/1.1\r\nHost: app\r\n\r\n", i); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(client), nil)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		_ = client.Close() // the client hangs up, as most do
		<-done
		// What the caller (handleConnection) closes as it returns.
		_ = server.Close()
		_ = upConn.Close()
	}
	httpLive := ctx.Live()
	for i := 0; i < conns; i++ {
		appSide, clientSide := tcpPair(t)
		upSide, appPeer := tcpPair(t)
		done := make(chan struct{})
		go func() {
			defer close(done)
			forwardRawTCP(ctx, appSide, upSide)
		}()
		_ = clientSide.Close()
		_ = appPeer.Close()
		<-done
	}
	pm.captures.Wait()

	if n, g := leakcheck.Parked("incoming.(*IngressProxyManager).handleHttp1ZeroCopy", "incoming.forwardRawTCP"); n != 0 {
		t.Errorf("%d goroutines of the ingress's handlers are parked after the %d connections they served all ended; the first:\n%s", n, 2*conns, g)
	}
	if live := ctx.Live(); live != 0 {
		t.Errorf("%d registrations on the ingress's context outlive the %d connections (%d after the HTTP/1.1 half)", live, 2*conns, httpLive)
	}
}

// The close on shutdown is still registered while a connection is served,
// and it still runs when the ingress stops: a connection stuck on a hung
// backend, or idle, would otherwise keep its handler blocked for good after
// StopIngress. The leak row above passes with no close on shutdown at all, so
// these pin it. Each waits until the handler has forwarded the client's bytes
// (to the backend, or its answer back to the client), which it does after
// registering the close, then checks the registration is live, stops the
// ingress and waits for the handler to return. Nothing reads a clock: go
// test's -timeout is the hang guard.
//
// The zero-copy loop blocks in two places, so its close has two halves: the
// upstream's, for a hung backend, and the client's, for a keep-alive client
// idle between requests, as a pooled client's connection mostly is.
func TestZeroCopyHTTPOnAHungBackendReturnsWhenTheIngressStops(t *testing.T) {
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
	})
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 64)}
	ctx := leakcheck.NewContext(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	backend, upConn := tcpPair(t) // a backend that reads the request and never answers
	server, client := tcpPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pm.handleHttp1ZeroCopy(ctx, server, upConn, pm.logger, pm.tcChan, 8080, false, func() {})
	}()
	if _, err := fmt.Fprintf(client, "GET /hung HTTP/1.1\r\nHost: app\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Read(make([]byte, 1)); err != nil {
		t.Fatalf("the request never reached the backend: %v", err)
	}
	if live := ctx.Live(); live != 1 {
		t.Fatalf("the handler, blocked on a hung backend, has %d closes registered on the ingress's context, want 1", live)
	}
	ctx.Cancel()
	<-done
}

func TestZeroCopyHTTPOnAnIdleKeepAliveClientReturnsWhenTheIngressStops(t *testing.T) {
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
	})
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 64)}
	ctx := leakcheck.NewContext(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	backend, upConn := tcpPair(t)
	server, client := tcpPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pm.handleHttp1ZeroCopy(ctx, server, upConn, pm.logger, pm.tcChan, 8080, false, func() {})
	}()
	go func() { // a backend that answers each request
		br := bufio.NewReader(backend)
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			_ = req.Body.Close()
			if _, err := fmt.Fprintf(backend, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"); err != nil {
				return
			}
		}
	}()
	if _, err := fmt.Fprintf(client, "GET /one HTTP/1.1\r\nHost: app\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("the client got no answer: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	// The client keeps its connection open and sends nothing more: the loop
	// waits on it for the next request, and only the client's half of the
	// close ends that wait. Stopped before it is there (taking its capture
	// slot, which watches ctx), the loop would return on its own, so the row
	// waits until it is: parked reading a socket in the handler's own frame.
	waitParkedReading(t, "incoming.(*IngressProxyManager).handleHttp1ZeroCopy")
	if live := ctx.Live(); live != 1 {
		t.Fatalf("the handler of an idle keep-alive client has %d closes registered on the ingress's context, want 1", live)
	}
	ctx.Cancel()
	<-done
	pm.captures.Wait() // the exchange's capture, on the stubbed hook
}

// waitParkedReading yields, by count, until a goroutine is parked on a socket
// (IO wait) in a frame of fn itself; a goroutine fn only created does not
// count. Nothing reads a clock.
func waitParkedReading(t *testing.T, fn string) {
	t.Helper()
	buf := make([]byte, 1<<20)
	for y := 0; ; y++ {
		n := runtime.Stack(buf, true)
		if n == len(buf) {
			buf = make([]byte, 2*len(buf))
			continue
		}
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			header, _, _ := strings.Cut(g, "\n")
			if strings.Contains(header, "[IO wait") && strings.Contains(g, fn+"(") {
				return
			}
		}
		if y == 100_000 {
			t.Fatalf("no goroutine is parked on a socket in %s", fn)
		}
		runtime.Gosched()
	}
}

func TestRawPassthroughOfAnIdleConnectionReturnsWhenTheIngressStops(t *testing.T) {
	ctx := leakcheck.NewContext(context.Background())
	appSide, clientSide := tcpPair(t)
	upSide, appPeer := tcpPair(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		forwardRawTCP(ctx, appSide, upSide)
	}()
	// One byte through, then both ends stay open and idle.
	if _, err := clientSide.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	if _, err := appPeer.Read(make([]byte, 1)); err != nil {
		t.Fatalf("the byte never reached the app: %v", err)
	}
	if live := ctx.Live(); live != 1 {
		t.Fatalf("the passthrough of an idle connection has %d closes registered on the ingress's context, want 1", live)
	}
	ctx.Cancel()
	<-done
}
