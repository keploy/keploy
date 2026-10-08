package proxy

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
	"sync"
	"testing"
	"time"

	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// An exchange is in flight from its request's first byte until its capture has
// decided it, and the mock manager must know: a mock the app made for it early
// on (an upstream call before a long stream) is otherwise swept as stale by any
// other request resolved meanwhile, and the exchange recorded without it. The
// ingress opens the exchange's window on the manager its capture resolves with,
// keeps it open while the response streams, hands it to the capture hook in its
// ctx, and closes it once the hook has returned. Both ingress paths: the
// synchronous loop and the zero-copy one.
func TestHandleHttp1Connection_OpensTheExchangeWindowUntilItsCaptureDecides(t *testing.T) {
	for _, synchronous := range []bool{true, false} {
		t.Run(map[bool]string{true: "synchronous", false: "zero-copy"}[synchronous], func(t *testing.T) {
			stubIngressPaused(t, func() bool { return false })
			mgr := syncMock.New(zap.NewNop())

			release := make(chan struct{})
			openAtApp := make(chan int, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				openAtApp <- mgr.OpenWindows() // the app has the request: it may call out now
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("first\n"))
				w.(http.Flusher).Flush() // chunked: the response streams
				<-release
				_, _ = w.Write([]byte("last\n"))
			}))
			t.Cleanup(upstream.Close)
			// Cleanups run last-in first-out: a failing assertion before the
			// stream is released still lets the handler finish, so Close does
			// not block on it and the test fails at once instead of timing out.
			var releaseOnce sync.Once
			releaseStream := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseStream)

			hookSaw := make(chan *syncMock.Window, 1)
			startOK := make(chan bool, 1)
			openInHook := make(chan int, 1)
			hookDone := make(chan struct{})
			stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
				req *http.Request, resp *http.Response, reqTS, respTS time.Time,
				opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
				defer close(hookDone)
				openInHook <- mgr.OpenWindows() // still open: the hook decides it
				startOK <- syncMock.WindowFromContext(ctx).Start().Equal(reqTS)
				hookSaw <- syncMock.WindowFromContext(ctx)
			})

			pm := &IngressProxyManager{
				logger:      zap.NewNop(),
				tcChan:      make(chan *models.TestCase, 4),
				synchronous: synchronous,
				samplingSem: make(chan struct{}, 1),
			}
			t.Cleanup(pm.captures.Wait) // every capture it started is over before the next test swaps a hook
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			accepted := make(chan net.Conn, 1)
			go func() {
				if c, aerr := ln.Accept(); aerr == nil {
					accepted <- c
				}
			}()
			client, err := net.Dial("tcp4", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			server := <-accepted
			t.Cleanup(func() { _ = server.Close() })

			ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
			defer cancel()
			handlerDone := make(chan struct{})
			go func() {
				defer close(handlerDone)
				pm.handleHttp1Connection(ctx, server, strings.TrimPrefix(upstream.URL, "http://"), pm.logger, pm.tcChan, make(chan struct{}, 1), 8080)
			}()

			if _, err := client.Write([]byte("GET /stream/1 HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatal(err)
			}
			if n := <-openAtApp; n != 1 {
				t.Fatalf("%d windows open when the request reached the app, want the exchange's: its egress calls start there", n)
			}
			// The response headers are through and its body is streaming.
			deadline := time.Now().Add(3 * time.Second)
			for mgr.OpenWindows() != 1 {
				if time.Now().After(deadline) {
					t.Fatalf("%d windows open while the response streams, want the exchange's", mgr.OpenWindows())
				}
				time.Sleep(time.Millisecond)
			}
			releaseStream()
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()

			select {
			case w := <-hookSaw:
				if w == nil {
					t.Fatal("the capture hook got no window in its ctx")
				}
				if n := <-openInHook; n != 1 {
					t.Fatalf("%d windows open while the capture hook ran, want the exchange's: it was ended before its capture decided it", n)
				}
				if !<-startOK {
					t.Fatal("the exchange's window does not start at the request time its capture stamps")
				}
				if !w.Keep() {
					t.Fatal("the exchange's window was given up")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the capture hook never ran")
			}
			<-hookDone
			deadline = time.Now().Add(3 * time.Second)
			for mgr.OpenWindows() != 0 {
				if time.Now().After(deadline) {
					t.Fatalf("%d windows still open after the capture returned", mgr.OpenWindows())
				}
				time.Sleep(time.Millisecond)
			}
			_ = client.Close()
			<-handlerDone
		})
	}
}

