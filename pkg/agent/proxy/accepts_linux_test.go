//go:build linux

package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// fakeCounter stands in for the hooks (agent.ListenerCounter): its mark notes
// what the socket it was handed was at that moment, and fails with markErr
// when set; it numbers accepted connections 1, 2, ... as they are asked
// about, the order the kernel would give an in-order queue, and fails with
// seqErr when set.
type fakeCounter struct {
	mu        sync.Mutex
	calls     int
	protocol  int
	listening int
	markErr   error
	seqErr    error
	next      uint64
	// dst is the destination the kernel gives every application
	// connection after the canary; nil pairs none.
	dst *agent.NetworkAddress
}

func (f *fakeCounter) MarkProxyListener(rc syscall.RawConn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		f.protocol, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PROTOCOL)
		if gerr == nil {
			f.listening, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
		}
	}); err != nil {
		return err
	}
	if gerr != nil {
		return gerr
	}
	return f.markErr
}

func (f *fakeCounter) ProxyConnPair(syscall.RawConn) (agent.ProxyConnPairing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seqErr != nil {
		return agent.ProxyConnPairing{}, f.seqErr
	}
	f.next++
	p := agent.ProxyConnPairing{Seq: f.next}
	// The canary is the first: none of the application's. The rest are
	// paired with dst when it is set.
	if f.next > 1 && f.dst != nil {
		p.AppSeq = f.next - 1
		p.Dst = f.dst
	}
	return p, nil
}

// teardownGate is an auxiliary hook whose teardown says it began, then waits
// for the test to let it go.
type teardownGate struct {
	entered, release chan struct{}
}

func (g *teardownGate) AfterStart(context.Context, agent.Proxy) error { return nil }
func (g *teardownGate) BeforeTeardown(context.Context) error {
	close(g.entered)
	<-g.release
	return nil
}

// startListening runs the proxy's start() as the agent does, on a port of the
// kernel's choosing, counted by c (nil: none), and returns the proxy once it
// says it is ready, and a func that stops it and returns once start() has.
func startListening(t *testing.T, c agent.ListenerCounter, setup ...func(*Proxy)) (*Proxy, func()) {
	t.Helper()
	cfg := config.New()
	cfg.ProxyPort = 0
	p := New(zap.NewNop(), plainDestInfo{}, cfg)
	if c != nil {
		p.SetListenerCounter(c)
	}
	for _, f := range setup {
		f(p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done // the test binary's -timeout bounds it
		})
	}
	t.Cleanup(stop)
	if err := <-ready; err != nil {
		t.Fatalf("start() reported not-ready: %v", err)
	}
	return p, stop
}

func protocolOf(t *testing.T, ln net.Listener) int {
	t.Helper()
	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var proto int
	var gerr error
	if err := rc.Control(func(fd uintptr) { proto, gerr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PROTOCOL) }); err != nil {
		t.Fatal(err)
	}
	if gerr != nil {
		t.Fatal(gerr)
	}
	return proto
}

// Counted, the proxy listens plain TCP and hands the hooks its socket once,
// before listen(); it proves the count with a connection of its own before it
// is ready (the canary, number 1); then it takes every accepted connection's
// number.
func TestTheProxyMarksAPlainTCPListenerBeforeListenAndNumbersItsAccepts(t *testing.T) {
	f := &fakeCounter{}
	p, _ := startListening(t, f)
	if f.calls != 1 {
		t.Fatalf("the mark was asked for %d times, want once", f.calls)
	}
	if f.protocol != unix.IPPROTO_TCP {
		t.Fatalf("the socket handed to the mark is protocol %d, want plain TCP (%d)", f.protocol, unix.IPPROTO_TCP)
	}
	if f.listening != 0 {
		t.Fatal("the socket was handed to the mark after listen(): what the kernel completed before the mark is in neither count")
	}
	if got := protocolOf(t, p.Listener); got != unix.IPPROTO_TCP {
		t.Fatalf("the proxy listens with protocol %d, want plain TCP (%d)", got, unix.IPPROTO_TCP)
	}
	if n, err := p.Accepted(); err != nil || n != 1 {
		t.Fatalf("once ready: Accepted() = %d, %v; want 1, the canary", n, err)
	}
	addr := p.Listener.Addr().String()
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		// The proxy accepts it and, with no destination to name for it,
		// closes it: its end is the proof it was accepted. No deadline but
		// the test binary's -timeout.
		_, _ = c.Read(make([]byte, 1))
	}
	if n, err := p.Accepted(); err != nil || n != 4 {
		t.Fatalf("after three connections: Accepted() = %d, %v; want 4", n, err)
	}
	if err := p.WaitAccepted(context.Background(), 4); err != nil {
		t.Fatalf("WaitAccepted(4): %v", err)
	}
}

