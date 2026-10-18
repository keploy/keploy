package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// fakePortHolder is the hooks' ingress hold, as the manager sees it: a park
// listener, the moved ports, and the forwarders registered with it.
type fakePortHolder struct {
	park  net.Listener
	moved map[uint16]uint16
	held  bool

	mu           sync.Mutex
	registered   map[uint16]net.Listener
	unregistered []uint16
	parked       uint64
}

func (f *fakePortHolder) IngressParkListener() net.Listener { return f.park }
func (f *fakePortHolder) MovedPort(orig uint16) (uint16, bool) {
	p, ok := f.moved[orig]
	return p, ok
}
func (f *fakePortHolder) RegisterIngressForwarder(orig uint16, ln net.Listener) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registered == nil {
		f.registered = map[uint16]net.Listener{}
	}
	f.registered[orig] = ln
}
func (f *fakePortHolder) UnregisterIngressForwarder(orig uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unregistered = append(f.unregistered, orig)
}
func (f *fakePortHolder) IngressHeld() bool     { return f.held }
func (f *fakePortHolder) IngressParked() uint64 { return f.parked }

// parkListener is a loopback listener whose connections report port orig as
// their local address, as the kernel's sk_lookup leaves a parked connection's:
// the port the caller dialed, not the park's own.
type parkListener struct {
	net.Listener
	orig int
}

func (p parkListener) Accept() (net.Conn, error) {
	c, err := p.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return origConn{Conn: c, port: p.orig}, nil
}

type origConn struct {
	net.Conn
	port int
}

func (c origConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: c.port}
}

func loopbackListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func portOf(t *testing.T, addr string) uint16 {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return uint16(n)
}

// A call the park took (one that reached the app's port before keploy's
// forwarder listened) is served by the app at its moved port, and recorded as
// a test of the port the caller dialed.
func TestAParkedCallIsServedAndRecordedOnTheDialedPort(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	captured := make(chan uint16, 1)
	stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase,
		req *http.Request, resp *http.Response, _ time.Time, _ time.Time,
		_ models.IncomingOptions, _ bool, _ bool, appPort uint16) {
		if req.URL.Path == "/hello" && resp.StatusCode == http.StatusOK {
			captured <- appPort
		}
	})
	app := oneShotUpstream(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	const dialed = 18080
	park := parkListener{Listener: loopbackListener(t), orig: dialed}
	holder := &fakePortHolder{park: park, moved: map[uint16]uint16{dialed: portOf(t, app)}, held: true}
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
	t.Cleanup(pm.captures.Wait)

	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	defer cancel()
	go pm.serveParkedConnections(ctx, park)

	c, err := net.Dial("tcp4", park.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, "GET /hello HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("the parked call got no answer: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("the parked call got %d %q, want the app's 200 ok", resp.StatusCode, body)
	}
	select {
	case port := <-captured:
		if port != dialed {
			t.Fatalf("the parked call was recorded for port %d, want the dialed %d", port, dialed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked call was not recorded")
	}
}

// A parked connection to a port keploy did not move (nothing in
// app_port_info for it) has nowhere to go: it is closed, nothing recorded.
func TestAParkedCallToAPortKeployDidNotMoveIsClosed(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	stubCaptureHook(t, func(context.Context, *zap.Logger, chan *models.TestCase,
		*http.Request, *http.Response, time.Time, time.Time, models.IncomingOptions, bool, bool, uint16) {
		t.Error("a call to a port keploy did not move was recorded")
	})
	park := parkListener{Listener: loopbackListener(t), orig: 18081}
	holder := &fakePortHolder{park: park, moved: map[uint16]uint16{}, held: true}
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
	t.Cleanup(pm.captures.Wait)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pm.serveParkedConnections(ctx, park)

	c, err := net.Dial("tcp4", park.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read %v, want the park to close the connection", err)
	}
}

// boundAtWait starts g's forwarder for orig (the app moved to newPort, which
// does not listen) and returns whether orig was bound when the forwarder's
// wait for the app began, then stops it.
func boundAtWait(t *testing.T, pm *IngressProxyManager) bool {
	t.Helper()
	orig := portOf(t, freeAddr(t))
	newPort := portOf(t, freeAddr(t))
	bound := make(chan bool, 1)
	saved := ingressTargetWait
	t.Cleanup(func() { ingressTargetWait = saved })
	ingressTargetWait = func(context.Context, uint16, string, time.Duration) (bool, error) {
		c, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(orig))))
		if err == nil {
			_ = c.Close()
		}
		bound <- err == nil
		return true, nil
	}
	g := newGoTCPIngressHook(pm)
	if err := g.StartIngress(context.Background(), orig, newPort); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.StopIngress(orig) })
	return <-bound
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// Without a hold (a kernel without sk_lookup), nothing covers the app's port
// while keploy waits for the app to listen on its moved one, so the forwarder
// binds the port first: a call in that wait waits in the backlog instead of
// being refused. With a hold the park covers the wait, and the order is as it
// was (wait, then bind).
func TestTheForwarderBindsBeforeItsWaitOnlyWhenNothingHoldsThePort(t *testing.T) {
	unheld := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1)}
	if !boundAtWait(t, unheld) {
		t.Fatal("with nothing holding the port, the forwarder had not bound it when its wait for the app began")
	}
	// Hooks that can hold ports but hold none on this kernel are no hold.
	failed := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1),
		portHolder: &fakePortHolder{held: false}}
	if !boundAtWait(t, failed) {
		t.Fatal("with a hold that did not start, the forwarder had not bound the port when its wait for the app began")
	}
	held := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1),
		portHolder: &fakePortHolder{held: true}}
	if boundAtWait(t, held) {
		t.Fatal("with the port held, the forwarder bound it before its wait for the app, unlike before")
	}
}

