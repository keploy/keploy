package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
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
	yielded      []uint16
	parkStopped  int
	lookups      map[string]uint64

	ipv6    map[uint16][]netip.Addr // AppIPv6Binds's answer, by port
	ipv6Err error
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
func (f *fakePortHolder) YieldIngressPort(orig uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.yielded = append(f.yielded, orig)
}
func (f *fakePortHolder) StopIngressPark() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parkStopped++
}
func (f *fakePortHolder) AppIPv6Binds(orig uint16) ([]netip.Addr, error) {
	return f.ipv6[orig], f.ipv6Err
}
func (f *fakePortHolder) IngressHeld() bool                      { return f.held }
func (f *fakePortHolder) IngressLookupCounts() map[string]uint64 { return f.lookups }

// parkListener is a loopback listener whose connections report port orig as
// their local address, as the kernel's sk_lookup leaves a parked connection's:
// the port the caller dialed, not the park's own.
// An IPv6 one (ip set) reports [ip]:orig, as a parked IPv6 call's does.
type parkListener struct {
	net.Listener
	orig int
	ip   net.IP
}

func (p parkListener) Accept() (net.Conn, error) {
	c, err := p.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return origConn{Conn: c, port: p.orig, ip: p.ip}, nil
}

type origConn struct {
	net.Conn
	port int
	ip   net.IP
}

func (c origConn) LocalAddr() net.Addr {
	if c.ip != nil {
		return &net.TCPAddr{IP: c.ip, Port: c.port}
	}
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

// The forwarder starts, and is registered with the hold, at once, whether or
// not the app listens on its moved port yet: a call that comes before the app
// listens waits for it in its dial (dialApp), and is answered once it does.
// The forwarder used to wait up to 3 s for the app first, on 25 ms dials that
// a loaded host timed out against an app that listened, with a false WARN,
// and opened and closed a connection to the app on each start.
func TestAForwarderStartsAtOnceAndItsCallsWaitForTheApp(t *testing.T) {
	holder := &fakePortHolder{held: true}
	core, logs := observer.New(zapcore.WarnLevel)
	pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
	t.Cleanup(pm.captures.Wait)
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	defer cancel()
	app := sharedAppAddr(t) // the app has not listened yet
	orig := portOf(t, freeAddr(t))
	g := newGoTCPIngressHook(pm)
	if err := g.StartIngress(ctx, orig, portOf(t, app)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.StopIngress(orig) })
	holder.mu.Lock()
	registered := holder.registered[orig] != nil
	holder.mu.Unlock()
	if !registered {
		t.Fatal("the forwarder was not registered with the hold before the app listened")
	}

	c, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(orig))))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	answered := callAndRead(t, c, "/")
	awaitWaiters(t, pm, app, 1)
	ln := testnet.ListenShared(t, "tcp4", app)
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	if status := <-answered; status != "200 OK" {
		t.Fatalf("a call that came before the app listened: %s, want the app's answer", status)
	}
	if all := logs.All(); len(all) != 0 {
		t.Fatalf("logged %v, want nothing at WARN or above", all)
	}
}

// scriptedPark is a park listener whose Accepts return, in turn, what the
// test sends on results (an error, or a connection), and block once it is
// closed or nothing is sent.
type scriptedPark struct {
	results chan any
	closed  chan struct{}
	once    sync.Once
	accepts chan struct{}
}

func newScriptedPark() *scriptedPark {
	return &scriptedPark{results: make(chan any, 8), closed: make(chan struct{}), accepts: make(chan struct{}, 64)}
}

func (p *scriptedPark) Accept() (net.Conn, error) {
	p.accepts <- struct{}{}
	select {
	case r := <-p.results:
		if err, ok := r.(error); ok {
			return nil, err
		}
		return r.(net.Conn), nil
	case <-p.closed:
		return nil, net.ErrClosed
	}
}
func (p *scriptedPark) Close() error   { p.once.Do(func() { close(p.closed) }); return nil }
func (p *scriptedPark) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv6unspecified} }
func (p *scriptedPark) isClosed() bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