// A mark that fails leaves the proxy listening, and its counts unknown: never
// a 0 that a test's end could take for "nothing waiting".
func TestAFailedMarkLeavesTheCountUnknown(t *testing.T) {
	f := &fakeCounter{markErr: errors.New("proxy_listener_mark is not open")}
	p, _ := startListening(t, f)
	c, err := net.Dial("tcp", p.Listener.Addr().String())
	if err != nil {
		t.Fatalf("the proxy does not listen after a failed mark: %v", err)
	}
	_ = c.Close()
	n, err := p.Accepted()
	if !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "proxy_listener_mark is not open") {
		t.Fatalf("Accepted() after a failed mark = %d, %v; want ErrProxyConnsUnsupported naming the cause", n, err)
	}
	if err := p.WaitAccepted(context.Background(), 1); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("WaitAccepted after a failed mark: %v, want ErrProxyConnsUnsupported at once", err)
	}
}

// A count that is silently broken (the kernel gave the canary no number) is
// unknown from the start, not settled.
func TestABrokenCountIsUnknownFromTheStart(t *testing.T) {
	f := &fakeCounter{seqErr: errors.New("an accepted connection has no sequence number")}
	p, _ := startListening(t, f)
	if n, err := p.Accepted(); !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "no sequence number") {
		t.Fatalf("Accepted() with a count the canary found broken = %d, %v; want unknown, naming why", n, err)
	}
}

// A connection that reaches the listener before the canary's own is kept for
// the accept loop, never taken for the canary.
func TestTheCanaryKeepsAnEarlierConnection(t *testing.T) {
	cfg := config.New()
	p := New(zap.NewNop(), plainDestInfo{}, cfg)
	f := &fakeCounter{}
	p.SetListenerCounter(f)
	ln, err := p.listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	early, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer early.Close()
	p.canary(context.Background(), ln)
	kept := p.takeEarly()
	if kept == nil || kept.RemoteAddr().String() != early.LocalAddr().String() {
		t.Fatalf("the canary kept %v, want the earlier connection from %v", kept, early.LocalAddr())
	}
	defer kept.Close()
	if p.takeEarly() != nil {
		t.Fatal("the canary kept more than the earlier connection")
	}
	if n, err := p.Accepted(); err != nil || n != 1 {
		t.Fatalf("after the canary: Accepted() = %d, %v; want 1", n, err)
	}
}

// swallowingListener hands its accept only the connections from the ports in
// pass; every other it closes as it comes, and says so on swallowed: the
// canary's own connection never reaches the canary, as when another accept
// takes it.
type swallowingListener struct {
	net.Listener
	pass      map[int]bool
	swallowed chan struct{}
}

func (l swallowingListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if ra, ok := c.RemoteAddr().(*net.TCPAddr); ok && l.pass[ra.Port] {
			return c, nil
		}
		_ = c.Close()
		select {
		case l.swallowed <- struct{}{}:
		default:
		}
	}
}