// An exchange that cannot be captured (recording paused under memory
// pressure) opens no window at all, in either loop: nothing would decide it,
// and an open window holds every droppable mock the app makes meanwhile.
func TestHandleHttp1Connection_OpensNoWindowForAnExchangeItCannotCapture(t *testing.T) {
	for _, synchronous := range []bool{true, false} {
		t.Run(map[bool]string{true: "synchronous", false: "zero-copy"}[synchronous], func(t *testing.T) {
			stubIngressPaused(t, func() bool { return true })
			mgr := syncMock.New(zap.NewNop())
			openAtApp := make(chan int, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				openAtApp <- mgr.OpenWindows()
				_, _ = w.Write([]byte("ok"))
			}))
			t.Cleanup(upstream.Close)
			stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
				req *http.Request, resp *http.Response, reqTS, respTS time.Time,
				opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
				t.Error("an exchange was captured while recording was paused")
			})
			pm := &IngressProxyManager{
				logger:      zap.NewNop(),
				tcChan:      make(chan *models.TestCase, 4),
				synchronous: synchronous,
				samplingSem: make(chan struct{}, 1),
			}
			t.Cleanup(pm.captures.Wait) // every capture it started is over before the next test swaps a hook
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			accepted := make(chan net.Conn, 1)
			go func() {
				if c, aerr := ln.Accept(); aerr == nil {
					accepted <- c
				}
			}()
			client, err := net.Dial("tcp4", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			server := <-accepted
			ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
			defer cancel()
			handlerDone := make(chan struct{})
			go func() {
				defer close(handlerDone)
				pm.handleHttp1Connection(ctx, server, strings.TrimPrefix(upstream.URL, "http://"), pm.logger, pm.tcChan, make(chan struct{}, 1), 8080)
			}()
			if _, err := client.Write([]byte("GET /x HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			if resp, err := http.ReadResponse(bufio.NewReader(client), nil); err == nil {
				_, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			_ = client.Close()
			select {
			case <-handlerDone:
			case <-time.After(5 * time.Second):
				t.Fatal("the handler did not return")
			}
			_ = server.Close()
			if n := mgr.OpenWindows(); n != 0 {
				t.Fatalf("%d windows left open by an exchange that was not captured", n)
			}
			select {
			case n := <-openAtApp:
				if n != 0 {
					t.Fatalf("%d windows opened for an exchange that cannot be captured: nothing would decide it", n)
				}
			default:
				t.Fatal("fixture: the request never reached the app")
			}
		})
	}
}

// ingressPipe serves one client connection through pm.handleHttp1Connection to
// upstream, and returns the client end and a channel closed when the handler
// returns.
func ingressPipe(t *testing.T, ctx context.Context, pm *IngressProxyManager, upstream string) (net.Conn, chan struct{}) {
	t.Helper()
	return ingressPipeOn(t, ctx, pm, upstream, make(chan struct{}, 1), nil)
}

// ingressPipeOn is ingressPipe with the synchronous lock the handler takes
// (sem), and, if wrap is not nil, with the handler's end of the client
// connection wrapped by it.
func ingressPipeOn(t *testing.T, ctx context.Context, pm *IngressProxyManager, upstream string, sem chan struct{}, wrap func(net.Conn) net.Conn) (net.Conn, chan struct{}) {
	t.Helper()
	// Registered first, so it runs last: every capture the handler started is
	// over before the next test swaps a hook or a semaphore it reads.
	t.Cleanup(pm.captures.Wait)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, aerr := ln.Accept(); aerr == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server := <-accepted
	t.Cleanup(func() { _ = server.Close() })
	if wrap != nil {
		server = wrap(server)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// As handleConnection does: the client's connection is closed once
		// its handler is done. A response body that ends at the close of its
		// connection ends for the client here.
		defer server.Close()
		pm.handleHttp1Connection(ctx, server, upstream, pm.logger, pm.tcChan, sem, 8080)
	}()
	return client, done
}

