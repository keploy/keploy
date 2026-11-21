package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/neterr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// While the hooks steer, the app listens on its own port, and the park takes
// the calls its sockets would have taken: each is forwarded to the address
// the caller dialed (the parked connection's local address), from a dial the
// hooks mark as keploy's own, and recorded on that port. Nothing is moved, so
// nothing names another port.
func TestASteeredCallIsForwardedToWhereItWasSentAndRecorded(t *testing.T) {
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
	port := portOf(t, app)
	park := parkListener{Listener: loopbackListener(t), orig: int(port)}
	holder := &fakePortHolder{park: park, moved: map[uint16]uint16{}, held: true, steers: true}
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
		t.Fatalf("the steered call got no answer: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("the steered call got %d %q, want the app's 200 ok", resp.StatusCode, body)
	}
	select {
	case got := <-captured:
		if got != port {
			t.Fatalf("the steered call was recorded for port %d, want %d", got, port)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the steered call was not recorded")
	}
	holder.mu.Lock()
	dials := append([]string(nil), holder.ownDials...)
	noted := append([]string(nil), holder.noted...)
	holder.mu.Unlock()
	if want := "127.0.0.1:" + strconv.Itoa(int(port)); len(dials) != 1 || dials[0] != want {
		t.Fatalf("keploy's own dials %q, want one to %s, marked as keploy's", dials, want)
	}
	if len(noted) != 1 {
		t.Fatalf("keploy asked %d time(s) whether its connection was noted as its own (%q), want once", len(noted), noted)
	}
	if n := pm.steeredServed.Load(); n != 1 {
		t.Fatalf("steered count %d, want 1", n)
	}
}

// A steered call is forwarded where the port holder says the app's socket
// that takes it can be reached (SteeredDialTarget: the loopback, for a socket
// bound to any address), not always to the address it was sent to.
func TestASteeredCallIsForwardedWhereThePortHolderSays(t *testing.T) {
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
	port := portOf(t, app)
	// The call was sent to 127.0.0.2, where nothing listens: only a forward
	// to where the holder says reaches the app.
	sent := netip.MustParseAddrPort("127.0.0.2:" + strconv.Itoa(int(port)))
	park := parkListener{Listener: loopbackListener(t), orig: int(port), ip: net.IPv4(127, 0, 0, 2)}
	holder := &fakePortHolder{park: park, moved: map[uint16]uint16{}, held: true, steers: true,
		steerTo: map[netip.AddrPort]netip.AddrPort{sent: netip.MustParseAddrPort(app)}}
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
		t.Fatalf("the steered call got no answer: %v", err)
	}
	_ = resp.Body.Close()
	select {
	case got := <-captured:
		if got != port {
			t.Fatalf("the steered call was recorded for port %d, want %d", got, port)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the steered call was not recorded")
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if len(holder.ownDials) != 1 || holder.ownDials[0] != app {
		t.Fatalf("keploy's own dials %q, want one to %s, where the holder said", holder.ownDials, app)
	}
}

// A connection the kernel could not note as keploy's own (OwnConnNoted) may
// have been steered back to keploy: it is closed, the call is not forwarded
// over it, and nothing is recorded.
func TestAConnectionTheKernelDidNotNoteAsKeploysIsNotUsed(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	stubCaptureHook(t, func(context.Context, *zap.Logger, chan *models.TestCase,
		*http.Request, *http.Response, time.Time, time.Time, models.IncomingOptions, bool, bool, uint16) {
		t.Error("a call keploy did not forward was recorded")
	})
	app := loopbackListener(t)
	got := make(chan error, 1)
	go func() {
		c, err := app.Accept()
		if err != nil {
			got <- err
			return
		}
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err = c.Read(make([]byte, 1))
		got <- err
	}()
	port := portOf(t, app.Addr().String())
	park := parkListener{Listener: loopbackListener(t), orig: int(port)}
	holder := &fakePortHolder{park: park, moved: map[uint16]uint16{}, held: true, steers: true, notedErr: errors.New("the kernel could not note it")}
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
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !neterr.IsPeerReset(err) {
		t.Fatalf("read %v, want the call closed", err)
	}
	if err := <-got; !errors.Is(err, io.EOF) {
		t.Fatalf("the app's end of keploy's connection read %v, want it closed with nothing sent", err)
	}
}

// A dial the hooks cannot mark as keploy's own is not made: the lookup would
// steer it back to keploy. The call is closed, nothing recorded.
func TestASteeredCallKeployCannotMarkIsNotDialed(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	stubCaptureHook(t, func(context.Context, *zap.Logger, chan *models.TestCase,
		*http.Request, *http.Response, time.Time, time.Time, models.IncomingOptions, bool, bool, uint16) {
		t.Error("a call keploy could not forward was recorded")
	})
	accepted := make(chan struct{}, 1)
	app := loopbackListener(t)
	go func() {
		if c, err := app.Accept(); err == nil {
			accepted <- struct{}{}
			_ = c.Close()
		}
	}()
	port := portOf(t, app.Addr().String())
	park := parkListener{Listener: loopbackListener(t), orig: int(port)}
	holder := &fakePortHolder{park: park, moved: map[uint16]uint16{}, held: true, steers: true, ownDialErr: errors.New("ingress_own is full")}
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
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !neterr.IsPeerReset(err) {
		t.Fatalf("read %v, want the call closed", err)
	}
	select {
	case <-accepted:
		t.Fatal("keploy connected to the app from a socket it could not mark as its own")
	case <-time.After(200 * time.Millisecond):
	}
}

// While the hooks steer, a bind event starts no forwarder: keploy binds none
// of the app's ports (the app holds its own), says the port is forwarded,
// steered, and the app listens on the port it asked for; and the hooks keep
// the port in the pod's steer table, for an agent restarted in the pod, at
// each of its bind events: each is a socket's note.
func TestASteeredPortStartsNoForwarder(t *testing.T) {
	app := loopbackListener(t) // the app holds its port, as it does while keploy steers
	port := portOf(t, app.Addr().String())
	core, logs := observer.New(zapcore.InfoLevel)
	holder := &fakePortHolder{held: true, steers: true}
	pm := &IngressProxyManager{logger: zap.New(core), active: map[uint16]proxyStop{}, portHolder: holder}
	hook := &recordingHook{}
	pm.ingressHook = hook
	pm.StartIngressProxy(context.Background(), port, port)
	if hook.started != 0 {
		t.Fatalf("the ingress hook was started %d times, want none: no forwarder binds a steered port", hook.started)
	}
	if got, ok := pm.AppListenPort(port); !ok || got != port {
		t.Fatalf("AppListenPort(%d) = %d, %v; want the port itself", port, got, ok)
	}
	if n := logs.FilterMessage("Started ingress forwarding").Len(); n != 1 {
		t.Fatalf("%d 'Started ingress forwarding' lines, want 1", n)
	}
	holder.mu.Lock()
	reg, kept := len(holder.registered), holder.kept
	holder.mu.Unlock()
	if reg != 0 {
		t.Fatalf("%d forwarders registered, want none", reg)
	}
	if kept != 1 {
		t.Fatalf("the steer table was kept %d time(s), want once: an agent restarted in the pod steers the port again from it", kept)
	}
	// Another socket of the app's bound on the port (another address, or
	// IPv6): the forwarding is the same, and the table is kept again, with
	// that socket's note.
	pm.StartIngressProxy(context.Background(), port, port)
	holder.mu.Lock()
	kept = holder.kept
	holder.mu.Unlock()
	if kept != 2 {
		t.Fatalf("the steer table was kept %d time(s) after a second bind event of the port, want twice: its second socket's note is steered again only from the table", kept)
	}
	if n := logs.FilterMessage("Started ingress forwarding").Len(); n != 1 || hook.started != 0 {
		t.Fatalf("%d 'Started ingress forwarding' lines and %d forwarders after the second bind event, want 1 and none", n, hook.started)
	}
	pm.StopAll()
}

// recordingHook counts the forwarders the manager starts.
type recordingHook struct{ started, stopped int }

func (h *recordingHook) StartIngress(context.Context, uint16, uint16) error {
	h.started++
	return nil
}
func (h *recordingHook) StopIngress(uint16) error {
	h.stopped++
	return nil
}

// The lookup steers to the park only once the manager accepts on it:
// serveParkedConnections arms the park as its loop starts. A park that
// cannot be armed is stopped at once, with one WARN, and its loop ends: a
// call is the kernel's, the app's own socket's while the hooks steer.
func TestTheParkIsArmedAsItsAcceptLoopStarts(t *testing.T) {
	park := parkListener{Listener: loopbackListener(t), orig: 18082}
	holder := &fakePortHolder{park: park, moved: map[uint16]uint16{}, held: true, steers: true}
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pm.serveParkedConnections(ctx, park)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		holder.mu.Lock()
		armed := holder.armed
		holder.mu.Unlock()
		if armed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the park was armed %d times, want once as its loop started", armed)
		}
	}

	refusedPark := parkListener{Listener: loopbackListener(t), orig: 18083}
	refused := &fakePortHolder{park: refusedPark, held: true, steers: true, armErr: errors.New("the sockmap refused the park")}
	core, logs := observer.New(zapcore.WarnLevel)
	pm2 := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: refused}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pm2.serveParkedConnections(ctx, refusedPark)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop of a park that could not be armed did not end")
	}
	refused.mu.Lock()
	stopped := refused.parkStopped
	refused.mu.Unlock()
	if stopped != 1 || logs.Len() != 1 {
		t.Fatalf("park stopped %d times, %d WARNs; want 1 and 1", stopped, logs.Len())
	}
}