// The proxy's stop ends a canary still waiting for its own connection: the
// proxy's shutdown, which removes the run's CA, runs only once the proxy has
// said it is ready. Nothing ended it, and a stop that came then waited until
// the CLI killed the agent for want of progress, the CA still installed. The
// count was never checked, so it is unknown; and the connection the canary
// kept for the accept loop, which returns at the stop, is closed.
func TestTheProxysStopEndsTheCanary(t *testing.T) {
	p := New(zap.NewNop(), plainDestInfo{}, config.New())
	p.SetListenerCounter(&fakeCounter{})
	ln, err := p.listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	early, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer early.Close()
	l := swallowingListener{Listener: ln, pass: map[int]bool{early.LocalAddr().(*net.TCPAddr).Port: true},
		swallowed: make(chan struct{}, 1)}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() { p.canary(ctx, l); close(done) }()
	select {
	case <-l.swallowed: // its own connection is gone: the canary waits for good
	case <-done:
		t.Fatal("the canary returned though its own connection never reached it")
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the canary never connected to the listener")
	}
	stop() // the proxy's stop
	select {
	case <-done:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the canary is still waiting for its own connection after the proxy's stop")
	}
	if n, err := p.Accepted(); !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Accepted() after a stop during the canary = %d, %v; want unknown, saying the proxy was stopped", n, err)
	}
	if p.takeEarly() != nil {
		t.Fatal("the canary left the accept loop a connection it will never take")
	}
	_ = early.SetReadDeadline(time.Now().Add(time.Minute)) // only turns a hang into a failure
	if _, err := early.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the connection the canary kept is still open after the stop (%v)", err)
	}
}

// unclosableListener is a listener its Close does not reach: a connect whose
// SYN a full queue dropped is not reached by the listener's close until the
// kernel's next retry of it, which can be a minute away.
type unclosableListener struct{ net.Listener }

func (unclosableListener) Close() error { return nil }

// The proxy's stop ends the canary's connect itself, not only by the
// listener's close: a stop that came before the connect is its answer at
// once, and the count, never checked, is unknown.
func TestTheProxysStopEndsTheCanarysConnect(t *testing.T) {
	p := New(zap.NewNop(), plainDestInfo{}, config.New())
	p.SetListenerCounter(&fakeCounter{})
	ln, err := p.listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ctx, stop := context.WithCancel(context.Background())
	stop() // the proxy's stop, before the canary's connect
	p.canary(ctx, unclosableListener{ln})
	if n, err := p.Accepted(); !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Accepted() after a stop before the canary's connect = %d, %v; want unknown, saying the proxy was stopped", n, err)
	}
}

// start() hands the canary its stop: a stop that came before the canary ends
// it, and start() still says it is ready, runs its shutdown, and returns; the
// count, never checked, says the proxy was stopped.
func TestAStopBeforeTheCanaryEndsTheStart(t *testing.T) {
	cfg := config.New()
	cfg.ProxyPort = 0
	p := New(zap.NewNop(), plainDestInfo{}, cfg)
	p.SetListenerCounter(&fakeCounter{})
	ctx, stop := context.WithCancel(context.Background())
	stop() // the proxy's stop, before its start
	ready := make(chan error, 1)
	if err := p.Serve(ctx, ready); err != nil {
		t.Fatalf("start() after a stop = %v, want nil", err)
	}
	if err := <-ready; err != nil {
		t.Fatalf("start() after a stop reported %v, want ready (nil)", err)
	}
	if n, err := p.Accepted(); !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("Accepted() after a stop before the canary = %d, %v; want unknown, saying the proxy was stopped", n, err)
	}
}

// stopAtCanaryCounter is a fakeCounter whose answer to the canary's own
// connection -- the first it is asked to pair -- comes with the proxy's stop:
// the stop lands just after the canary has checked the count and returned,
// before the accept loop has taken what the canary kept.
type stopAtCanaryCounter struct {
	*fakeCounter
	stop context.CancelFunc
	once sync.Once
}

func (c *stopAtCanaryCounter) ProxyConnPair(rc syscall.RawConn) (agent.ProxyConnPairing, error) {
	defer c.once.Do(c.stop)
	return c.fakeCounter.ProxyConnPair(rc)
}