func acceptErr(errno syscall.Errno) error {
	return &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", errno)}
}

// A park Accept that fails for want of a resource (out of file descriptors)
// is retried, with one WARN, and the call behind it is then served: the park
// stays in the lookup.
func TestAParkThatRunsOutOfDescriptorsKeepsAccepting(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	captured := make(chan uint16, 1)
	stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase,
		_ *http.Request, _ *http.Response, _ time.Time, _ time.Time,
		_ models.IncomingOptions, _ bool, _ bool, appPort uint16) {
		captured <- appPort
	})
	app := oneShotUpstream(t, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	const dialed = 18083
	holder := &fakePortHolder{moved: map[uint16]uint16{dialed: portOf(t, app)}, held: true}
	core, logs := observer.New(zapcore.WarnLevel)
	pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
	t.Cleanup(pm.captures.Wait)
	park := newScriptedPark()
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	done := make(chan struct{})
	go func() { pm.serveParkedConnections(ctx, park); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	real := parkListener{Listener: loopbackListener(t), orig: dialed}
	c, err := net.Dial("tcp4", real.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	served, err := real.Accept()
	if err != nil {
		t.Fatal(err)
	}
	park.results <- acceptErr(syscall.EMFILE)
	park.results <- acceptErr(syscall.ENFILE)
	park.results <- served
	if status := <-callAndRead(t, c, "/after-emfile"); status != "200 OK" {
		t.Fatalf("the call behind the failed accepts: %s", status)
	}
	select {
	case port := <-captured:
		if port != dialed {
			t.Fatalf("recorded for port %d, want %d", port, dialed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call behind the failed accepts was not recorded")
	}
	holder.mu.Lock()
	stopped := holder.parkStopped
	holder.mu.Unlock()
	if stopped != 0 || park.isClosed() {
		t.Fatalf("the park was stopped (%d) or closed (%v) on a recoverable accept error", stopped, park.isClosed())
	}
	if w := logs.FilterMessageSnippet("for want of a resource").All(); len(w) != 1 {
		t.Fatalf("%d WARNs for two failed accepts, want one: %v", len(w), logs.All())
	}
}

// A park Accept that fails for good stops the park: it is taken out of the
// lookup (StopIngressPark) and closed, with a WARN, so a call to the port is
// refused, as without the hold, never left to hang in a park nothing
// accepts.
func TestAParkThatCannotAcceptIsStopped(t *testing.T) {
	holder := &fakePortHolder{held: true}
	core, logs := observer.New(zapcore.WarnLevel)
	pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
	park := newScriptedPark()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { pm.serveParkedConnections(ctx, park); close(done) }()
	park.results <- acceptErr(syscall.EINVAL)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the park's accept loop went on after an accept error it cannot get past")
	}
	holder.mu.Lock()
	stopped := holder.parkStopped
	holder.mu.Unlock()
	if stopped != 1 || !park.isClosed() {
		t.Fatalf("park stopped %d times, closed %v; want it stopped once and closed", stopped, park.isClosed())
	}
	if w := logs.FilterMessageSnippet("ingress hold stopped").All(); len(w) != 1 {
		t.Fatalf("WARNs %v, want one saying the hold stopped", logs.All())
	}
}

// stubParkRetryWait replaces the park's retry wait: each pause the loop asks
// for is sent on the returned channel, and the wait reports what more says
// (true: the pause passed).
func stubParkRetryWait(t *testing.T, more func() bool) <-chan time.Duration {
	t.Helper()
	pauses := make(chan time.Duration, 64)
	saved := parkRetryWait
	t.Cleanup(func() { parkRetryWait = saved })
	parkRetryWait = func(_ context.Context, d time.Duration) bool {
		pauses <- d
		return more()
	}
	return pauses
}

// The park's retries after Accepts that fail for want of a resource (out of
// descriptors, buffers or memory) pause 5 ms, then twice as long each time,
// never more than 1 s; a good Accept starts the pauses again from 5 ms. Each
// is retried: the park is never stopped for one, and one WARN says so.
func TestTheParksRetryPausesGrowTo1sAndStartAgainAfterAGoodAccept(t *testing.T) {
	pauses := stubParkRetryWait(t, func() bool { return true })
	holder := &fakePortHolder{held: true}
	core, logs := observer.New(zapcore.WarnLevel)
	pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
	park := newScriptedPark()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pm.serveParkedConnections(ctx, park); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	ms := time.Millisecond
	steps := []struct {
		result any
		pause  time.Duration // 0: a good Accept, no pause
	}{
		{acceptErr(syscall.EMFILE), 5 * ms}, {acceptErr(syscall.ENFILE), 10 * ms}, {acceptErr(syscall.EMFILE), 20 * ms},
		{acceptErr(syscall.EMFILE), 40 * ms}, {acceptErr(syscall.EMFILE), 80 * ms}, {acceptErr(syscall.EMFILE), 160 * ms},
		{acceptErr(syscall.EMFILE), 320 * ms}, {acceptErr(syscall.EMFILE), 640 * ms}, {acceptErr(syscall.EMFILE), time.Second},
		{acceptErr(syscall.EMFILE), time.Second}, {acceptErr(syscall.EMFILE), time.Second},
		{nil, 0}, // a good Accept
		{acceptErr(syscall.ENOBUFS), 5 * ms}, {acceptErr(syscall.ENOMEM), 10 * ms},
	}
	for i, st := range steps {
		if st.result == nil {
			a, b := net.Pipe() // not a TCP connection: the park closes it
			t.Cleanup(func() { _ = b.Close() })
			park.results <- a
			continue
		}
		park.results <- st.result
		select {
		case got := <-pauses:
			if got != st.pause {
				t.Fatalf("step %d (%v): paused %v, want %v", i, st.result, got, st.pause)
			}
		case <-done:
			t.Fatalf("step %d (%v): the park's accept loop ended instead of retrying", i, st.result)
		}
	}
	holder.mu.Lock()
	stopped := holder.parkStopped
	holder.mu.Unlock()
	if stopped != 0 || park.isClosed() {
		t.Fatalf("the park was stopped (%d) or closed (%v) on accept errors it can get past", stopped, park.isClosed())
	}
	if w := logs.FilterMessageSnippet("for want of a resource").All(); len(w) != 1 {
		t.Fatalf("%d WARNs for the failed accepts, want one: %v", len(w), logs.All())
	}
}

// When recording ends (the context is canceled), the park's accept loop just
// returns: its Accept then fails because the park is closed, or its retry
// pause is cut short, and neither stops the hold's park (StopIngressPark) or
// logs a WARN, which are for a park that cannot accept.
func TestARecordingThatEndsLeavesTheParkUnstoppedAndSaysNothing(t *testing.T) {
	t.Run("in its Accept", func(t *testing.T) {
		holder := &fakePortHolder{held: true}
		core, logs := observer.New(zapcore.WarnLevel)
		pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
		park := newScriptedPark()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { pm.serveParkedConnections(ctx, park); close(done) }()
		<-park.accepts
		cancel() // the park is closed, and its Accept fails with net.ErrClosed
		<-done
		holder.mu.Lock()
		stopped := holder.parkStopped
		holder.mu.Unlock()
		if stopped != 0 || logs.Len() != 0 {
			t.Fatalf("a recording's end stopped the park %d times and logged %v", stopped, logs.All())
		}
	})
	t.Run("in its retry pause", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		pauses := stubParkRetryWait(t, func() bool { cancel(); return false })
		holder := &fakePortHolder{held: true}
		core, logs := observer.New(zapcore.WarnLevel)
		pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
		park := newScriptedPark()
		done := make(chan struct{})
		go func() { pm.serveParkedConnections(ctx, park); close(done) }()
		park.results <- acceptErr(syscall.EMFILE)
		<-done
		if len(pauses) != 1 || len(park.accepts) != 1 {
			t.Fatalf("the loop paused %d times and accepted %d times, want one each: no Accept after a pause the recording's end cut short", len(pauses), len(park.accepts))
		}
		holder.mu.Lock()
		stopped := holder.parkStopped
		holder.mu.Unlock()
		if stopped != 0 || len(logs.FilterMessageSnippet("ingress hold stopped").All()) != 0 {
			t.Fatalf("a recording's end in a retry pause stopped the park %d times: %v", stopped, logs.All())
		}
	})
}

// The park's retry wait passes its pause, and ends at once, false, with the
// recording's context: a canceled one never waits out even an hour's pause.
func TestTheParksRetryWaitEndsWithTheRecording(t *testing.T) {
	// It waits the whole pause, at least (a lower bound, which any timer
	// meets: not a budget): the pause is what keeps a park out of file
	// descriptors from spinning on Accept.
	const pause = 20 * time.Millisecond
	began := time.Now()
	if !parkRetryWait(context.Background(), pause) {
		t.Fatal("the retry wait did not pass its pause")
	}
	if waited := time.Since(began); waited < pause {
		t.Fatalf("the retry wait passed after %v, before its pause of %v", waited, pause)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := make(chan bool, 1)
	go func() { res <- parkRetryWait(ctx, time.Hour) }()
	select {
	case ok := <-res:
		if ok {
			t.Fatal("the retry wait passed on a canceled context")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the retry wait outlived its canceled context")
	}
}

// The end of recording counts the hold's IPv4 and IPv6 calls apart: an IPv4
// call reaches the park only before the port's forwarder listens, an IPv6
// one (an app bound to :: takes both families) for the whole run, since
// keploy's forwarder listens on IPv4 only.
func TestTheEndCountsTheHoldsIPv6CallsApart(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	captured := make(chan uint16, 2)
	stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase,
		_ *http.Request, _ *http.Response, _ time.Time, _ time.Time,
		_ models.IncomingOptions, _ bool, _ bool, appPort uint16) {
		captured <- appPort
	})
	const d4, d6 = 18084, 18085
	ok := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	holder := &fakePortHolder{moved: map[uint16]uint16{d4: portOf(t, oneShotUpstream(t, ok)), d6: portOf(t, oneShotUpstream(t, ok))}, held: true}
	core, logs := observer.New(zapcore.InfoLevel)
	pm := &IngressProxyManager{logger: zap.New(core), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
	t.Cleanup(pm.captures.Wait)
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	defer cancel()
	for _, park := range []parkListener{{Listener: loopbackListener(t), orig: d4}, {Listener: loopbackListener(t), orig: d6, ip: net.IPv6loopback}} {
		go pm.serveParkedConnections(ctx, park)
		c, err := net.Dial("tcp4", park.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if status := <-callAndRead(t, c, "/"); status != "200 OK" {
			t.Fatalf("the parked call to %d: %s", park.orig, status)
		}
		select {
		case <-captured:
		case <-time.After(10 * time.Second):
			t.Fatalf("the parked call to %d was not recorded", park.orig)
		}
	}
	pm.logIngressHoldEnd()
	for snippet, want := range map[string]uint64{
		"that arrived before its forwarder listened": 1,
		"held IPv6 calls to the application's port":  1,
	} {
		l := logs.FilterMessageSnippet(snippet).All()
		if len(l) != 1 || l[0].ContextMap()["count"] != want {
			t.Fatalf("the end's %q lines: %v, want one with count %d", snippet, l, want)
		}
	}
}

// A forwarder that starts while the port is held is registered with the
// hold, so the lookup steers the port to it instead of the park; stopped, it
// is unregistered.
func TestAForwarderIsRegisteredWithTheHoldAndUnregisteredWhenItStops(t *testing.T) {
	holder := &fakePortHolder{held: true}
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder}
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
	if pm.ingressPark() != nil || pm.ingressHeld() {
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
func (holdingHooks) Delete(context.Context, uint16) error                       { return nil }
func (holdingHooks) Load(context.Context, agent.HookCfg, config.Agent) error    { return nil }
func (h holdingHooks) WatchBindEvents(context.Context) (<-chan models.IngressEvent, error) {
	return h.events, nil
}

// New takes the hooks' hold, and Start serves its park; when recording ends
// (the bind events end), keploy says how many calls the hold served, the
// window it covered: its own count of the parked calls it forwarded, not the
// lookup's (here 3 lookups, as a retransmitted SYN makes, for one call),
// which it gives at Debug.
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
	holder := &fakePortHolder{park: park, moved: map[uint16]uint16{dialed: portOf(t, app)}, held: true, lookups: map[string]uint64{"parked": 3}}
	hooks := holdingHooks{fakePortHolder: holder, events: make(chan models.IngressEvent)}
	core, logs := observer.New(zapcore.DebugLevel)
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
			if n := l[0].ContextMap()["count"]; n != uint64(1) || l[0].Level != zapcore.InfoLevel {
				t.Fatalf("the end said count %v at %v, want 1 at info", n, l[0].Level)
			}
			d := logs.FilterMessageSnippet("the ingress hold's listener lookups").All()
			if len(d) != 1 || d[0].Level != zapcore.DebugLevel || fmt.Sprint(d[0].ContextMap()["lookups"]) != "map[parked:3]" {
				t.Fatalf("the end's Debug line of the lookup's counts: %v", d)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the end did not say what the hold served: %v", logs.All())
		}
	}
}

// heldUpstream is an app that answers each call only once the test lets it:
// got receives each request as it arrives, and each send on answer answers
// one.
func heldUpstream(t *testing.T) (addr string, got <-chan string, answer chan<- struct{}) {
	t.Helper()
	ln := loopbackListener(t)
	gotc, answerc := make(chan string, 4), make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				gotc <- req.URL.Path
				<-answerc
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			}(c)
		}
	}()
	t.Cleanup(func() { close(answerc) })
	return ln.Addr().String(), gotc, answerc
}

