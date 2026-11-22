package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/agent/memoryguard"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.keploy.io/server/v3/utils/plaintcp/plaintcptest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func newTestIngressHook() *goTCPIngressHook {
	return newGoTCPIngressHook(&IngressProxyManager{logger: zap.NewNop()})
}

// startIngressAtFreePort starts hook's forwarder at a port no other socket
// is given while the test runs (testnet.ReservedPort) and returns the port.
func startIngressAtFreePort(t *testing.T, ctx context.Context, hook *goTCPIngressHook) uint16 {
	t.Helper()
	port := uint16(testnet.ReservedPort(t))
	if err := hook.StartIngress(ctx, port, 0); err != nil {
		t.Fatalf("StartIngress: %v", err)
	}
	return port
}

// bindAgain binds port at every IPv4 address and closes it, as the next
// run's app would. On Linux it first fails when a socket of this process is
// at the port (testnet.BoundHere). It is nil when the bind succeeds, and on
// Linux also when the bind failed and no socket of this process is at the
// port after it either: another process took the port once it was free, and
// may have let it go again. Else it is the bind's error. A caller whose code
// under test may drop a listener holds the finalizers first
// (testnet.HoldFinalizers), so a listener the code left open is still open
// here.
func bindAgain(t *testing.T, port uint16) error {
	t.Helper()
	return bindAgainBetween(t, port, nil)
}

// bindAgainBetween is bindAgain that calls between, when it is not nil,
// after its first look at the port and before its bind.
func bindAgainBetween(t *testing.T, port uint16, between func()) error {
	t.Helper()
	if n, known := testnet.BoundHere(t, int(port)); known && n > 0 {
		return fmt.Errorf("%d socket(s) of this process still at port %d", n, port)
	}
	if between != nil {
		between()
	}
	ln, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(int(port)))
	if err == nil {
		_ = ln.Close()
		return nil
	}
	if n, known := testnet.BoundHere(t, int(port)); known && n == 0 {
		return nil
	}
	return err
}

// forwarderOf is the state of hook's forwarder at port. The test keeps it,
// so its listener stays reachable whatever the code under test does with it.
func forwarderOf(t *testing.T, hook *goTCPIngressHook, port uint16) *tcpForwarderState {
	t.Helper()
	hook.mu.Lock()
	defer hook.mu.Unlock()
	st, ok := hook.forwarders[port]
	if !ok {
		t.Fatalf("no forwarder registered for port %d", port)
	}
	return st
}

// listenerClosed reports whether ln was closed: the descriptor of a closed
// listener takes no more calls (net.ErrClosed).
func listenerClosed(t *testing.T, ln net.Listener) bool {
	t.Helper()
	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	err = rc.Control(func(uintptr) {})
	if err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Control: %v", err)
	}
	return err != nil
}

// bindAgain passes a port another process took once it was free, and fails
// one a socket of this process still holds.
func TestBindAgainTellsAPortAnotherProcessTookFromOneThisProcessHolds(t *testing.T) {
	testnet.HoldFinalizers(t)
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	if err := bindAgain(t, port); err == nil {
		t.Fatalf("bindAgain passed port %d while this test's listener holds it", port)
	}
	testnet.GiveAway(t, ln.(*net.TCPListener))
	if err := bindAgain(t, port); err != nil {
		t.Fatalf("bindAgain failed port %d, which another process took: %v", port, err)
	}
}