func waitOpenWindows(t *testing.T, mgr *syncMock.SyncMockManager, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for mgr.OpenWindows() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%d windows open %s, want %d", mgr.OpenWindows(), what, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// A keep-alive exchange that turns out not to be captured (its response is
// over the capture budget) ends its window before the connection's next
// request, not when the connection closes: a window left open holds every
// droppable mock the app makes meanwhile.
func TestHandleHttp1ZeroCopy_EndsTheWindowOfAnExchangeItDoesNotCapture(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	mgr := syncMock.New(zap.NewNop())
	big := strings.Repeat("x", maxHTTPBodyCaptureBytes+1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			_, _ = w.Write([]byte(big))
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)
	captured := make(chan string, 4)
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
		captured <- req.URL.Path
	})
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
	defer cancel()
	client, done := ingressPipe(t, ctx, pm, strings.TrimPrefix(upstream.URL, "http://"))
	br := bufio.NewReader(client)

	if _, err := client.Write([]byte("GET /big HTTP/1.1\r\nHost: app.local\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	waitOpenWindows(t, mgr, 0, "after a keep-alive exchange that is not captured, before the next request")

	if _, err := client.Write([]byte("GET /small HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if resp, err = http.ReadResponse(br, nil); err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	select {
	case p := <-captured:
		if p != "/small" {
			t.Fatalf("captured %s, want only /small", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the next exchange was not captured")
	}
	<-done
	waitOpenWindows(t, mgr, 0, "after the connection ended")
}

// An Upgrade (a WebSocket) becomes a raw tunnel that nothing will ever decide
// as an HTTP exchange: it opens no window, however long the tunnel lasts.
func TestHandleHttp1ZeroCopy_OpensNoWindowForAnUpgradeTunnel(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	mgr := syncMock.New(zap.NewNop())
	up, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = up.Close() })
	go func() {
		c, aerr := up.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		if _, rerr := http.ReadRequest(r); rerr != nil {
			return
		}
		_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
		_, _ = io.Copy(c, r) // echo the tunnel
	}()
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
		t.Error("an Upgrade tunnel was captured")
	})
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
	defer cancel()
	client, done := ingressPipe(t, ctx, pm, up.Addr().String())
	br := bufio.NewReader(client)
	if _, err := client.Write([]byte("GET /ws HTTP/1.1\r\nHost: app.local\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(br, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("tunnel echo %q %v", echo, err)
	}
	if n := mgr.OpenWindows(); n != 0 {
		t.Fatalf("%d windows open while an Upgrade tunnel runs, want none", n)
	}
	_ = client.Close()
	<-done
}

// An exchange that ends early — the upstream closes without a response —
// ends its window on the way out, in both ingress loops.
func TestHandleHttp1Connection_EndsTheWindowWhenTheUpstreamFails(t *testing.T) {
	for _, synchronous := range []bool{true, false} {
		t.Run(map[bool]string{true: "synchronous", false: "zero-copy"}[synchronous], func(t *testing.T) {
			stubIngressPaused(t, func() bool { return false })
			mgr := syncMock.New(zap.NewNop())
			up, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = up.Close() })
			go func() {
				for {
					c, aerr := up.Accept()
					if aerr != nil {
						return
					}
					go func() {
						_, _ = http.ReadRequest(bufio.NewReader(c))
						_ = c.Close() // no response
					}()
				}
			}()
			stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
				req *http.Request, resp *http.Response, reqTS, respTS time.Time,
				opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
				t.Error("an exchange with no response was captured")
			})
			pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), synchronous: synchronous, samplingSem: make(chan struct{}, 1)}
			ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
			defer cancel()
			client, done := ingressPipe(t, ctx, pm, up.Addr().String())
			if _, err := client.Write([]byte("POST /x HTTP/1.1\r\nHost: app.local\r\nContent-Length: 2\r\n\r\nhi")); err != nil {
				t.Fatal(err)
			}
			// Whatever the client gets (a 502, or a closed connection), the
			// exchange is over: its window must be too, connection open or not.
			_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _ = http.ReadResponse(bufio.NewReader(client), nil)
			waitOpenWindows(t, mgr, 0, "after an exchange that ended early")
			_ = client.Close()
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				t.Fatal("the handler did not return")
			}
		})
	}
}

// The reproduced bug, end to end through the OSS ingress, in both loops. The app makes an egress call as soon as it has the request; a
// duplicate decided meanwhile prunes (the mock is held for the exchange in
// flight); the response comes; the capture keeps the request. The mock must be
// recorded with it: one window, the same from the request's first byte to its
// verdict (a window ended and opened again would have dropped it).
// Synchronous: the real hooksUtils.Capture resolves. Zero-copy: a hook that
// decides like the enterprise capture does.
func TestHandleHttp1Connection_KeptExchangeRecordsAMockHeldForIt(t *testing.T) {
	for _, synchronous := range []bool{true, false} {
		t.Run(map[bool]string{true: "synchronous", false: "zero-copy"}[synchronous], func(t *testing.T) {
			stubIngressPaused(t, func() bool { return false })
			mgr := syncMock.New(zap.NewNop())
			out := make(chan *models.Mock, 16)
			mgr.SetOutputChannel(out)
			mgr.SetFirstRequestSignaled()
			old := time.Now().Add(-time.Minute)
			for i := 0; i < models.StartupMockTestCaseWindow; i++ {
				mgr.ResolveRange(old, old, "", true, false)
			}

			var early *models.Mock
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				early = &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: time.Now()},
					TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
				mgr.AddMock(early)
				time.Sleep(2 * time.Millisecond)
				mgr.DeleteMocksStrictlyBefore(time.Now()) // a duplicate decided meanwhile
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			t.Cleanup(upstream.Close)

			if !synchronous {
				stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
					req *http.Request, resp *http.Response, reqTS, respTS time.Time,
					opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
					win := syncMock.WindowFromContext(ctx)
					if win.Keep() {
						syncMock.FromContextOrGlobal(ctx).ResolveKept(win, reqTS, respTS, "t", false)
						tc <- &models.TestCase{Name: "t"}
					}
				})
			} else {
				stubCaptureHook(t, hooksUtils.Capture)
			}
			pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), synchronous: synchronous, samplingSem: make(chan struct{}, 1)}
			ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
			defer cancel()
			client, done := ingressPipe(t, ctx, pm, strings.TrimPrefix(upstream.URL, "http://"))
			if _, err := client.Write([]byte("GET /slow/1 HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			select {
			case <-pm.tcChan:
			case <-time.After(5 * time.Second):
				t.Fatal("the exchange was not recorded")
			}
			<-done
			waitOpenWindows(t, mgr, 0, "after the capture")
			found := false
			for len(out) > 0 {
				if <-out == early {
					found = true
				}
			}
			if !found {
				t.Fatal("the kept exchange was recorded WITHOUT the mock it made while it was in flight")
			}
		})
	}
}