// callAndRead sends a GET for path on c and returns a channel the answer's
// status (or the error) arrives on.
func callAndRead(t *testing.T, c net.Conn, path string) <-chan string {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second)) // an unanswered call fails the test, not the run
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			done <- err.Error()
			return
		}
		_ = resp.Body.Close()
		done <- resp.Status
	}()
	return done
}

// In synchronous recording, a port's calls are taken one at a time: a call
// the park serves and one the port's forwarder accepts take the port's one
// semaphore (portSem), as two of the forwarder's own do. While the app holds
// each, the port's slot is taken.
func TestAParkedCallAndTheForwardersShareThePortsSynchronousSlot(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	// Each call's capture is waited for below: its connection's goroutine
	// (the park's or the forwarder's, which nothing joins) starts the
	// capture after it answers, and pm.captures.Wait in the cleanup counts
	// only a capture that started before it.
	captured := make(chan string, 2)
	stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase,
		req *http.Request, _ *http.Response, _ time.Time, _ time.Time,
		_ models.IncomingOptions, _ bool, _ bool, _ uint16) {
		captured <- req.URL.Path
	})
	app, got, answer := heldUpstream(t)
	orig := portOf(t, freeAddr(t))
	park := parkListener{Listener: loopbackListener(t), orig: int(orig)}
	holder := &fakePortHolder{park: park, moved: map[uint16]uint16{orig: portOf(t, app)}, held: true}
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), samplingSem: make(chan struct{}, 1), portHolder: holder, synchronous: true}
	t.Cleanup(pm.captures.Wait)
	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())))
	defer cancel()
	go pm.serveParkedConnections(ctx, park)

	for _, via := range []string{"park", "forwarder"} {
		if via == "forwarder" {
			g := newGoTCPIngressHook(pm)
			if err := g.StartIngress(ctx, orig, portOf(t, app)); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = g.StopIngress(orig) })
		}
		addr := park.Addr().String()
		if via == "forwarder" {
			addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(orig)))
		}
		c, err := net.Dial("tcp4", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		done := callAndRead(t, c, "/"+via)
		select {
		case p := <-got:
			if p != "/"+via {
				t.Fatalf("the app got %s, want /%s", p, via)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s's call did not reach the app", via)
		}
		if n := len(pm.portSem(orig)); n != 1 {
			t.Fatalf("while the app holds the %s's call, the port's synchronous slot holds %d, want 1", via, n)
		}
		answer <- struct{}{}
		if s := <-done; s != "200 OK" {
			t.Fatalf("the %s's call: %s", via, s)
		}
		select {
		case p := <-captured:
			if p != "/"+via {
				t.Fatalf("recorded %s, want /%s", p, via)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s's call was not recorded", via)
		}
	}
}

// gatedIngressHook is a forwarder whose start returns only once release is
// closed, and says on entered that it began: the window in which the manager
// has reserved the port for it but does not yet know where the app went.
type gatedIngressHook struct {
	entered chan uint16
	release chan struct{}
}

func (g gatedIngressHook) StartIngress(_ context.Context, orig, _ uint16) error {
	g.entered <- orig
	<-g.release
	return nil
}
func (gatedIngressHook) StopIngress(uint16) error { return nil }

// Under the hold a call to the app's port is served and recorded as a test on
// that port before keploy's forwarder listens there, so the record's question
// of where the app listens on it (the unpublished- and loopback-only-port
// warnings) can come before the forwarder has started, or while it starts.
// The app's socket is then on the port its bind was moved to, which the hold
// knows from the bind itself: answering the app's own port there reads
// nothing listening (no socket is on it until the forwarder's), and answering
// "not known" says nothing.
func TestAppListenPortIsTheHeldMoveBeforeTheForwarderListens(t *testing.T) {
	hook := gatedIngressHook{entered: make(chan uint16, 1), release: make(chan struct{})}
	pm := &IngressProxyManager{logger: zap.NewNop(), active: make(map[uint16]proxyStop), ingressHook: hook,
		portHolder: &fakePortHolder{held: true, moved: map[uint16]uint16{8096: 45629, 8098: 41000}}}

	if port, ok := pm.AppListenPort(8096); !ok || port != 45629 {
		t.Fatalf("held, before its bind event is read: got %d, %v; want the moved bind 45629", port, ok)
	}
	if port, ok := pm.AppListenPort(8095); !ok || port != 8095 {
		t.Fatalf("a port the hooks did not move: got %d, %v; want the app's own port", port, ok)
	}

	started := make(chan struct{})
	go func() {
		defer close(started)
		pm.StartIngressProxy(context.Background(), 8096, 45629)
	}()
	if orig := <-hook.entered; orig != 8096 {
		t.Fatalf("the forwarder started for %d, want 8096", orig)
	}
	if port, ok := pm.AppListenPort(8096); !ok || port != 45629 {
		t.Fatalf("held, while its forwarder starts: got %d, %v; want the moved bind 45629", port, ok)
	}
	close(hook.release)
	<-started
	if port, ok := pm.AppListenPort(8096); !ok || port != 45629 {
		t.Fatalf("forwarded: got %d, %v; want the moved bind 45629", port, ok)
	}

	// A bind event without the new port: the hold still knows it.
	pm.StartIngressProxy(context.Background(), 8098, 0)
	if port, ok := pm.AppListenPort(8098); !ok || port != 41000 {
		t.Fatalf("forwarded, the bind event without the new port: got %d, %v; want the hold's 41000", port, ok)
	}
}
