//go:build linux

package proxy

import (
	"context"
	"errors"
	"net"

	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
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

func (f *fakeCounter) ProxyConnSeq(syscall.RawConn) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seqErr != nil {
		return 0, f.seqErr
	}
	f.next++
	return f.next, nil
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
	p.canary(ln)
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
	if n, err := p.ListenDrops(); err != nil || n != 0 {
		t.Fatalf("a fresh marked listener's drops: %d, %v; want 0", n, err)
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
	if n, err := p.ListenDrops(); err != nil || n < raw {
		t.Fatalf("the marked listener's drops: %d, %v; want at least the kernel's %d", n, err, raw)
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