// An exchange's window runs to the LAST byte of its response, in both ingress
// loops: an egress call the app makes while it sends the body (a stream that
// fetches as it goes, a handler that flushes its headers and then calls out) is
// the exchange's own, and the time the capture is given as the response's is
// the window's end, for the resolve and for the recorded test case alike. The
// synchronous loop stamped it once the response's HEADERS were read, so the
// resolve stopped short of every call made after them: a duplicate's prune held
// such a mock for the exchange in flight, and the window's end then dropped it
// as outside any window (without a prune it stayed behind for the stale
// cutoff). The exchange was recorded without it, with nothing counted or
// warned of.
//
// Every framing of a body: a chunked stream, which gives the synchronous lock
// back at its headers; a response of a known length, which keeps it to the
// end; and a body that ends when the app closes the connection (how a server
// streams to an HTTP/1.0 client, and one way to send server-sent events),
// whose last byte is that close, so a call made after its last data and before
// the close is the exchange's too. Synchronous: the real hooksUtils.Capture
// resolves. Zero-copy: a hook that decides like the enterprise capture does.
func TestHandleHttp1Connection_KeptExchangeRecordsAMockItMadeWhileSendingItsBody(t *testing.T) {
	const first, last = "data: 1\n\n", "data: 2\n\n"
	for _, framing := range []string{"chunked", "content-length", "until-close"} {
		for _, synchronous := range []bool{true, false} {
			name := framing + "/" + map[bool]string{true: "synchronous", false: "zero-copy"}[synchronous]
			t.Run(name, func(t *testing.T) {
				stubIngressPaused(t, func() bool { return false })
				mgr := syncMock.New(zap.NewNop())
				out := make(chan *models.Mock, 16)
				mgr.SetOutputChannel(out)
				mgr.SetFirstRequestSignaled()
				old := time.Now().Add(-time.Minute)
				for i := 0; i < models.StartupMockTestCaseWindow; i++ {
					mgr.ResolveRange(old, old, "", true, false)
				}

				// An egress call of the app's: its mock, and when it was made.
				type egress struct {
					when string
					mock *models.Mock
				}
				made := make(chan egress, 3)
				call := func(when string) {
					m := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: time.Now()},
						TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
					mgr.AddMock(m)
					made <- egress{when, m}
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					call("before its response headers")
					w.Header().Set("Content-Type", "text/event-stream")
					switch framing {
					case "content-length":
						w.Header().Set("Content-Length", strconv.Itoa(len(first)+len(last)))
					case "until-close":
						w.Header().Set("Transfer-Encoding", "identity") // no chunking: the close ends the body
					}
					_, _ = w.Write([]byte(first))
					w.(http.Flusher).Flush() // the headers and the first part are out
					time.Sleep(50 * time.Millisecond)
					call("while it sent its response body")
					time.Sleep(5 * time.Millisecond)
					mgr.DeleteMocksStrictlyBefore(time.Now()) // a duplicate decided meanwhile
					_, _ = w.Write([]byte(last))
					if framing == "until-close" {
						w.(http.Flusher).Flush() // the last data is out, the body is not over
						time.Sleep(20 * time.Millisecond)
						call("after its last data, before the close that ends its body")
						time.Sleep(5 * time.Millisecond)
					}
				}))
				t.Cleanup(upstream.Close)

				if !synchronous {
					stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
						req *http.Request, resp *http.Response, reqTS, respTS time.Time,
						opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
						win := syncMock.WindowFromContext(ctx)
						if win.Keep() {
							syncMock.FromContextOrGlobal(ctx).ResolveKept(win, reqTS, respTS, "t", false)
							tc <- &models.TestCase{Name: "t",
								HTTPReq:  models.HTTPReq{Timestamp: reqTS},
								HTTPResp: models.HTTPResp{Timestamp: respTS}}
						}
					})
				} else {
					stubCaptureHook(t, hooksUtils.Capture)
				}
				pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), synchronous: synchronous, samplingSem: make(chan struct{}, 1)}
				ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
				defer cancel()
				client, done := ingressPipe(t, ctx, pm, strings.TrimPrefix(upstream.URL, "http://"))
				if _, err := client.Write([]byte("GET /stream/1 HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n")); err != nil {
					t.Fatal(err)
				}
				_ = client.SetReadDeadline(time.Now().Add(8 * time.Second)) // a body that never ends fails the test, not its run
				resp, err := http.ReadResponse(bufio.NewReader(client), nil)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if string(body) != first+last {
					t.Fatalf("fixture: the client read %q, want the whole body", body)
				}
				got := "until-close"
				switch {
				case len(resp.TransferEncoding) > 0:
					got = "chunked"
				case resp.ContentLength >= 0:
					got = "content-length"
				}
				if got != framing {
					t.Fatalf("fixture: the response came framed %s, want %s", got, framing)
				}
				var tc *models.TestCase
				select {
				case tc = <-pm.tcChan:
				case <-time.After(5 * time.Second):
					t.Fatal("the exchange was not recorded")
				}
				<-done
				waitOpenWindows(t, mgr, 0, "after the capture")

				recorded := map[*models.Mock]bool{}
				for len(out) > 0 {
					recorded[<-out] = true
				}
				calls := 2
				if framing == "until-close" {
					calls = 3
				}
				for i := 0; i < calls; i++ {
					c := <-made
					// The recorded window covers the call: replay picks a test
					// case's mocks by its request and response times.
					if at := c.mock.Spec.ReqTimestampMock; at.Before(tc.HTTPReq.Timestamp) || at.After(tc.HTTPResp.Timestamp) {
						t.Errorf("the test case's window [%s, %s] leaves out the call the app made %s, at %s: the response's time is not its last byte's",
							tc.HTTPReq.Timestamp.Format(time.RFC3339Nano), tc.HTTPResp.Timestamp.Format(time.RFC3339Nano), c.when, at.Format(time.RFC3339Nano))
					}
					if !recorded[c.mock] {
						t.Errorf("the kept exchange was recorded WITHOUT the mock of the call it made %s", c.when)
					}
				}
				// None is left behind for a later reaper to drop.
				if _, _, _, buffered := mgr.GetDropStats(); buffered != 0 {
					t.Errorf("%d mocks left in the buffer after the exchange was recorded, want none", buffered)
				}
			})
		}
	}
}

