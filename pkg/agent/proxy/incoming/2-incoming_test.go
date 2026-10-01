package proxy

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

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

func TestLooksLikeTLSClientHello(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"tls1.0 record", []byte{0x16, 0x03, 0x01, 0x02, 0x00}, true},
		{"tls1.3 record version", []byte{0x16, 0x03, 0x03}, true},
		{"upper bound 3.3", []byte{0x16, 0x03, 0x03}, true},
		{"just past bound 3.4", []byte{0x16, 0x03, 0x04}, false},
		{"minor 5", []byte{0x16, 0x03, 0x05}, false},
		{"major 2", []byte{0x16, 0x02, 0x01}, false},
		{"alert type", []byte{0x15, 0x03, 0x03}, false},
		{"http1", []byte("GET / HTTP/1.1\r\n"), false},
		{"h2 preface", []byte(clientPreface), false},
		{"too short", []byte{0x16, 0x03}, false},
		{"wrong minor", []byte{0x16, 0x03, 0x09}, false},
	}
	for _, c := range cases {
		if got := looksLikeTLSClientHello(c.in); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// A TLS handshake reaching the ingress listener must be relayed to the
// relocated listener byte for byte, not answered as HTTP/1.
func TestIngressRelaysTLSHandshakeUntouched(t *testing.T) {
	backend, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer backend.Close()
	hello := append([]byte{0x16, 0x03, 0x01, 0x00, 0x30}, make([]byte, 60)...)
	got := make(chan []byte, 1)
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, len(hello))
		_, _ = io.ReadFull(c, buf)
		got <- buf
		_, _ = c.Write([]byte{0x16, 0x03, 0x03, 0x00, 0x01, 0xAA})
	}()

	pm := &IngressProxyManager{logger: zap.NewNop()}
	cliSide, srvSide := net.Pipe()
	go pm.handleConnection(context.Background(), srvSide, backend.Addr().String(), zap.NewNop(), nil, make(chan struct{}, 1), 8443)

	if _, err := cliSide.Write(hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	reply := make([]byte, 6)
	_ = cliSide.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(cliSide, reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply[0] != 0x16 || reply[5] != 0xAA {
		t.Fatalf("reply was not the backend's TLS bytes: %x", reply)
	}
	select {
	case b := <-got:
		if string(b) != string(hello) {
			t.Fatalf("backend saw altered bytes")
		}
	case <-time.After(time.Second):
		t.Fatal("backend never received the handshake")
	}
	_ = cliSide.Close()
}

// When the upstream closes first, the client side must see EOF rather than
// hang until it closes itself.
func TestIngressRelayHalfClosesTowardClient(t *testing.T) {
	backend, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer backend.Close()
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 64)
		_, _ = io.ReadFull(c, buf[:5])
		_, _ = c.Write([]byte{0x16, 0x03, 0x03, 0x00, 0x01, 0xBB})
		_ = c.Close()
	}()

	front, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer front.Close()
	pm := &IngressProxyManager{logger: zap.NewNop()}
	go func() {
		c, err := front.Accept()
		if err != nil {
			return
		}
		pm.handleConnection(context.Background(), c, backend.Addr().String(), zap.NewNop(), nil, make(chan struct{}, 1), 8443)
	}()

	cli, err := net.Dial("tcp4", front.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	// Only the 24-byte preface read is needed for detection; send enough.
	hello := append([]byte{0x16, 0x03, 0x01, 0x00, 0x30}, make([]byte, 60)...)
	if _, err := cli.Write(hello); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
	data, err := io.ReadAll(cli)
	if err != nil {
		t.Fatalf("client never saw EOF after upstream closed: %v (got %x)", err, data)
	}
	if len(data) != 6 || data[5] != 0xBB {
		t.Fatalf("unexpected reply %x", data)
	}
}