// bindAgain fails a port a socket of this process is still at even where
// the bind succeeds. A connection accepted at the port outlives the
// listener that accepted it. It keeps the SO_REUSEADDR its listener set.
// net.Listen sets it on both sockets. On Linux the new listener then binds
// beside the connection.
func TestBindAgainFailsAPortAConnectionThisProcessAcceptedIsStillAt(t *testing.T) {
	if _, known := testnet.BoundHere(t, 0); !known {
		t.Skip("BoundHere cannot tell here whose socket is at a port")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	s, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := ln.Close(); err != nil {
		t.Fatalf("close the listener: %v", err)
	}
	if err := bindAgain(t, port); err == nil {
		t.Fatalf("bindAgain passed port %d, where a connection this test accepted is still open", port)
	}
}

// bindAgain looks at the port again after a failed bind: a socket of this
// process that came to the port after its first look fails it.
// testnet.SharedPort holds the port. BoundHere does not count the hold's
// socket, so the first look finds nothing. The bind fails beside the hold
// either way.
func TestBindAgainFailsAPortASocketOfThisProcessTookAfterItsFirstLook(t *testing.T) {
	if _, known := testnet.BoundHere(t, 0); !known {
		t.Skip("BoundHere cannot tell here whose socket is at a port")
	}
	port := testnet.SharedPort(t)
	err := bindAgainBetween(t, uint16(port), func() {
		testnet.ListenShared(t, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	})
	if err == nil {
		t.Fatalf("bindAgain passed port %d, where this test listened after its first look", port)
	}
}

// A forwarder must NOT bind the app port when the agent context is already
// canceled (shutting down). Binding during teardown leaves the listener holding
// the port, so the next record run's application fails to bind it with "address
// already in use" — the flaky port-8000 reuse failure.
func TestStartIngressSkipsBindWhenContextCanceled(t *testing.T) {
	testnet.HoldFinalizers(t)
	hook := newTestIngressHook()
	port := uint16(testnet.ReservedPort(t))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // agent already shutting down

	if err := hook.StartIngress(ctx, port, 0); err == nil {
		t.Fatal("expected StartIngress to abort on a canceled context, got nil")
	}

	// The port must be free — the fix must not have bound it.
	if err := bindAgain(t, port); err != nil {
		t.Fatalf("port %d should be free after a canceled StartIngress, but bind failed: %v", port, err)
	}

	hook.mu.Lock()
	_, registered := hook.forwarders[port]
	hook.mu.Unlock()
	if registered {
		t.Fatalf("no forwarder should be registered for port %d after a canceled StartIngress", port)
	}
}

// The normal path must bind the port and fully release it on StopIngress, so a
// subsequent run can rebind it immediately.
func TestStartIngressReleasesPortOnStop(t *testing.T) {
	testnet.HoldFinalizers(t)
	hook := newTestIngressHook()
	port := startIngressAtFreePort(t, context.Background(), hook)
	st := forwarderOf(t, hook, port)

	// Port is bound now.
	if ln, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(int(port))); err == nil {
		_ = ln.Close()
		t.Fatalf("port %d should be bound by the running forwarder", port)
	}

	if err := hook.StopIngress(port); err != nil {
		t.Fatalf("StopIngress: %v", err)
	}
	if !listenerClosed(t, st.listener) {
		t.Fatalf("StopIngress returned with the forwarder's listener at port %d open", port)
	}

	// Port must be released.
	if err := bindAgain(t, port); err != nil {
		t.Fatalf("port %d should be free after StopIngress, but bind failed: %v", port, err)
	}
}

// The forwarder listens at the application's own port, on every interface, so
// it is plain TCP: Go's default would make that port MPTCP, which the
// application never chose.
func TestStartIngressListensPlainTCP(t *testing.T) {
	hook := newTestIngressHook()
	port := startIngressAtFreePort(t, context.Background(), hook)
	t.Cleanup(func() { _ = hook.StopIngress(port) })
	plaintcptest.RequirePlainTCP(t, forwarderOf(t, hook, port).listener)
}

// newCapturingIngressHook is a forwarder hook whose manager records into
// nothing and has holder (nil: no hold) hold the moved ports; each recorded
// call's path arrives on the channel, and its log, debug level up, in the
// logs.
func newCapturingIngressHook(t *testing.T, holder IngressPortHolder) (*goTCPIngressHook, <-chan string, *observer.ObservedLogs) {
	t.Helper()
	stubIngressPaused(t, func() bool { return false })
	captured := make(chan string, 4)
	stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase,
		req *http.Request, _ *http.Response, _ time.Time, _ time.Time,
		_ models.IncomingOptions, _ bool, _ bool, _ uint16) {
		captured <- req.URL.Path
	})
	core, logs := observer.New(zapcore.DebugLevel)
	pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
	t.Cleanup(pm.captures.Wait)
	return newGoTCPIngressHook(pm), captured, logs
}

// requireIPv6 skips the test where this host has no IPv6 loopback.
func requireIPv6(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	_ = ln.Close()
}

// answeredAndRecorded calls the forwarder over c, and fails the test unless
// the application's answer comes back and the call is recorded.
func answeredAndRecorded(t *testing.T, c net.Conn, captured <-chan string, how string) {
	t.Helper()
	if status := <-callAndRead(t, c, "/v6"); status != "200 OK" {
		t.Fatalf("a call %s: %s, want the application's answer", how, status)
	}
	select {
	case path := <-captured:
		if path != "/v6" {
			t.Fatalf("recorded a call to %q, want /v6", path)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("a call %s was not recorded", how)
	}
}

// Without the hold, nothing takes an application port's IPv6 calls but
// keploy's forwarder: the application's bind on :: was moved off the port.
// So the forwarder listens on both families, and a call over IPv6 (in an
// IPv6-only Pod, every call) is answered and recorded. It used to listen on
// IPv4 only, and every IPv6 call was refused for the whole recording.
func TestAForwarderWithoutAHoldTakesIPv6Calls(t *testing.T) {
	requireIPv6(t)
	hook, captured, logs := newCapturingIngressHook(t, nil)
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	defer cancel()
	app := oneShotUpstream(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	orig := uint16(testnet.ReservedPort(t))
	if err := hook.StartIngress(ctx, orig, portOf(t, app)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hook.StopIngress(orig) })

	c6, err := net.Dial("tcp6", net.JoinHostPort("::1", strconv.Itoa(int(orig))))
	if err != nil {
		t.Fatalf("the forwarder took no IPv6 call: %v", err)
	}
	defer func() { _ = c6.Close() }()
	answeredAndRecorded(t, c6, captured, "over IPv6")
	c4, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(orig))))
	if err != nil {
		t.Fatalf("the forwarder took no IPv4 call: %v", err)
	}
	defer func() { _ = c4.Close() }()
	answeredAndRecorded(t, c4, captured, "over IPv4")
	if warns := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
		t.Fatalf("WARNs from a forwarder that took both families of a free port: %v", warns)
	}
	for _, not := range []string{"holds the application's port on IPv6", "cannot listen on the application's port over IPv6"} {
		if said := logs.FilterMessageSnippet(not).All(); len(said) != 0 {
			t.Fatalf("a forwarder that took the port's IPv6 says it did not: %v", said)
		}
	}
}