// The synchronous loop gives its lock back at the headers of a response of
// unknown length (or at a request body of unknown length), and the next request
// runs while the rest of the exchange is still going. The first exchange's
// window runs to its response's last byte, which a slow client paces once the
// body outgrows what the read-ahead holds, so it can span the next request's
// calls; but past the first's response headers a call made within the next request's
// claim is the next request's (see syncMock.Window.Yield), whichever of the
// two is decided first. What the first does that no other request claims is
// its own.
//
//   - a body sent at once: a handler that makes its call and then writes a
//     chunked JSON body in one go (Go chunks any handler output over 2 KiB
//     without a Content-Length), to a slow client, a body larger than the
//     read-ahead holds (lowered here, and the app's socket buffer with it), so
//     the app waits on the client and its last byte comes at the client's
//     pace. The next request, waiting on the lock, makes its call at once and
//     answers 300 ms later, so the first is decided first. Were its window to
//     take every call up to its last byte, it would take the next request's:
//     mapped to the wrong test case, which mapping-based replay of the next
//     request cannot find.
//   - a stream that calls out after: the stream calls out again once the next
//     request has been answered, before its body ends; the next request's
//     capture is decided only after the stream's, so its window is still open
//     then, and must claim nothing after its last byte.
//   - a body sent at once, then a stream: as the first, and the next request is
//     a stream that calls out after its headers, once the app has sent all of
//     the first's body (the ingress has read its close), while the first's
//     client is still slow to take it. Both windows yielded at their headers;
//     the first's ends when its last byte came from the app, not when its
//     client took it, so the call is the stream's.
//   - a request body sent in chunks: the lock is given back before the request
//     reaches the app; the response has a known length and its body goes on
//     after its headers, while the next request makes its call. The next
//     request calls out only once the first's headers are read: before them
//     the two windows overlap with neither yielded, and the first resolved
//     takes what both span, as on main.
func TestHandleHttp1Connection_SyncWindowsShareABodyOnlyWithWhatNoOtherRequestClaims(t *testing.T) {
	const (
		atOnce       = "a body sent at once"
		atOnceStream = "a body sent at once, then a stream"
		stream       = "a stream that calls out after"
		chunkReq     = "a request body sent in chunks"
	)
	for _, shape := range []string{atOnce, atOnceStream, stream, chunkReq} {
		t.Run(shape, func(t *testing.T) {
			stubIngressPaused(t, func() bool { return false })
			if shape == atOnce { // past the read-ahead's bound: the app waits on the client
				prev := aheadMax
				aheadMax = 8 << 10
				t.Cleanup(func() { aheadMax = prev })
			}
			mgr := syncMock.New(zap.NewNop())
			out := make(chan *models.Mock, 16)
			mgr.SetOutputChannel(out)
			maps := make(chan models.TestMockMapping, 16)
			mctx, mcancel := context.WithCancel(context.Background())
			defer mcancel()
			mgr.SetMappingChannel(mctx, maps)
			mgr.SetFirstRequestSignaled()
			old := time.Now().Add(-time.Minute)
			for i := 0; i < models.StartupMockTestCaseWindow; i++ {
				mgr.ResolveRange(old, old, "", true, false)
			}
			call := func() *models.Mock {
				m := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: time.Now()},
					TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
				mgr.AddMock(m)
				return m
			}

			up, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = up.Close() })
			var first, next, after *models.Mock // the first exchange's call, the next request's, the stream's after it
			nextMade := make(chan struct{})
			nextAnswered := make(chan struct{})
			firstHeadersRead := make(chan struct{}) // by the first exchange's client
			firstSent := make(chan struct{})        // the ingress has read the first's close
			nextHeadersRead := make(chan struct{})  // by the next request's client
			// The first request reached the app: its handler took the lock
			// before it sent it on, so the next request's waits on it.
			firstArrived := make(chan struct{})
			if shape == atOnceStream {
				var once sync.Once
				seen := func(n int, err error) {
					if errors.Is(err, io.EOF) { // only the first's app closes before the end
						once.Do(func() { close(firstSent) })
					}
				}
				aheadReadSeen.Store(&seen)
				t.Cleanup(func() { aheadReadSeen.Store(nil) })
			}
			wait := func(c chan struct{}) bool {
				select {
				case <-c:
					return true
				case <-time.After(5 * time.Second):
					return false
				}
			}
			go func() {
				for {
					c, aerr := up.Accept()
					if aerr != nil {
						return
					}
					go func(c net.Conn) {
						defer c.Close()
						br := bufio.NewReader(c)
						req, rerr := http.ReadRequest(br)
						if rerr != nil {
							return
						}
						switch req.URL.Path {
						case "/first":
							close(firstArrived)
							if _, rerr := io.Copy(io.Discard, req.Body); rerr != nil {
								return
							}
							first = call()
							const chunked = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"
							switch shape {
							case atOnce, atOnceStream:
								body := strings.Repeat("x", 64<<10)
								if shape == atOnce {
									_ = c.(*net.TCPConn).SetWriteBuffer(16 << 10)
									body = strings.Repeat("x", 1<<20)
								}
								_, _ = c.Write([]byte(chunked + strconv.FormatInt(int64(len(body)), 16) + "\r\n" + body + "\r\n0\r\n\r\n"))
							case stream:
								_, _ = c.Write([]byte(chunked + "8\r\ndata: 1\n\r\n"))
								if !wait(nextAnswered) {
									return
								}
								time.Sleep(5 * time.Millisecond)
								after = call()
								time.Sleep(5 * time.Millisecond)
								_, _ = c.Write([]byte("8\r\ndata: 2\n\r\n0\r\n\r\n"))
							case chunkReq:
								_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 4\r\n\r\n{\""))
								if !wait(nextMade) {
									return
								}
								time.Sleep(5 * time.Millisecond)
								_, _ = c.Write([]byte("a}"))
							}
						case "/next":
							if shape == chunkReq && !wait(firstHeadersRead) {
								return
							}
							next = call()
							if shape == atOnceStream {
								_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n8\r\ndata: 1\n\r\n"))
								// After the ingress read the first's last byte (and
								// its close), and this one's headers: past them.
								if !wait(firstSent) || !wait(nextHeadersRead) {
									return
								}
								after = call()
								close(nextMade)
								_, _ = c.Write([]byte("8\r\ndata: 2\n\r\n0\r\n\r\n"))
								return
							}
							close(nextMade)
							if shape != stream {
								time.Sleep(300 * time.Millisecond) // the first is decided first
							}
							_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
						}
					}(c)
				}
			}()

			firstDecided := make(chan struct{})
			stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
				req *http.Request, resp *http.Response, reqTS, respTS time.Time,
				opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
				if req.URL.Path == "/next" && (shape == stream || shape == atOnceStream) {
					wait(firstDecided)
				}
				hooksUtils.Capture(ctx, logger, tc, req, resp, reqTS, respTS, opts, sync, mapping, appPort)
				if req.URL.Path == "/first" {
					close(firstDecided)
				}
			})
			pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), synchronous: true, mapping: true, samplingSem: make(chan struct{}, 1)}
			ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
			defer cancel()
			lock := make(chan struct{}, 1)
			gate := make(chan struct{})
			var openOnce sync.Once
			openGate := func() { openOnce.Do(func() { close(gate) }) }
			var wrap func(net.Conn) net.Conn
			if shape == atOnce || shape == atOnceStream {
				wrap = func(c net.Conn) net.Conn { return gatedWriteConn{Conn: c, gate: gate} } // a slow client
			} else {
				openGate()
			}
			c1, done1 := ingressPipeOn(t, ctx, pm, up.Addr().String(), lock, wrap)
			t.Cleanup(openGate) // runs before ingressPipeOn's cleanups: a failure still lets the handler finish
			firstReq := "GET /first HTTP/1.1\r\nHost: app.local\r\n\r\n"
			if shape == chunkReq {
				firstReq = "POST /first HTTP/1.1\r\nHost: app.local\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\n\r\n"
			}
			if _, err := c1.Write([]byte(firstReq)); err != nil {
				t.Fatal(err)
			}
			// The next request's connection opens only once the first's
			// handler has taken the lock. Both handlers take it as they
			// start, so one opened before could take it first, and the
			// exchanges would not run in the order each shape means: with a
			// request body sent in chunks, the next request's known-length
			// exchange would hold it while its app waits on the first's
			// headers, which the first cannot get without it.
			if !wait(firstArrived) {
				t.Fatal("the first request never reached the app")
			}
			c2, done2 := ingressPipeOn(t, ctx, pm, up.Addr().String(), lock, nil) // waits on the lock, or takes it given back
			if _, err := c2.Write([]byte("GET /next HTTP/1.1\r\nHost: app.local\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			_ = c1.SetReadDeadline(time.Now().Add(8 * time.Second))
			_ = c2.SetReadDeadline(time.Now().Add(8 * time.Second))
			readHeaders := func(c net.Conn) *http.Response {
				t.Helper()
				resp, rerr := http.ReadResponse(bufio.NewReader(c), nil)
				if rerr != nil {
					t.Fatal(rerr)
				}
				return resp
			}
			readAll := func(resp *http.Response) {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			switch shape {
			case atOnce:
				if !wait(nextMade) {
					t.Fatal("the next request never reached the app: the lock was not given back at the first one's headers")
				}
				time.Sleep(10 * time.Millisecond)
				openGate()
				readAll(readHeaders(c1))
				readAll(readHeaders(c2))
			case atOnceStream:
				r2 := readHeaders(c2) // the lock was given back at the first's headers
				close(nextHeadersRead)
				if !wait(nextMade) {
					t.Fatal("the next stream never called out")
				}
				time.Sleep(10 * time.Millisecond)
				openGate()
				readAll(readHeaders(c1))
				readAll(r2)
			case stream:
				readAll(readHeaders(c2))
				close(nextAnswered)
				readAll(readHeaders(c1))
			case chunkReq:
				r1 := readHeaders(c1) // the proxy has read them: the next request may call out
				close(firstHeadersRead)
				readAll(r1)
				readAll(readHeaders(c2))
			}
			<-done1
			<-done2
			names := map[string]string{} // path -> test case name
			for i := 0; i < 2; i++ {
				select {
				case tc := <-pm.tcChan:
					names[strings.TrimPrefix(tc.HTTPReq.URL, "http://app.local")] = tc.Name
				case <-time.After(5 * time.Second):
					t.Fatal("an exchange was not recorded")
				}
			}
			waitOpenWindows(t, mgr, 0, "after both were decided")

			owner := map[string]string{}
			for len(maps) > 0 {
				e := <-maps
				for _, id := range e.MockIDs {
					if prev, dup := owner[id]; dup && prev != e.TestName {
						t.Errorf("mock %s mapped to %s and %s", id, prev, e.TestName)
					}
					owner[id] = e.TestName
				}
			}
			check := func(what string, m *models.Mock, path string) {
				t.Helper()
				if m == nil {
					t.Fatalf("fixture: %s was never made", what)
				}
				if got := owner[m.Name]; got != names[path] {
					t.Errorf("%s went to %q, want %s's test case %q", what, got, path, names[path])
				}
			}
			check("the first exchange's call", first, "/first")
			check("the call the next request made within its claim", next, "/next")
			switch shape {
			case stream:
				check("the call the stream made after the next request was answered", after, "/first")
			case atOnceStream:
				check("the call the next stream made after the first's app had sent all of its body", after, "/next")
			}
		})
	}
}