// heldConns returns n connections, server ends and client ends, from a
// listener of their own: what the canary keeps (the server ends), and how the
// test sees each closed (the client ends).
func heldConns(t *testing.T, n int) (servers, clients []net.Conn) {
	t.Helper()
	aux, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer aux.Close()
	for i := 0; i < n; i++ {
		c, err := net.Dial("tcp", aux.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		s, err := aux.Accept()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		servers, clients = append(servers, s), append(clients, c)
	}
	return servers, clients
}

// A stop that lands just after the canary returned closes every connection
// the canary kept for the accept loop that the loop never took. They stayed
// open until the process exited: the loop returned at the stop, and nothing
// else held them (the one a round holds at the stop:
// TestAStopAsARoundTakesAKeptConnectionClosesIt).
func TestAStopJustAfterTheCanaryClosesWhatItKept(t *testing.T) {
	const kept = 8 // the loop takes about one before it sees the stop
	servers, clients := heldConns(t, kept)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	cfg := config.New()
	cfg.ProxyPort = 0
	p := New(zap.NewNop(), plainDestInfo{}, cfg)
	p.SetListenerCounter(&stopAtCanaryCounter{fakeCounter: &fakeCounter{}, stop: stop})
	for _, s := range servers {
		p.keepEarly(s) // kept by the canary, before its own
	}
	ready := make(chan error, 1)
	if err := p.Serve(ctx, ready); err != nil {
		t.Fatalf("start() = %v, want nil", err)
	}
	if err := <-ready; err != nil {
		t.Fatalf("start() reported %v, want ready", err)
	}
	for i, c := range clients {
		_ = c.SetReadDeadline(time.Now().Add(time.Minute)) // only turns a leak into a failure
		if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("kept connection %d of %d is still open after the proxy stopped (%v)", i+1, kept, err)
		}
	}
}

// A stop that lands as the loop's round has taken a kept connection, before
// it hands it over: the loop returns at the stop, and the connection is
// closed once the round ends, not left in its channel, open until the
// process exits.
func TestAStopAsARoundTakesAKeptConnectionClosesIt(t *testing.T) {
	servers, clients := heldConns(t, 1)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	cfg := config.New()
	cfg.ProxyPort = 0
	p := New(zap.NewNop(), plainDestInfo{}, cfg)
	p.SetListenerCounter(&fakeCounter{})
	p.keepEarly(servers[0])
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	// The stop lands as the round takes it, and the round holds it until
	// start() has returned: the loop can only have seen the stop.
	p.handingOver = func(net.Conn) { stop(); <-release }
	ready := make(chan error, 1)
	if err := p.Serve(ctx, ready); err != nil {
		t.Fatalf("start() = %v, want nil", err)
	}
	if err := <-ready; err != nil {
		t.Fatalf("start() reported %v, want ready", err)
	}
	releaseOnce()
	_ = clients[0].SetReadDeadline(time.Now().Add(time.Minute)) // only turns a leak into a failure
	if _, err := clients[0].Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the kept connection the round held is still open after the proxy stopped (%v)", err)
	}
}

// Not counted (the switch off): the proxy listens exactly as it always did,
// net.Listen, MPTCP wherever the kernel (and GODEBUG) has it, makes no canary
// connection, and says its counts are unknown.
func TestUncountedTheProxyListensAsBefore(t *testing.T) {
	p, _ := startListening(t, nil)
	ref, err := net.Listen("tcp", ":0") // what net.Listen makes here
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()
	if got, want := protocolOf(t, p.Listener), protocolOf(t, ref); got != want {
		t.Fatalf("the uncounted proxy listens with protocol %d, want net.Listen's %d", got, want)
	}
	if _, err := p.Accepted(); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("Accepted() uncounted: %v, want ErrProxyConnsUnsupported", err)
	}
	if _, err := p.ListenDrops(); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("ListenDrops() uncounted: %v, want ErrProxyConnsUnsupported", err)
	}
	if len(p.early) != 0 {
		t.Fatal("an uncounted proxy accepted a canary")
	}
}