// With the hold, the forwarder listens on IPv4 only, as before: the hold
// takes the port's IPv6 calls (its lookup can hand them only to an IPv6
// socket, the park), so the forwarder leaves the port's IPv6 free.
func TestAForwarderWithAHoldListensOnIPv4Only(t *testing.T) {
	requireIPv6(t)
	hook, _, _ := newCapturingIngressHook(t, &fakePortHolder{held: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orig := uint16(testnet.ReservedPort(t))
	if err := hook.StartIngress(ctx, orig, 1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hook.StopIngress(orig) })
	if a := forwarderOf(t, hook, orig).listener.Addr().(*net.TCPAddr); a.IP.To4() == nil {
		t.Fatalf("the forwarder listens at %s, want IPv4", a)
	}
	ln, err := net.Listen("tcp6", net.JoinHostPort("::", strconv.Itoa(int(orig))))
	if err != nil {
		t.Fatalf("the port's IPv6 is not free beside a forwarder with the hold: %v", err)
	}
	_ = ln.Close()
}

// Without the hold, the forwarder keeps other listeners off both families
// of the port's wildcard address, as its listener on IPv4 alone kept them
// off IPv4: one listener on both families would not. On Windows a later
// bind of 0.0.0.0:P succeeds beside a socket on both families, and takes
// the port's IPv4 calls from the forwarder.
func TestAForwarderWithoutAHoldKeepsOtherListenersOffThePort(t *testing.T) {
	requireIPv6(t)
	hook, _, _ := newCapturingIngressHook(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orig := testnet.ReservedPort(t)
	if err := hook.StartIngress(ctx, uint16(orig), 1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hook.StopIngress(uint16(orig)) })
	for _, other := range []struct{ network, host string }{{"tcp4", "0.0.0.0"}, {"tcp6", "::"}} {
		ln, err := net.Listen(other.network, net.JoinHostPort(other.host, strconv.Itoa(orig)))
		if err == nil {
			_ = ln.Close()
			t.Fatalf("another listener bound %s %s beside the forwarder", other.network, ln.Addr())
		}
		if !neterr.IsAddrInUse(err) {
			t.Fatalf("another listener at %s [%s]:%d: %v, want address in use", other.network, other.host, orig, err)
		}
	}
}

// Without the hold, a listener that is not keploy's and holds the port on
// IPv6 alone keeps the port's IPv6 calls, as without keploy, and the
// forwarder takes its IPv4 ones, as it always did. The listener is at the
// IPv6 loopback, or at :: with IPV6_V6ONLY (Go sets it for "tcp6"). At ::
// the forwarder's IPv6 bind fails everywhere ("address in use"; on Windows
// WSAEADDRINUSE, which syscall.EADDRINUSE does not match), and its debug log
// says another listener holds the port's IPv6, naming the port, and nothing
// is logged as a warning; at ::1 the bind fails on Linux only.
func TestAForwarderWithoutAHoldLeavesIPv6ToAnIPv6OnlyListener(t *testing.T) {
	for _, at := range []string{"::1", "::"} {
		t.Run("at ["+at+"]", func(t *testing.T) {
			hook, captured, logs := newCapturingIngressHook(t, nil)
			ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
			defer cancel()
			app := oneShotUpstream(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			orig := testnet.ReservedPort(t)
			// tcp6: IPV6_V6ONLY, beside the reservation.
			other, err := net.Listen("tcp6", net.JoinHostPort(at, strconv.Itoa(orig)))
			if err != nil {
				t.Skipf("no IPv6 here: %v", err)
			}
			t.Cleanup(func() { _ = other.Close() })
			if err := hook.StartIngress(ctx, uint16(orig), portOf(t, app)); err != nil {
				t.Fatalf("the forwarder did not start beside an IPv6-only listener at [%s]: %v", at, err)
			}
			t.Cleanup(func() { _ = hook.StopIngress(uint16(orig)) })

			c4, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(orig)))
			if err != nil {
				t.Fatalf("the forwarder took no IPv4 call: %v", err)
			}
			defer func() { _ = c4.Close() }()
			answeredAndRecorded(t, c4, captured, "over IPv4")

			accepted := make(chan error, 1)
			go func() {
				c, err := other.Accept()
				if err == nil {
					_ = c.Close()
				}
				accepted <- err
			}()
			c6, err := net.Dial("tcp6", net.JoinHostPort("::1", strconv.Itoa(orig)))
			if err != nil {
				t.Fatalf("the IPv6-only listener took no IPv6 call: %v", err)
			}
			_ = c6.Close()
			select {
			case err := <-accepted:
				if err != nil {
					t.Fatalf("the IPv6-only listener's accept: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the IPv6 call did not reach the IPv6-only listener")
			}
			if at != "::" {
				return
			}
			said := logs.FilterMessageSnippet("another listener holds the application's port on IPv6").All()
			if len(said) != 1 || said[0].ContextMap()["port"] != uint16(orig) || said[0].ContextMap()["family"] != "IPv6" {
				t.Fatalf("logged %v, want one line saying another listener holds port %d on IPv6", said, orig)
			}
			if warns := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
				t.Fatalf("WARNs %v, want none: the port's IPv6 calls go where they went before", warns)
			}
		})
	}
}

