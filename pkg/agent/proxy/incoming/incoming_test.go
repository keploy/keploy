package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestWaitForIngressTargetReady(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
		close(accepted)
	}()

	if err := waitForIngressTarget(context.Background(), ln.Addr().String(), 250*time.Millisecond); err != nil {
		t.Fatalf("waitForIngressTarget returned error: %v", err)
	}

	select {
	case <-accepted:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("waitForIngressTarget did not dial the ready listener")
	}
}

func TestWaitForIngressTargetTimeout(t *testing.T) {
	addr := testnet.RefusedAddr(t, "tcp")

	if err := waitForIngressTarget(context.Background(), addr, 40*time.Millisecond); err == nil {
		t.Fatal("expected timeout waiting for unused port")
	}
}

func TestWaitForIngressTargetWhenKnownSkipsUnknownPort(t *testing.T) {
	start := time.Now()
	waited, err := waitForIngressTargetWhenKnown(context.Background(), 0, "127.0.0.1:0", 5*time.Second)
	if err != nil {
		t.Fatalf("waitForIngressTargetWhenKnown returned error: %v", err)
	}
	if waited {
		t.Fatal("expected unknown redirected port to skip target wait")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("unknown redirected port should skip immediately, took %s", elapsed)
	}
}

func TestDialIngressTargetWaitsForAppToListen(t *testing.T) {
	// The port is held for the test's life (testnet.SharedPort): a dial to
	// it is refused until the app listens there, and no other socket can
	// take it meanwhile.
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))

	listening := make(chan net.Listener, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		l, err := testnet.OpenShared("tcp4", addr)
		if err != nil {
			listening <- nil
			return
		}
		listening <- l
	}()

	conn, err := dialIngressTarget(context.Background(), addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dialIngressTarget returned error: %v", err)
	}
	_ = conn.Close()
	if l := <-listening; l == nil {
		t.Fatal("app listener could not rebind the port")
	} else {
		_ = l.Close()
	}
}

func TestDialIngressTargetGivesUpAfterTimeout(t *testing.T) {
	addr := testnet.RefusedAddr(t, "tcp")

	start := time.Now()
	if _, err := dialIngressTarget(context.Background(), addr, 100*time.Millisecond); err == nil {
		t.Fatal("expected an error dialing a port nothing listens on")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("expected to retry for the timeout then give up, took %s", elapsed)
	}
}

func TestDialIngressTargetStopsOnCanceledContext(t *testing.T) {
	addr := testnet.RefusedAddr(t, "tcp")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := dialIngressTarget(ctx, addr, 5*time.Second); err == nil {
		t.Fatal("expected an error dialing a port nothing listens on")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("canceled context should stop retries immediately, took %s", elapsed)
	}
}

func newTestIngressHook() *goTCPIngressHook {
	return newGoTCPIngressHook(&IngressProxyManager{logger: zap.NewNop()})
}

func freeTCPPort(t *testing.T) uint16 {
	t.Helper()
	// Probe on 0.0.0.0 (the address the forwarder binds) so the port we hand back
	// is actually free for that bind, then release it for the caller to claim.
	ln, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	return port
}

// startIngressAtFreePort starts hook's forwarder at a free port and returns
// the port. Nothing holds the port between freeTCPPort and the forwarder's
// bind: the forwarder binds it with net.Listen, and no hold can sit beside
// that. So when another socket took the port meanwhile, it tries another.
// On Windows the bind's error there is WSAEADDRINUSE, which errors.Is does
// not match to EADDRINUSE, so it fails at once, as the test did before.
func startIngressAtFreePort(t *testing.T, ctx context.Context, hook *goTCPIngressHook) uint16 {
	t.Helper()
	for i := 0; ; i++ {
		port := freeTCPPort(t)
		err := hook.StartIngress(ctx, port, 0)
		if err == nil {
			return port
		}
		if !errors.Is(err, syscall.EADDRINUSE) || i == 19 {
			t.Fatalf("StartIngress: %v", err)
		}
	}
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
	port := freeTCPPort(t)

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

	port := freeTCPPort(t)
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
// never called), the deferred listener.Close must still release the port so the
// next run can rebind it. Without that defer this test fails.
func TestStartIngressReleasesPortWhenAcceptLoopExits(t *testing.T) {
	testnet.HoldFinalizers(t)
	hook := newTestIngressHook()
	ctx, cancel := context.WithCancel(context.Background())
	port := startIngressAtFreePort(t, ctx, hook)
	st := forwarderOf(t, hook, port)
	cancel() // cancel WITHOUT calling StopIngress

	<-st.done // the accept loop exited
	if !listenerClosed(t, st.listener) {
		t.Fatalf("the accept loop exited with the forwarder's listener at port %d open", port)
	}
	if err := bindAgain(t, port); err != nil {
		t.Fatalf("port %d should be free once the accept loop exited without StopIngress, but bind failed: %v", port, err)
	}
}

func TestDialIngressTargetReachesAnIPv6App(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	defer ln.Close()
	conn, err := dialIngressTarget(context.Background(), ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dialIngressTarget(%s): %v", ln.Addr(), err)
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

// The live handler of a self-call reaches the app over the connection keploy
// dialled while it held the caller's connect (util.WithPredialed): the app's
// listener sees the one connection the caller made, not a second one beside
// an idle held one that its close() would wait for.
func TestServeLiveReachesTheAppOverTheConnectionDialledForIt(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	accepted := make(chan int, 4)
	got := make(chan string, 4)
	go func() {
		for i := 1; ; i++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- i
			go func(i int, c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4)
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				n, _ := io.ReadFull(c, buf)
				got <- strconv.Itoa(i) + " " + string(buf[:n])
				_, _ = c.Write([]byte("PONG"))
			}(i, c)
		}
	}()
	held, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	pre := util.Adopt(addr, held)

	pm := &IngressProxyManager{logger: zap.NewNop()}
	cli, srv := net.Pipe()
	defer cli.Close()
	go pm.ServeLive(util.WithPredialed(context.Background(), pre), srv, addr, uint16(ln.Addr().(*net.TCPAddr).Port))
	_ = cli.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := cli.Write([]byte("PING")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(cli, buf); err != nil || string(buf) != "PONG" {
		t.Fatalf("the app's answer must reach the caller: got %q (%v)", string(buf), err)
	}
	if r := <-got; r != "1 PING" {
		t.Fatalf("the call must reach the app on the connection dialled for it (1 PING), got %q", r)
	}
	select {
	case i := <-accepted:
		if i != 1 {
			t.Fatalf("the app's listener accepted connection %d", i)
		}
	default:
	}
	select {
	case i := <-accepted:
		t.Fatalf("the app's listener accepted a second connection (%d) the caller never made", i)
	default:
	}
}