// Once the listener closes (the proxy stopping, or its accept loop failing),
// every wait on the count ends, before the shutdown's slower steps (the
// auxiliary hook's teardown, the CA's removal) have run: a connection the
// kernel counted and nobody accepted is not waited on through them.
func TestAClosedListenerEndsTheWaitsAtOnce(t *testing.T) {
	gate := &teardownGate{entered: make(chan struct{}), release: make(chan struct{})}
	cfg := config.New()
	cfg.ProxyPort = 0
	p := New(zap.NewNop(), plainDestInfo{}, cfg)
	p.SetListenerCounter(&fakeCounter{})
	p.auxiliaryHook = gate
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- p.WaitAccepted(context.Background(), 2) }()
	cancel()
	<-gate.entered // the shutdown is in its slow steps
	if n, err := p.Accepted(); !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Accepted() during the teardown = %d, %v; want unknown, saying the listener closed", n, err)
	}
	if _, err := p.ListenDrops(); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("ListenDrops() during the teardown: %v, want unknown", err)
	}
	// No deadline of its own: the test binary's -timeout bounds it.
	if err := <-waited; !errors.Is(err, models.ErrProxyConnsUnsupported) || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("a wait across the listener's close: %v, want ErrProxyConnsUnsupported saying it closed", err)
	}
	close(gate.release)
	<-done
}