// While the hooks steer, every dial keploy makes to the app is its own,
// the wait's poll for an app that has not listened yet among them: between
// the app's bind and its listen the lookup would steer an unmarked dial back
// to keploy's park, and the call would be captured inside itself. The
// poll's connection, handed to the call, is checked as noted too.
func TestAWaitsPollForTheAppIsKeploysOwnDial(t *testing.T) {
	addr := sharedAppAddr(t)
	holder := &fakePortHolder{held: true, steers: true}
	pm := New(zap.NewNop(), nil, &config.Config{})
	pm.portHolder = holder
	ch := dialAsync(context.Background(), pm, addr, nil)
	time.Sleep(200 * time.Millisecond) // the first dial refused, the poll begun
	l := testnet.ListenShared(t, "tcp4", addr)
	r := result(t, ch)
	if r.err != nil {
		t.Fatalf("dialApp returned error: %v", r.err)
	}
	_ = r.conn.Close()
	if n := accepted(t, l); n != 1 {
		t.Fatalf("the app saw %d connections, want the call's one", n)
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	if len(holder.ownDials) < 2 {
		t.Fatalf("keploy's own dials %q, want the call's first and the poll's, each marked as keploy's", holder.ownDials)
	}
	if len(holder.noted) != 1 || holder.noted[0] != r.conn.LocalAddr().String() {
		t.Fatalf("connections checked as noted %q, want the one handed to the call (%s)", holder.noted, r.conn.LocalAddr())
	}
}