// The teardown guard in StartIngressProxy must be SILENT: a bind event drained
// after context cancel must not arm a forwarder AND must not emit an ERROR log
// (the flask-secret CI lane fails on any ERROR in the record output).
func TestStartIngressProxySilentOnCanceledContext(t *testing.T) {
	testnet.HoldFinalizers(t)
	core, logs := observer.New(zapcore.DebugLevel)
	pm := &IngressProxyManager{
		logger: zap.New(core),
		active: make(map[uint16]proxyStop),
	}
	pm.ingressHook = newGoTCPIngressHook(pm)

	port := uint16(testnet.ReservedPort(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // agent already shutting down

	pm.StartIngressProxy(ctx, port, 0)

	if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
		t.Fatalf("expected no ERROR logs when skipping a forwarder during teardown, got %d: %v", n, logs.All())
	}
	pm.mu.Lock()
	_, active := pm.active[port]
	pm.mu.Unlock()
	if active {
		t.Fatalf("no forwarder should be armed for port %d after a canceled StartIngressProxy", port)
	}
	if err := bindAgain(t, port); err != nil {
		t.Fatalf("port %d should be free after a canceled StartIngressProxy, bind failed: %v", port, err)
	}
}

// When the accept loop exits because its context was canceled (and StopIngress is
// never called), context.AfterFunc must wake the listener parked in Accept, the
// deferred listener.Close must release the port so the next run can rebind it,
// and no WARN or ERROR log may be emitted.
func TestStartIngressReleasesPortWhenAcceptLoopExits(t *testing.T) {
	testnet.HoldFinalizers(t)
	core, logs := observer.New(zapcore.WarnLevel)
	hook := newGoTCPIngressHook(&IngressProxyManager{logger: zap.New(core)})

	baseline := countAcceptIngressInAccept()
	ctx, cancel := context.WithCancel(context.Background())
	port := startIngressAtFreePort(t, ctx, hook)
	st := forwarderOf(t, hook, port)
	t.Cleanup(func() {
		cancel()
		_ = st.listener.Close()
		if st.listener6 != nil {
			_ = st.listener6.Close()
		}
	})

	wantLoops := 1
	if st.listener6 != nil {
		wantLoops = 2
	}
	waitForAcceptIngressParked(t, baseline+wantLoops)

	cancel() // cancel WITHOUT calling StopIngress
	waitForDone(t, st.done, fmt.Sprintf("accept loop on port %d to exit after ctx cancel", port))

	if !listenerClosed(t, st.listener) {
		t.Fatalf("the accept loop exited with the forwarder's IPv4 listener at port %d open", port)
	}
	if st.listener6 != nil && !listenerClosed(t, st.listener6) {
		t.Fatalf("the accept loop exited with the forwarder's IPv6 listener at port %d open", port)
	}
	if err := bindAgain(t, port); err != nil {
		t.Fatalf("port %d should be free once the accept loop exited without StopIngress, but bind failed: %v", port, err)
	}
	if logs.Len() != 0 {
		t.Fatalf("forwarder logged %v on ctx cancel, want no WARN or ERROR", logs.All())
	}
}

func TestDialAppReachesAnIPv6App(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	defer ln.Close()
	conn, err := New(zap.NewNop(), nil, &config.Config{}).dialApp(context.Background(), ln.Addr().String(), nil)
	if err != nil {
		t.Fatalf("dialApp(%s): %v", ln.Addr(), err)
	}
	_ = conn.Close()
}

// nopIngressHook starts and stops forwarders without binding anything.
type nopIngressHook struct{}

func (nopIngressHook) StartIngress(context.Context, uint16, uint16) error { return nil }
func (nopIngressHook) StopIngress(uint16) error                           { return nil }

// While recording, keploy's forwarder holds the app's port and the app listens
// on the port its bind was moved to. Asked where the app listens, the agent has
// to look there: the socket on the app's own port is keploy's.
func TestAppListenPortFollowsTheMovedBind(t *testing.T) {
	pm := &IngressProxyManager{logger: zap.NewNop(), active: make(map[uint16]proxyStop)}
	pm.ingressHook = nopIngressHook{}

	if port, ok := pm.AppListenPort(8097); !ok || port != 8097 {
		t.Fatalf("no forwarder: got %d, %v; want the app's own port", port, ok)
	}
	pm.StartIngressProxy(context.Background(), 8097, 41541)
	if port, ok := pm.AppListenPort(8097); !ok || port != 41541 {
		t.Fatalf("forwarded: got %d, %v; want the moved bind 41541", port, ok)
	}
	// A bind event without the new port: the forwarder holds 8097 and where
	// the app went is not known.
	pm.StartIngressProxy(context.Background(), 8098, 0)
	if port, ok := pm.AppListenPort(8098); ok {
		t.Fatalf("forwarded to an unknown port: got %d, ok; want not ok", port)
	}
	pm.StopAll()
	if port, ok := pm.AppListenPort(8097); !ok || port != 8097 {
		t.Fatalf("after StopAll: got %d, %v; want the app's own port", port, ok)
	}
}

// The HTTP/1 ingress is handed its client's connection wrapped (replayConn),
// and a half-close of it reaches the connection underneath: lingerClose ends
// what the ingress sends with one, so a client still sending a body the
// ingress cut off knows the response is all. The wrapper, embedding net.Conn
// as an interface, did not promote CloseWrite, and the half-close did
// nothing. The connection still reads after it, from what was read ahead on.
func TestReplayConnHalfClosesTheConnectionUnderneath(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()

	conn := newReplayConn([]byte("GE"), server)
	if err := util.CloseWriteIfPossible(conn); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := client.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("the client read (%d, %v), want the end of what the ingress sends", n, err)
	}
	if _, err := io.WriteString(client, "T /"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "GET /" {
		t.Fatalf("read %q (%v) after the half-close, want what was read ahead, then what the client sent", got, err)
	}
}

// A gRPC call a pause of the memory guard costs is counted on its ingress
// connection, named as the HTTP ingress names one (ingressConnName): the
// recording's warning names the connections that lost calls, gRPC's too.
func TestAGRPCCallAPauseCostsIsCountedOnItsConnection(t *testing.T) {
	frames := func(write func(*http2.Framer, *hpack.Encoder, *bytes.Buffer) error) []byte {
		t.Helper()
		var out, block bytes.Buffer
		if err := write(http2.NewFramer(&out, nil), hpack.NewEncoder(&block), &block); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	headers := func(fr *http2.Framer, enc *hpack.Encoder, block *bytes.Buffer, end bool, fields ...string) error {
		block.Reset()
		for i := 0; i < len(fields); i += 2 {
			if err := enc.WriteField(hpack.HeaderField{Name: fields[i], Value: fields[i+1]}); err != nil {
				return err
			}
		}
		return fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block.Bytes(), EndStream: end, EndHeaders: true})
	}
	msg := []byte{0, 0, 0, 0, 0} // an empty gRPC message
	req := append([]byte(clientPreface), frames(func(fr *http2.Framer, enc *hpack.Encoder, block *bytes.Buffer) error {
		if err := headers(fr, enc, block, false, ":method", "POST", ":scheme", "http", ":path", "/svc.Echo/Ping",
			":authority", "localhost", "content-type", "application/grpc"); err != nil {
			return err
		}
		return fr.WriteData(1, true, msg)
	})...)
	resp := frames(func(fr *http2.Framer, enc *hpack.Encoder, block *bytes.Buffer) error {
		if err := headers(fr, enc, block, false, ":status", "200", "content-type", "application/grpc"); err != nil {
			return err
		}
		if err := fr.WriteData(1, false, msg); err != nil {
			return err
		}
		return headers(fr, enc, block, true, "grpc-status", "0")
	})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() { // the app: takes the call, answers it
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if _, err := io.ReadFull(c, make([]byte, len(req))); err == nil {
			_, _ = c.Write(resp)
		}
		_, _ = io.Copy(io.Discard, c)
	}()

	restore := memoryguard.SetRecordingPausedForTest(true)
	defer restore()
	mgr := syncMock.New(zap.NewNop())
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
	defer cancel()
	pm := &IngressProxyManager{logger: zap.NewNop()}
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pm.handleConnection(ctx, server, ln.Addr().String(), pm.logger, make(chan *models.TestCase), make(chan struct{}, 1), 8080)
	}()
	go func() { _, _ = client.Write(req) }()
	if _, err := io.ReadFull(client, make([]byte, len(resp))); err != nil {
		t.Fatalf("the call's response: %v", err)
	}
	want := ingressConnName(server, ln.Addr().String())
	deadline := time.Now().Add(30 * time.Second) // bounds only a call never decided
	for mgr.NotRecordedUnderPressure().Total == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the call the pause cost was not counted")
		}
		time.Sleep(time.Millisecond)
	}
	if got := mgr.NotRecordedUnderPressure(); got.Total != 1 || got.ByConn[want] != 1 {
		t.Fatalf("not recorded under pressure: %+v, want the call on %q", got, want)
	}
	cancel() // the ingress closes both ends
	<-done
	_ = client.Close()
}