// The proxy's own marked listener (dual-stack, as the proxy opens it) reports
// the drops the kernel makes at it: with its accept queue shrunk to nothing
// and nobody accepting, a connection that does not fit is dropped there, and
// ListenDrops says so; a stuck 0 would read as verified.
func TestTheMarkedListenersDropsAreItsOwn(t *testing.T) {
	cfg := config.New()
	p := New(zap.NewNop(), plainDestInfo{}, cfg)
	p.SetListenerCounter(&fakeCounter{})
	ln, err := p.listen(context.Background(), ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if n, err := p.ListenDrops(); err != nil || n.Listener != 0 {
		t.Fatalf("a fresh marked listener's drops: %+v, %v; want 0", n, err)
	}
	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var lerr error
	if err := rc.Control(func(fd uintptr) { lerr = unix.Listen(int(fd), 0) }); err != nil || lerr != nil {
		t.Fatalf("shrink the accept queue: %v %v", err, lerr)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	for i := 0; i < 4; i++ {
		connectNoWait(t, port)
	}
	// The kernel's own count of the listener's drops moves as the
	// connects' packets are processed; no deadline but -timeout.
	var raw uint64
	for raw == 0 {
		if raw, err = listenerDrops(rc); err != nil {
			t.Fatal(err)
		}
		if raw == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	if n, err := p.ListenDrops(); err != nil || n.Listener < raw {
		t.Fatalf("the marked listener's drops: %+v, %v; want at least the kernel's %d", n, err, raw)
	}
}

// A namespace count keploy cannot read is said, never read as 0: ListenDrops
// gives it as unread, with why, so every drop at the listener counts (the
// agent's chargeDrops). A 0 there would charge every real drop min(own, 0),
// nothing.
func TestANamespaceCountNotReadIsUnreadNeverNone(t *testing.T) {
	p := New(zap.NewNop(), plainDestInfo{}, config.New())
	p.SetListenerCounter(&fakeCounter{})
	ln, err := p.listen(context.Background(), ":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// Without CAP_NET_ADMIN the listener's namespace cannot be told
	// (SIOCGSKNS) either: unread too, with that why. A process that holds
	// it (root, as CI's listener-drops job runs this row) must be told, so
	// that the netstat half below is checked where CI can fail it.
	d, err := p.ListenDrops()
	if err != nil || d.NetKnown != (d.NetUnread == "") {
		t.Fatalf("the namespace's count: %+v, %v; want it read, or unread with why", d, err)
	}
	told := d.NetKnown || !strings.Contains(d.NetUnread, "SIOCGSKNS")
	if !told && holdsNetAdmin(t) {
		t.Fatalf("the namespace's count with CAP_NET_ADMIN held: %+v; want the listener's namespace told (SIOCGSKNS), so its netstat missing is checked", d)
	}
	was := netstatPath
	t.Cleanup(func() { netstatPath = was })
	netstatPath = filepath.Join(t.TempDir(), "no-netstat")
	d, err = p.ListenDrops()
	if err != nil {
		t.Fatalf("the listener's own count is read whatever the namespace's: %v", err)
	}
	if d.NetKnown || d.Net != 0 || d.NetUnread == "" {
		t.Fatalf("the namespace's count, unreadable: %+v; want it unread, with why", d)
	}
	if told && !strings.Contains(d.NetUnread, "read the network namespace's TCP counters") {
		t.Fatalf("the namespace's count, its netstat missing: %+v; want that why", d)
	}
}

// holdsNetAdmin says whether this process holds CAP_NET_ADMIN in its
// effective set: with it, the kernel tells a socket's network namespace
// (SIOCGSKNS) in the process's own.
func holdsNetAdmin(t *testing.T) bool {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		t.Fatalf("read this process's capabilities: %v", err)
	}
	return data[unix.CAP_NET_ADMIN/32].Effective&(1<<(unix.CAP_NET_ADMIN%32)) != 0
}

// parseListenDrops reads TcpExtListenDrops out of /proc/net/netstat's text,
// by its name, never by a position a kernel may move; text without it, or
// whose values do not match the names, is no count.
func TestParseListenDropsReadsTheNamespacesCount(t *testing.T) {
	const netstat = "TcpExt: SyncookiesSent EmbryonicRsts ListenOverflows ListenDrops TCPBacklogDrop\n" +
		"TcpExt: 0 5 2 3 0\n" +
		"IpExt: InNoRoutes ListenDrops\n" +
		"IpExt: 0 99\n"
	if n, err := parseListenDrops([]byte(netstat)); err != nil || n != 3 {
		t.Fatalf("parseListenDrops = %d, %v; want 3", n, err)
	}
	for name, text := range map[string]string{
		"no ListenDrops":           "TcpExt: SyncookiesSent EmbryonicRsts\nTcpExt: 0 5\n",
		"no values":                "TcpExt: SyncookiesSent ListenDrops\n",
		"values that do not fit":   "TcpExt: SyncookiesSent ListenDrops\nTcpExt: 0\n",
		"a value that is no count": "TcpExt: SyncookiesSent ListenDrops\nTcpExt: 0 x\n",
		"no TcpExt":                "IpExt: InNoRoutes ListenDrops\nIpExt: 0 99\n",
	} {
		if n, err := parseListenDrops([]byte(text)); err == nil {
			t.Errorf("%s: parseListenDrops = %d, want an error", name, n)
		}
	}
}

// connectNoWait starts a connect to 127.0.0.1:port and does not wait for it:
// one whose SYN the kernel dropped is still retrying.
func connectNoWait(t *testing.T, port int) {
	t.Helper()
	c, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(c) })
	if err := unix.Connect(c, &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil && !errors.Is(err, unix.EINPROGRESS) {
		t.Fatal(err)
	}
}

// listenerDrops reads the kernel's drops at a listener: a backlog of 0 and
// connections nobody accepts overflow its accept queue, and each final ACK the
// kernel drops for it is counted there, and only there.
func TestListenerDropsCountsAFullAcceptQueue(t *testing.T) {
	full := listenBacklog0(t)
	other := listenBacklog0(t)
	port := func(fd int) int {
		sa, err := unix.Getsockname(fd)
		if err != nil {
			t.Fatal(err)
		}
		return sa.(*unix.SockaddrInet4).Port
	}
	drops := func(fd int) uint64 {
		t.Helper()
		n, err := listenerDrops(rawConnOf(fd))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := drops(full); n != 0 {
		t.Fatalf("a fresh listener's drops: %d, want 0", n)
	}
	for i := 0; i < 4; i++ {
		connectNoWait(t, port(full))
	}
	// The drops follow the connects' packets, processed as they are sent on
	// loopback; no deadline but the test binary's -timeout.
	for drops(full) == 0 {
		time.Sleep(time.Millisecond)
	}
	if n := drops(other); n != 0 {
		t.Fatalf("another listener's drops: %d, want 0: they are the full listener's alone", n)
	}
}

func listenBacklog0(t *testing.T) int {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	return fd
}

// rawConnOf is a syscall.RawConn over a bare fd.
type rawConnOf int

func (r rawConnOf) Control(f func(uintptr)) error  { f(uintptr(r)); return nil }
func (r rawConnOf) Read(func(uintptr) bool) error  { return errors.New("not used") }
func (r rawConnOf) Write(func(uintptr) bool) error { return errors.New("not used") }

// namedUpstream is an HTTP dependency whose every answer is name.
func namedUpstream(t *testing.T, name string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, name)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// callThrough makes one HTTP call through the proxy at addr and returns the
// body, "" when the proxy closed the connection instead.
func callThrough(t *testing.T, addr string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: dependency\r\nConnection: close\r\n\r\n"); err != nil {
		return ""
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil) // no deadline but -timeout
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// A connection the kernel paired with the application's is taken where its
// own socket says it is going, never where the by-port map says: a stale
// entry, or one another connection on the same port wrote, cannot misroute
// it.
func TestAPairedConnectionGoesWhereItsOwnSocketSays(t *testing.T) {
	right := namedUpstream(t, "right")
	wrong := namedUpstream(t, "wrong")
	f := &fakeCounter{dst: ipv4Dest(t, right)}
	cfg := config.New()
	cfg.ProxyPort = 0
	cfg.Agent.Mode = models.MODE_RECORD // with no session yet: a call passes through
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, wrong)}, cfg)
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.SetListenerCounter(f)
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan error, 1), make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	if got := callThrough(t, p.Listener.Addr().String()); got != "right" {
		t.Fatalf("the paired connection reached %q, want the destination its own socket carries (\"right\")", got)
	}
}