// A forwarder that starts while the port is held is registered with the
// hold, so the lookup steers the port to it instead of the park; stopped, it
// is unregistered.
func TestAForwarderIsRegisteredWithTheHoldAndUnregisteredWhenItStops(t *testing.T) {
	holder := &fakePortHolder{held: true}
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
	saved := ingressTargetWait
	t.Cleanup(func() { ingressTargetWait = saved })
	ingressTargetWait = func(context.Context, uint16, string, time.Duration) (bool, error) { return true, nil }
	orig := portOf(t, freeAddr(t))
	g := newGoTCPIngressHook(pm)
	if err := g.StartIngress(context.Background(), orig, portOf(t, freeAddr(t))); err != nil {
		t.Fatal(err)
	}
	holder.mu.Lock()
	ln := holder.registered[orig]
	holder.mu.Unlock()
	if ln == nil || portOf(t, ln.Addr().String()) != orig {
		t.Fatalf("the forwarder for %d was not registered with the hold: %v", orig, holder.registered)
	}
	if err := g.StopIngress(orig); err != nil {
		t.Fatal(err)
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if len(holder.unregistered) != 1 || holder.unregistered[0] != orig {
		t.Fatalf("unregistered %v, want [%d]", holder.unregistered, orig)
	}
}

// Without a hold, the manager has no park to serve and registers nothing.
func TestWithoutAHoldThereIsNoParkToServe(t *testing.T) {
	pm := &IngressProxyManager{logger: zap.NewNop()}
	if pm.ingressPark() != nil || pm.ingressHeld() || pm.IngressParked() != 0 {
		t.Fatal("a manager without a hold reported one")
	}
	pm.registerIngressForwarder(1, nil) // no-ops, no panic
	pm.unregisterIngressForwarder(1)
}

// holdingHooks are hooks that hold the moved ports: what New type-asserts for.
type holdingHooks struct {
	*fakePortHolder
	events chan models.IngressEvent
}

func (holdingHooks) Get(context.Context, uint16) (*agent.NetworkAddress, error) { return nil, nil }
func (holdingHooks) Delete(context.Context, uint16) error                        { return nil }
func (holdingHooks) Load(context.Context, agent.HookCfg, config.Agent) error     { return nil }
func (h holdingHooks) WatchBindEvents(context.Context) (<-chan models.IngressEvent, error) {
	return h.events, nil
}

// New takes the hooks' hold, and Start serves its park; when recording ends
// (the bind events end), keploy says how many calls the hold served, the
// window it covered.
func TestStartServesTheHoldsParkAndTheEndSaysWhatItServed(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	captured := make(chan uint16, 1)
	stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase,
		_ *http.Request, _ *http.Response, _ time.Time, _ time.Time,
		_ models.IncomingOptions, _ bool, _ bool, appPort uint16) {
		captured <- appPort
	})
	app := oneShotUpstream(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	const dialed = 18082
	park := parkListener{Listener: loopbackListener(t), orig: dialed}
	holder := &fakePortHolder{park: park, moved: map[uint16]uint16{dialed: portOf(t, app)}, held: true, parked: 2}
	hooks := holdingHooks{fakePortHolder: holder, events: make(chan models.IngressEvent)}
	core, logs := observer.New(zapcore.InfoLevel)
	pm := New(zap.New(core), hooks, &config.Config{})
	t.Cleanup(pm.captures.Wait)
	if pm.portHolder == nil {
		t.Fatal("New did not take the hooks' hold")
	}
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	defer cancel()
	pm.Start(ctx, models.IncomingOptions{})

	c, err := net.Dial("tcp4", park.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second)) // an unserved park fails the test, not the run
	if _, err := io.WriteString(c, "GET /early HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if resp, err := http.ReadResponse(bufio.NewReader(c), nil); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the parked call through Start: %v %v", resp, err)
	}
	select {
	case port := <-captured:
		if port != dialed {
			t.Fatalf("recorded for port %d, want %d", port, dialed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked call through Start was not recorded")
	}

	close(hooks.events) // recording ends
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if l := logs.FilterMessageSnippet("served calls to the application's port that arrived before its forwarder listened").All(); len(l) == 1 {
			if n := l[0].ContextMap()["count"]; n != uint64(2) {
				t.Fatalf("the end said count %v, want 2", n)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the end did not say what the hold served: %v", logs.All())
		}
	}
}