// raceAtDoneReadCtx runs onFirstDone inside the first call to Done while
// returning an unclosed channel, and delegates later Done calls to the
// underlying context. On an accept loop that polls ctx.Done() before calling
// SetDeadline, this places the shutdown race deterministically: the select
// takes its default branch, and then the next operation runs with ctx already
// canceled and the listener already closed.
type raceAtDoneReadCtx struct {
	context.Context
	onFirstDone func()
	once        sync.Once
	firstCh     chan struct{}
}

func (c *raceAtDoneReadCtx) Done() <-chan struct{} {
	first := false
	c.once.Do(func() {
		first = true
		c.onFirstDone()
	})
	if first {
		return c.firstCh
	}
	return c.Context.Done()
}

// Stopping an ingress forwarder cancels its context and closes its listener
// concurrently with acceptIngress. Even when shutdown runs immediately after
// the loop's context check — or when the listener is closed before the context
// cancels — acceptIngress must exit cleanly without logging an ERROR or WARN.
func TestAcceptIngressLogsNoErrorWhenListenerClosesAtShutdown(t *testing.T) {
	t.Run("ctx cancels and listener closes after Done check", func(t *testing.T) {
		core, logs := observer.New(zapcore.WarnLevel)
		h := newGoTCPIngressHook(&IngressProxyManager{logger: zap.New(core)})

		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer func() { _ = ln.Close() }()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rctx := &raceAtDoneReadCtx{
			Context: ctx,
			firstCh: make(chan struct{}),
			onFirstDone: func() {
				cancel()
				_ = ln.Close()
			},
		}

		h.acceptIngress(rctx, ln, "127.0.0.1:1", make(chan struct{}, 1), 0)

		if !listenerClosed(t, ln) {
			t.Fatal("acceptIngress returned with the listener still open")
		}
		if logs.Len() != 0 {
			t.Fatalf("acceptIngress logged %v on shutdown, want no WARN or ERROR", logs.All())
		}
	})

	t.Run("listener closed before ctx cancels", func(t *testing.T) {
		core, logs := observer.New(zapcore.WarnLevel)
		h := newGoTCPIngressHook(&IngressProxyManager{logger: zap.New(core)})

		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		if err := ln.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		h.acceptIngress(context.Background(), ln, "127.0.0.1:1", make(chan struct{}, 1), 0)

		if logs.Len() != 0 {
			t.Fatalf("acceptIngress logged %v on a closed listener, want no WARN or ERROR", logs.All())
		}
	})

	t.Run("ctx already canceled before acceptIngress starts", func(t *testing.T) {
		core, logs := observer.New(zapcore.WarnLevel)
		h := newGoTCPIngressHook(&IngressProxyManager{logger: zap.New(core)})

		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer func() { _ = ln.Close() }()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		h.acceptIngress(ctx, ln, "127.0.0.1:1", make(chan struct{}, 1), 0)

		if !listenerClosed(t, ln) {
			t.Fatal("acceptIngress returned with the listener still open on a canceled ctx")
		}
		if logs.Len() != 0 {
			t.Fatalf("acceptIngress logged %v on an already-canceled ctx, want no WARN or ERROR", logs.All())
		}
	})
}