// A connection the kernel tied to no connection of the application's (a NAT
// between them, among the causes) is refused under the proven switch, a
// capture loss with a WARN: never sent where a by-port entry says.
func TestAnUnpairedConnectionIsRefusedAsACaptureLoss(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	wrong := namedUpstream(t, "wrong")
	f := &fakeCounter{} // pairs none
	cfg := config.New()
	cfg.ProxyPort = 0
	cfg.Agent.Mode = models.MODE_RECORD // with no session yet: a call passes through
	p := New(zap.NewNop(), fakeDestInfo{dest: ipv4Dest(t, wrong)}, cfg)
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.SetListenerCounter(f)
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan error, 1), make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	if got := callThrough(t, p.Listener.Addr().String()); got != "" {
		t.Fatalf("the unpaired connection reached %q, want it refused", got)
	}
	losses := p.CaptureLosses()
	if len(losses) != 1 || losses[0].Kind != models.CaptureLossUnpairedConn || losses[0].Count != 1 {
		t.Fatalf("CaptureLosses = %+v, want one unpaired connection", losses)
	}
}

// With the switch off, a connection the kernel tied to none of the
// application's (a NAT between them) is routed by its source port, as it
// always was, so the application's calls work: a proxy whose calls were
// counted from its start drops to not counted, for good, says why once, and
// its CountingLost says so (its agent then answers every later read unread);
// one that was not counted (no sock_diag, here) pairs nothing. Neither takes
// a capture loss: the call is routed.
func TestASettlingProxyRoutesANATdConnectionAsBeforeWithTheSwitchOff(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	for _, counted := range []bool{true, false} {
		wrong := namedUpstream(t, "wrong")
		f := &fakeCounter{} // pairs none
		cfg := config.New()
		cfg.ProxyPort = 0
		cfg.Agent.Mode = models.MODE_RECORD // with no session yet: a call passes through
		cfg.Agent.SettleReads = true
		core, logs := observer.New(zap.WarnLevel)
		p := New(zap.New(core), fakeDestInfo{dest: ipv4Dest(t, wrong)}, cfg)
		if err := p.InitIntegrations(context.Background()); err != nil {
			t.Fatal(err)
		}
		p.SetListenerCounter(f)
		if !counted {
			p.paired.fail(errors.New("no sock_diag here"))
		}
		ctx, cancel := context.WithCancel(context.Background())
		ready, done := make(chan error, 1), make(chan error, 1)
		go func() { done <- p.Serve(ctx, ready) }()
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
		if p.CountedFromStart() != counted || p.usesPairing() != counted || p.keepsConnections() != counted {
			t.Fatalf("counted %v: CountedFromStart %v, usesPairing %v, keepsConnections %v", counted, p.CountedFromStart(), p.usesPairing(), p.keepsConnections())
		}
		for i := 0; i < 2; i++ {
			if got := callThrough(t, p.Listener.Addr().String()); got != "wrong" {
				t.Fatalf("counted %v: the NAT'd connection reached %q, want it routed by its source port (\"wrong\")", counted, got)
			}
		}
		if losses := p.CaptureLosses(); len(losses) != 0 {
			t.Fatalf("counted %v: CaptureLosses %+v, want none: the calls were routed", counted, losses)
		}
		lostCh, lost := p.CountingLost()
		if counted {
			if lost == nil || !errors.Is(lost, models.ErrProxyConnsUnsupported) {
				t.Fatalf("counted: CountingLost %v, want why the run's calls stopped being counted", lost)
			}
			select {
			case <-lostCh:
			default:
				t.Fatal("counted: CountingLost's channel is not closed")
			}
			if p.usesPairing() || p.keepsConnections() {
				t.Fatal("counted: the proxy still pairs or keeps connections after the NAT'd one")
			}
			if n := logs.FilterMessageSnippet("NAT'd connection it cannot attribute").Len(); n != 1 {
				t.Fatalf("counted: %d WARNs of the NAT, want 1 for two NAT'd connections", n)
			}
		} else if n := logs.FilterMessageSnippet("NAT'd connection it cannot attribute").Len(); lost != nil || n != 0 {
			t.Fatalf("not counted: CountingLost %v, %d WARNs of a NAT; want neither", lost, n)
		}
		pairs := 0
		p.pairs.Range(func(any, any) bool { pairs++; return true })
		if pairs != 0 {
			t.Fatalf("counted %v: %d pairings left in the proxy's map, want none", counted, pairs)
		}
		cancel()
		<-done
	}
}

