package proxy

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

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
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

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
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	listening := make(chan net.Listener, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		l, err := net.Listen("tcp4", addr)
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
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	start := time.Now()
	if _, err := dialIngressTarget(context.Background(), addr, 100*time.Millisecond); err == nil {
		t.Fatal("expected an error dialing a port nothing listens on")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond || elapsed > time.Second {
		t.Fatalf("expected to retry for the timeout then give up, took %s", elapsed)
	}
}

func TestDialIngressTargetStopsOnCanceledContext(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

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

// A forwarder must NOT bind the app port when the agent context is already
// canceled (shutting down). Binding during teardown leaves the listener holding
// the port, so the next record run's application fails to bind it with "address
// already in use" — the flaky port-8000 reuse failure.
func TestStartIngressSkipsBindWhenContextCanceled(t *testing.T) {
	hook := newTestIngressHook()
	port := freeTCPPort(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // agent already shutting down

	if err := hook.StartIngress(ctx, port, 0); err == nil {
		t.Fatal("expected StartIngress to abort on a canceled context, got nil")
	}

	// The port must be free — the fix must not have bound it.
	ln, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(int(port)))
	if err != nil {
		t.Fatalf("port %d should be free after a canceled StartIngress, but bind failed: %v", port, err)
	}
	_ = ln.Close()

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
	hook := newTestIngressHook()
	port := freeTCPPort(t)

	if err := hook.StartIngress(context.Background(), port, 0); err != nil {
		t.Fatalf("StartIngress: %v", err)
	}

	// Port is bound now.
	if ln, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(int(port))); err == nil {
		_ = ln.Close()
		t.Fatalf("port %d should be bound by the running forwarder", port)
	}

	if err := hook.StopIngress(port); err != nil {
		t.Fatalf("StopIngress: %v", err)
	}

	// Port must be released.
	ln, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(int(port)))
	if err != nil {
		t.Fatalf("port %d should be free after StopIngress, but bind failed: %v", port, err)
	}
	_ = ln.Close()
}

// The teardown guard in StartIngressProxy must be SILENT: a bind event drained
// after context cancel must not arm a forwarder AND must not emit an ERROR log
// (the flask-secret CI lane fails on any ERROR in the record output).
func TestStartIngressProxySilentOnCanceledContext(t *testing.T) {
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
	if ln, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(int(port))); err != nil {
		t.Fatalf("port %d should be free after a canceled StartIngressProxy, bind failed: %v", port, err)
	} else {
		_ = ln.Close()
	}
}

// When the accept loop exits because its context was canceled (and StopIngress is
// never called), the deferred listener.Close must still release the port so the
// next run can rebind it. Without that defer this test times out.
func TestStartIngressReleasesPortWhenAcceptLoopExits(t *testing.T) {
	hook := newTestIngressHook()
	port := freeTCPPort(t)

	ctx, cancel := context.WithCancel(context.Background())
	if err := hook.StartIngress(ctx, port, 0); err != nil {
		t.Fatalf("StartIngress: %v", err)
	}
	cancel() // cancel WITHOUT calling StopIngress

	deadline := time.Now().Add(5 * time.Second)
	for {
		if ln, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(int(port))); err == nil {
			_ = ln.Close()
			return // port released by the accept-loop defer
		}
		if time.Now().After(deadline) {
			t.Fatalf("port %d not released within 5s after context cancel without StopIngress", port)
		}
		time.Sleep(20 * time.Millisecond)
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