func waitForDone(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// countAcceptIngressInAccept counts goroutines whose stack currently shows
// acceptIngress parked inside (*net.TCPListener).Accept.
func countAcceptIngressInAccept() int {
	buf := make([]byte, 64*1024)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, len(buf)*2)
	}
	count := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "(*goTCPIngressHook).acceptIngress(") &&
			strings.Contains(g, "(*TCPListener).Accept(") {
			count++
		}
	}
	return count
}

func waitForAcceptIngressParked(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if countAcceptIngressInAccept() >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d acceptIngress goroutine(s) to park in (*TCPListener).Accept (saw %d)",
				want, countAcceptIngressInAccept())
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

type deadlineOnAcceptCtx struct {
	context.Context
	mu   sync.Mutex
	err  error
	done chan struct{}
}

func newDeadlineOnAcceptCtx() *deadlineOnAcceptCtx {
	return &deadlineOnAcceptCtx{
		Context: context.Background(),
		done:    make(chan struct{}),
	}
}

func (c *deadlineOnAcceptCtx) expire() {
	c.mu.Lock()
	if c.err == nil {
		c.err = context.DeadlineExceeded
		close(c.done)
	}
	c.mu.Unlock()
}

func (c *deadlineOnAcceptCtx) Done() <-chan struct{} { return c.done }

func (c *deadlineOnAcceptCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

type cancelOnAcceptListener struct {
	net.Listener
	cancel context.CancelFunc
	conn   net.Conn
	mu     sync.Mutex
	calls  int
}

func (l *cancelOnAcceptListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.calls++
	n := l.calls
	l.mu.Unlock()
	if n == 1 {
		l.cancel()
		return l.conn, nil
	}
	return nil, net.ErrClosed
}

func (l *cancelOnAcceptListener) Close() error { return nil }

// If ctx ends just as Accept returns a connection, acceptIngress closes that
// connection directly instead of spawning handleConnection on an ended ctx.
// Note: StartIngress cancels its child ctx with context.Canceled, which
// handleConnection's utils.LogError suppresses anyway; deadlineOnAcceptCtx
// uses context.DeadlineExceeded solely to make skipping handleConnection
// observable without relying on goroutine scheduling.
func TestAcceptIngressClosesConnectionAcceptedAfterCancel(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	h := newGoTCPIngressHook(&IngressProxyManager{logger: zap.New(core)})

	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	ctx := newDeadlineOnAcceptCtx()
	ln := &cancelOnAcceptListener{cancel: ctx.expire, conn: server}

	h.acceptIngress(ctx, ln, "127.0.0.1:1", make(chan struct{}, 1), 0)

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("accepted connection after cancel read %v, want io.EOF (closed)", err)
	}
	ln.mu.Lock()
	calls := ln.calls
	ln.mu.Unlock()
	if calls != 1 {
		t.Fatalf("Accept called %d times after cancel, want 1", calls)
	}
	if logs.Len() != 0 {
		t.Fatalf("acceptIngress logged %v, want no WARN or ERROR", logs.All())
	}
}