// NAT'd connections handled at once, with the switch off, all drop the count
// together: each is routed (dropCounting answers true for every one, none is
// refused), the count reads dropped and never "not counted from the start"
// meanwhile, the WARN is said once, and its channel closed once.
func TestNATdConnectionsHandledAtOnceAllDropTheCount(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	core, logs := observer.New(zap.WarnLevel)
	p := New(zap.New(core), fakeDestInfo{}, config.New())
	for i := 0; i < 20000; i++ {
		// A counted proxy again, its drop undone.
		p.countingLost.Store(nil)
		p.countingLostCh = make(chan struct{})
		p.countedFromStart.Store(true)
		_ = logs.TakeAll() // the previous round's WARN
		var wg sync.WaitGroup
		start := make(chan struct{})
		var refused, uncounted atomic.Int32
		for g := 0; g < 8; g++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				if !p.dropCounting(c) {
					refused.Add(1)
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				if !p.CountedFromStart() {
					uncounted.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if n := refused.Load(); n != 0 {
			t.Fatalf("round %d: %d of 8 NAT'd connections handled at once would be refused", i, n)
		}
		if n := uncounted.Load(); n != 0 {
			t.Fatalf("round %d: CountedFromStart read false %d times while the count was dropped", i, n)
		}
		if n := logs.FilterMessageSnippet("NAT'd connection it cannot attribute").Len(); n != 1 {
			t.Fatalf("round %d: %d WARNs, want 1", i, n)
		}
		ch, err := p.CountingLost()
		if err == nil {
			t.Fatalf("round %d: no reason after the drop", i)
		}
		select {
		case <-ch:
		default:
			t.Fatalf("round %d: the drop's channel is not closed", i)
		}
	}
}

// The marked listener has no TCP Fast Open, even one its socket was given
// before listen(): a Fast Open connection would never pair.
func TestTheMarkedListenerHasNoFastOpen(t *testing.T) {
	ln, markErr, err := ListenMarked(context.Background(), "127.0.0.1:0", func(rc syscall.RawConn) error {
		var serr error
		if err := rc.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_FASTOPEN, 16) }); err != nil {
			return err
		}
		return serr
	})
	if err != nil || markErr != nil {
		t.Fatalf("ListenMarked: %v, %v", err, markErr)
	}
	defer ln.Close()
	rc, err := ln.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var qlen int
	var gerr error
	if err := rc.Control(func(fd uintptr) { qlen, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_FASTOPEN) }); err != nil || gerr != nil {
		t.Fatalf("read TCP_FASTOPEN: %v %v", err, gerr)
	}
	if qlen != 0 {
		t.Fatalf("the marked listener's Fast Open queue is %d, want 0", qlen)
	}
}