// gatedWriteConn is a connection whose writes wait for gate: a client that is
// slow to take what it is sent.
type gatedWriteConn struct {
	net.Conn
	gate <-chan struct{}
}

func (c gatedWriteConn) Write(p []byte) (int, error) {
	<-c.gate
	return c.Conn.Write(p)
}

// The synchronous loop gives its lock back at the headers of a response of
// unknown length (a chunked JSON body, as most APIs send), and the next request
// is read at once. The response's time is its last byte's as the app sent it,
// not the moment the ingress was done forwarding it: for a body read whole with
// its headers that is before the lock is given back, however long the client
// takes to read it. Otherwise the exchange's window would run into the next
// request's, and its resolve, which comes first, would take the mocks of the
// calls the next request made early on: the two are one after the other for the
// app, which is what the synchronous loop is for.
func TestHandleHttp1Connection_SyncResponseReadWholeEndsBeforeTheLockIsGivenBack(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	mgr := syncMock.New(zap.NewNop())

	up, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = up.Close() })
	atApp := make(chan struct{})
	go func() {
		c, aerr := up.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		if _, rerr := http.ReadRequest(bufio.NewReader(c)); rerr != nil {
			return
		}
		close(atApp)
		// Headers and the whole chunked body in one segment: one read.
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\nb\r\n{\"ok\":true}\r\n0\r\n\r\n"))
	}()

	respTime := make(chan time.Time, 1)
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
		respTime <- respTS
	})
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), synchronous: true, samplingSem: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
	defer cancel()
	lock := make(chan struct{}, 1)
	gate := make(chan struct{})
	var openOnce sync.Once
	openGate := func() { openOnce.Do(func() { close(gate) }) }
	client, done := ingressPipeOn(t, ctx, pm, up.Addr().String(), lock, func(c net.Conn) net.Conn {
		return gatedWriteConn{Conn: c, gate: gate}
	})
	// Registered after ingressPipeOn's cleanups, so it runs before them: a
	// failure still lets the handler finish its write and return.
	t.Cleanup(openGate)
	if _, err := client.Write([]byte("GET /api HTTP/1.1\r\nHost: app.local\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-atApp: // the handler holds the lock: it took it before it dialled the app
	case <-time.After(5 * time.Second):
		t.Fatal("fixture: the request never reached the app")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(lock) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the lock was not given back at the headers of a chunked response")
		}
		time.Sleep(200 * time.Microsecond)
	}
	freed := time.Now() // the next request may be read from here on
	time.Sleep(30 * time.Millisecond)
	openGate() // the client takes the response only now

	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"ok":true}` {
		t.Fatalf("fixture: the client read %q", body)
	}
	select {
	case at := <-respTime:
		if at.After(freed) {
			t.Fatalf("the response's time is %s after the lock was given back: its window runs into the next request's, though its last byte was read with its headers",
				at.Sub(freed))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the exchange was not captured")
	}
	<-done
	waitOpenWindows(t, mgr, 0, "after the capture")
}

// A request that kept the lock is in flight no more once its response's last
// byte came from the app, though its capture has not decided it yet (the
// window stays open until it has): the synchronous loop yields its window
// there. A stream that runs after it, and gave the lock back at its headers,
// claims a call it makes past them, and its own resolve hands that call on at
// once. Without the yield the first request still claimed everything after its
// start until its capture decided it: the call was held for it, counted
// against the hold's budget, and reached the stream's test case only once the
// first one's capture was done.
func TestHandleHttp1Connection_SyncWindowYieldsAtItsLastByte(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	mgr := syncMock.New(zap.NewNop())
	out := make(chan *models.Mock, 16)
	mgr.SetOutputChannel(out)
	maps := make(chan models.TestMockMapping, 16)
	mctx, mcancel := context.WithCancel(context.Background())
	defer mcancel()
	mgr.SetMappingChannel(mctx, maps)
	mgr.SetFirstRequestSignaled()
	old := time.Now().Add(-time.Minute)
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		mgr.ResolveRange(old, old, "", true, false)
	}

	wait := func(c chan struct{}) bool {
		select {
		case <-c:
			return true
		case <-time.After(5 * time.Second):
			return false
		}
	}
	up, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = up.Close() })
	var after *models.Mock // the call the stream makes past its headers
	streamHeadersRead := make(chan struct{})
	callMade := make(chan struct{})
	go func() {
		for {
			c, aerr := up.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				req, rerr := http.ReadRequest(bufio.NewReader(c))
				if rerr != nil {
					return
				}
				switch req.URL.Path {
				case "/kept":
					_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
				case "/stream":
					_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n8\r\ndata: 1\n\r\n"))
					if !wait(streamHeadersRead) {
						return
					}
					after = &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: time.Now()},
						TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
					mgr.AddMock(after)
					close(callMade)
					_, _ = c.Write([]byte("8\r\ndata: 2\n\r\n0\r\n\r\n"))
				}
			}(c)
		}
	}()

	decideKept := make(chan struct{})
	var decideOnce sync.Once
	letKeptBeDecided := func() { decideOnce.Do(func() { close(decideKept) }) }
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
		if req.URL.Path == "/kept" {
			wait(decideKept) // its window stays open, past its last byte
		}
		hooksUtils.Capture(ctx, logger, tc, req, resp, reqTS, respTS, opts, sync, mapping, appPort)
	})
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), synchronous: true, mapping: true, samplingSem: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 10*time.Second)
	defer cancel()
	lock := make(chan struct{}, 1)

	c1, done1 := ingressPipeOn(t, ctx, pm, up.Addr().String(), lock, nil)
	t.Cleanup(letKeptBeDecided) // runs before ingressPipeOn's cleanups: a failure still lets the capture finish
	if _, err := c1.Write([]byte("GET /kept HTTP/1.1\r\nHost: app.local\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	c2, done2 := ingressPipeOn(t, ctx, pm, up.Addr().String(), lock, nil) // waits on the lock
	if _, err := c2.Write([]byte("GET /stream HTTP/1.1\r\nHost: app.local\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = c1.SetReadDeadline(time.Now().Add(8 * time.Second))
	_ = c2.SetReadDeadline(time.Now().Add(8 * time.Second))
	r1, err := http.ReadResponse(bufio.NewReader(c1), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, r1.Body)
	r1.Body.Close()
	_ = c1.Close() // as its client does, told Connection: close: the lock is given back
	<-done1

	r2, err := http.ReadResponse(bufio.NewReader(c2), nil)
	if err != nil {
		t.Fatal(err)
	}
	close(streamHeadersRead)
	_, _ = io.Copy(io.Discard, r2.Body)
	r2.Body.Close()
	<-done2

	var streamName string
	select {
	case tc := <-pm.tcChan:
		if !strings.HasSuffix(tc.HTTPReq.URL, "/stream") {
			t.Fatalf("%s was recorded while its capture was held", tc.HTTPReq.URL)
		}
		streamName = tc.Name
	case <-time.After(5 * time.Second):
		t.Fatal("the stream was not recorded while the first request's capture was held")
	}
	if !wait(callMade) {
		t.Fatal("fixture: the stream never called out")
	}
	if n := mgr.OpenWindows(); n != 1 {
		t.Fatalf("%d windows open, want the first request's: its capture is held", n)
	}
	handedOn := false
	for len(maps) > 0 {
		e := <-maps
		for _, id := range e.MockIDs {
			if id == after.Name && e.TestName == streamName {
				handedOn = true
			}
		}
	}
	if !handedOn {
		t.Fatal("the stream's call past its headers was not handed on with its resolve: the first request, past its last byte, still claimed it")
	}

	letKeptBeDecided()
	select {
	case <-pm.tcChan:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request was not recorded once its capture went on")
	}
	waitOpenWindows(t, mgr, 0, "after both were decided")
}