type blockingAcceptListener struct {
	net.Listener
	entered   chan struct{}
	closed    chan struct{}
	enterOnce sync.Once
	closeOnce sync.Once
	mu        sync.Mutex
	closes    int
}

func newBlockingAcceptListener() *blockingAcceptListener {
	return &blockingAcceptListener{
		entered: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (l *blockingAcceptListener) Accept() (net.Conn, error) {
	l.enterOnce.Do(func() { close(l.entered) })
	<-l.closed
	return nil, net.ErrClosed
}

func (l *blockingAcceptListener) Close() error {
	l.mu.Lock()
	l.closes++
	l.mu.Unlock()
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *blockingAcceptListener) closeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closes
}

// Once acceptIngress is blocked in Accept, canceling ctx must close the
// listener via AfterFunc and wake the loop immediately without StopIngress.
func TestAcceptIngressWakesBlockedAcceptOnContextCancel(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	h := newGoTCPIngressHook(&IngressProxyManager{logger: zap.New(core)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := newBlockingAcceptListener()
	t.Cleanup(func() { _ = ln.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.acceptIngress(ctx, ln, "127.0.0.1:1", make(chan struct{}, 1), 0)
	}()

	waitForDone(t, ln.entered, "acceptIngress to enter Accept")
	cancel()
	waitForDone(t, done, "acceptIngress to exit after ctx cancel")

	if ln.closeCount() == 0 {
		t.Fatal("listener was not closed when ctx canceled while blocked in Accept")
	}
	if logs.Len() != 0 {
		t.Fatalf("acceptIngress logged %v on ctx cancel, want no WARN or ERROR", logs.All())
	}
}

type errAcceptListener struct {
	net.Listener
	err    error
	mu     sync.Mutex
	closes int
}

func (l *errAcceptListener) Accept() (net.Conn, error) { return nil, l.err }

func (l *errAcceptListener) Close() error {
	l.mu.Lock()
	l.closes++
	l.mu.Unlock()
	return nil
}

func (l *errAcceptListener) closeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closes
}

type temporaryAcceptError struct{ msg string }

func (e temporaryAcceptError) Error() string   { return e.msg }
func (e temporaryAcceptError) Temporary() bool { return true }

type scriptedIngressListener struct {
	results chan any
	closed  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	closes  int
}

func newScriptedIngressListener() *scriptedIngressListener {
	return &scriptedIngressListener{
		results: make(chan any, 32),
		closed:  make(chan struct{}),
	}
}

func (l *scriptedIngressListener) Accept() (net.Conn, error) {
	select {
	case r := <-l.results:
		if err, ok := r.(error); ok {
			return nil, err
		}
		return r.(net.Conn), nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *scriptedIngressListener) Close() error {
	l.mu.Lock()
	l.closes++
	l.mu.Unlock()
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *scriptedIngressListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func (l *scriptedIngressListener) closeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closes
}

// Temporary Accept failures on a live context (EMFILE, ENFILE, ENOBUFS, ENOMEM,
// or any Temporary() error) are retried with exponential backoff from 5 ms up
// to 1 s, resetting to 5 ms after a successful Accept, logging only the first
// failure at WARN. Canceling ctx during backoff stops the loop immediately.
func TestAcceptIngressRetriesTemporaryAcceptErrorsWithBackoff(t *testing.T) {
	t.Run("exponential backoff capped at 1s, reset on good Accept, single WARN", func(t *testing.T) {
		pauses := make(chan time.Duration, 32)
		savedWait := ingressAcceptRetryWait
		t.Cleanup(func() { ingressAcceptRetryWait = savedWait })
		ingressAcceptRetryWait = func(_ context.Context, d time.Duration) bool {
			pauses <- d
			return true
		}

		core, logs := observer.New(zapcore.WarnLevel)
		h := newGoTCPIngressHook(&IngressProxyManager{logger: zap.New(core)})
		ln := newScriptedIngressListener()
		ctx, cancel := context.WithCancel(context.Background())

		const origPort = uint16(18084)
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.acceptIngress(ctx, ln, "127.0.0.1:1", make(chan struct{}, 1), origPort)
		}()
		t.Cleanup(func() {
			cancel()
			_ = ln.Close()
			waitForDone(t, done, "acceptIngress cleanup")
		})

		ms := time.Millisecond
		steps := []struct {
			result any
			pause  time.Duration // 0: successful Accept, no backoff
		}{
			{acceptErr(syscall.EMFILE), 5 * ms},
			{acceptErr(syscall.ENFILE), 10 * ms},
			{acceptErr(syscall.EMFILE), 20 * ms},
			{acceptErr(syscall.EMFILE), 40 * ms},
			{acceptErr(syscall.EMFILE), 80 * ms},
			{acceptErr(syscall.EMFILE), 160 * ms},
			{acceptErr(syscall.EMFILE), 320 * ms},
			{acceptErr(syscall.EMFILE), 640 * ms},
			{acceptErr(syscall.EMFILE), time.Second},
			{acceptErr(syscall.EMFILE), time.Second},
			{nil, 0}, // good Accept resets backoff
			{acceptErr(syscall.ENOBUFS), 5 * ms},
			{acceptErr(syscall.ENOMEM), 10 * ms},
			{temporaryAcceptError{msg: "transient accept error"}, 20 * ms},
		}

		for i, st := range steps {
			if st.result == nil {
				a, b := net.Pipe()
				_ = b.Close() // handleConnection hits immediate EOF and returns silently
				ln.results <- a
				continue
			}
			ln.results <- st.result
			select {
			case got := <-pauses:
				if got != st.pause {
					t.Fatalf("step %d (%v): backoff %v, want %v", i, st.result, got, st.pause)
				}
			case <-done:
				t.Fatalf("step %d (%v): acceptIngress exited instead of retrying", i, st.result)
			case <-time.After(5 * time.Second):
				t.Fatalf("step %d (%v): timed out waiting for retry backoff", i, st.result)
			}
		}

		if got := ln.closeCount(); got != 0 {
			t.Fatalf("listener closed %d times during temporary errors, want 0", got)
		}

		cancel()
		waitForDone(t, done, "acceptIngress to exit on ctx cancel after retries")

		warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
		errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
		if len(warns) != 1 || warns[0].ContextMap()["port"] != origPort {
			t.Fatalf("WARNs = %v, want exactly 1 WARN naming port %d", warns, origPort)
		}
		if len(errs) != 0 {
			t.Fatalf("ERRORs = %v, want none on temporary errors + clean shutdown", errs)
		}
	})

	t.Run("ctx cancel interrupts backoff wait immediately", func(t *testing.T) {
		savedWait := ingressAcceptRetryWait
		t.Cleanup(func() { ingressAcceptRetryWait = savedWait })
		inWait := make(chan struct{})
		var waitOnce sync.Once
		ingressAcceptRetryWait = func(ctx context.Context, _ time.Duration) bool {
			waitOnce.Do(func() { close(inWait) })
			// Exercise the real timer/ctx.Done select with a long duration so
			// only ctx cancellation can return within the 5 s bound.
			return savedWait(ctx, time.Hour)
		}

		core, logs := observer.New(zapcore.WarnLevel)
		h := newGoTCPIngressHook(&IngressProxyManager{logger: zap.New(core)})
		ln := newScriptedIngressListener()
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan struct{})
		go func() {
			defer close(done)
			h.acceptIngress(ctx, ln, "127.0.0.1:1", make(chan struct{}, 1), 18085)
		}()
		t.Cleanup(func() {
			cancel()
			_ = ln.Close()
			waitForDone(t, done, "acceptIngress backoff-cancel cleanup")
		})

		ln.results <- acceptErr(syscall.EMFILE)
		waitForDone(t, inWait, "acceptIngress to enter retry backoff wait")

		cancel()
		waitForDone(t, done, "acceptIngress to exit immediately when ctx cancels during backoff")

		if errs := logs.FilterLevelExact(zapcore.ErrorLevel).All(); len(errs) != 0 {
			t.Fatalf("ERRORs = %v, want none when ctx cancels during backoff", errs)
		}
	})
}

// When Accept fails on a live context with a non-temporary, non-ErrClosed
// error, acceptIngress must log an ERROR naming the port and stating that
// recording has stopped there, and still close the listener before returning.
func TestAcceptIngressLogsErrorAndClosesListenerOnPermanentAcceptFailure(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	h := newGoTCPIngressHook(&IngressProxyManager{logger: zap.New(core)})

	const origPort = uint16(18086)
	ln := &errAcceptListener{err: acceptErr(syscall.EINVAL)}
	h.acceptIngress(context.Background(), ln, "127.0.0.1:1", make(chan struct{}, 1), origPort)

	if got := ln.closeCount(); got != 1 {
		t.Fatalf("listener Close called %d times, want 1", got)
	}
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	if len(errs) != 1 {
		t.Fatalf("ERRORs = %v, want 1", logs.All())
	}
	if errs[0].ContextMap()["port"] != origPort {
		t.Fatalf("ERROR port = %v, want %d", errs[0].ContextMap()["port"], origPort)
	}
	if !strings.Contains(errs[0].Message, "recording has stopped on this port") {
		t.Fatalf("ERROR message %q does not state recording has stopped on this port", errs[0].Message)
	}
	if warns := logs.FilterLevelExact(zapcore.WarnLevel).All(); len(warns) != 0 {
		t.Fatalf("WARNs = %v, want none on permanent failure", warns)
	}
}
