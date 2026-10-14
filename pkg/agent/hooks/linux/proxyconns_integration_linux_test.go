//go:build linux && proxyconns_integration

// Root-only: it loads keploy's BPF object and attaches k_sockops. Behind its
// own tag, apart from pidmap_integration: it links the proxy and the agent,
// whose package initialisation opens sockets that the PID-map test's count of
// refused inserts would see.

package linux

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/acceptcount"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
)

// loadProxyConnObject loads keploy's object into h and attaches k_sockops at
// the root of the cgroup hierarchy, as hooks.load does.
func loadProxyConnObject(t *testing.T, h *Hooks) {
	t.Helper()
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	spec, err := loadBpf()
	if err != nil {
		t.Fatal(err)
	}
	if p := spec.Programs["k_sockops"]; p != nil {
		p.Type = ebpf.SockOps
		p.AttachType = ebpf.AttachCGroupSockOps
	}
	h.objectsMutex.Lock()
	err = spec.LoadAndAssign(&h.objects, nil)
	h.agentRegistrationMap = h.objects.KeployAgentRegistrationMap
	h.redirectProxyMap = h.objects.RedirectProxyMap
	h.objectsMutex.Unlock()
	if err != nil {
		t.Fatalf("load keploy's BPF object: %v", err)
	}
	t.Cleanup(func() { _ = h.objects.Close() })
	cg, err := agent.DetectCgroupPath(h.logger)
	if err != nil {
		t.Fatalf("no cgroup v2 path: %v", err)
	}
	l, err := link.AttachCgroup(link.CgroupOptions{Path: cg, Attach: ebpf.AttachCGroupSockOps, Program: h.objects.K_sockops})
	if err != nil {
		t.Fatalf("attach k_sockops to %s: %v", cg, err)
	}
	t.Cleanup(func() { _ = l.Close() })
}

// The kernel's count of the connections completed to the proxy's listener,
// opened as the proxy opens it (proxy.ListenMarked) with these hooks' mark,
// and the proxy's count of its accepts (acceptcount, which the proxy uses)
// agree: before a listener is marked there is no count; before anything is
// accepted the kernel already counts what waits in the accept queue; once all
// of it is accepted the two are equal; every address the listener takes
// counts; a connection to another listener is in neither; and an MPTCP
// listener is refused.
func TestProxyConnsMatchWhatTheProxyAccepts(t *testing.T) {
	h := NewHooks(zap.NewNop(), &config.Config{})
	loadProxyConnObject(t, h)
	ctx := context.Background()
	// Before a listener is marked the kernel counts nothing: no count, not 0.
	if n, err := h.ProxyConnsEstablished(ctx); !errors.Is(err, models.ErrProxyConnsUnsupported) {
		t.Fatalf("before the mark: ProxyConnsEstablished = %d, %v; want ErrProxyConnsUnsupported", n, err)
	}
	ln, markErr, err := proxy.ListenMarked(ctx, ":0", h.MarkProxyListener)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if markErr != nil {
		t.Fatalf("the hooks did not mark the proxy's listener: %v", markErr)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	established := func() uint64 {
		t.Helper()
		n, err := h.ProxyConnsEstablished(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := established(); got != 0 {
		t.Fatalf("a fresh object counts %d, want 0", got)
	}
	// waitEstablished waits for the kernel's count to reach want: a client's
	// connect returns when its side is established, the server's side a moment
	// later. No deadline but the test binary's -timeout.
	waitEstablished := func(want uint64, why string) {
		t.Helper()
		for {
			got := established()
			if got == want {
				return
			}
			if got > want {
				t.Fatalf("%s: established %d, want %d", why, got, want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	dial := func(addr string) {
		t.Helper()
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial %s: %v", addr, err)
		}
		t.Cleanup(func() { _ = c.Close() })
	}

	for i := 1; i <= 3; i++ {
		dial(fmt.Sprintf("127.0.0.1:%d", port))
		waitEstablished(uint64(i), "connections nothing accepted yet")
	}
	dial(fmt.Sprintf("[::1]:%d", port))
	waitEstablished(4, "a connection to [::1]")
	dial(fmt.Sprintf("127.0.0.2:%d", port))
	waitEstablished(5, "a connection to 127.0.0.2, which the proxy's listener takes too")

	// The proxy's side, accepting all five as its accept loop does.
	var accepts acceptcount.Counter
	for i := 0; i < 5; i++ {
		c, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		pairing, err := h.ProxyConnPair(mustRaw(t, c.(*net.TCPConn)))
		if err != nil {
			t.Fatal(err)
		}
		accepts.Add(pairing.Seq)
		_ = c.Close()
	}
	acc, _ := accepts.Accepted()
	if got := established(); got != 5 || acc != 5 {
		t.Fatalf("after accepting all five: established %d, accepted %d; want 5 and 5", got, acc)
	}

	// Another listener, not marked: the connection it accepts, which proves
	// the kernel's side of it done, is not counted. (The same address and
	// port in another network namespace is the ebpf repository's test.)
	other, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	dial(other.Addr().String())
	oc, err := other.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = oc.Close()
	if got := established(); got != 5 {
		t.Fatalf("a connection to an unmarked listener was counted: established %d, want 5", got)
	}

	// An MPTCP listener is refused: the kernel never copies its mark into its
	// connections' sockets, so its count would stay 0.
	var lc net.ListenConfig
	lc.SetMultipathTCP(true)
	mp, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mp.Close() })
	rc, err := mp.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var proto int
	var perr error
	if err := rc.Control(func(fd uintptr) { proto, perr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PROTOCOL) }); err != nil || perr != nil {
		t.Fatalf("read the listener's protocol: %v %v", err, perr)
	}
	err = h.MarkProxyListener(rc)
	switch {
	case proto != unix.IPPROTO_MPTCP:
		// Go fell back to TCP: this kernel has no MPTCP.
		t.Log("no MPTCP in this kernel: the refusal is not reachable here")
	case err == nil || !strings.Contains(err.Error(), "not plain TCP"):
		t.Fatalf("an MPTCP listener: MarkProxyListener = %v, want a not-plain-TCP refusal", err)
	}
}

func mustRaw(t *testing.T, c syscallConner) syscall.RawConn {
	t.Helper()
	rc, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

// The whole count, as the agent runs it: keploy's object, the proxy's real
// listener and accept loop (Serve) counted by these hooks, and the agent's
// ProxyConns and WaitProxyAccepted. Once ready, the proxy's own canary is
// number 1 and accepted; connections made then are numbered by the kernel,
// taken by the accept loop, and a wait on the count read covers exactly them.
func TestTheProxysRealLoopSettlesOnTheKernelsCount(t *testing.T) {
	h := NewHooks(zap.NewNop(), &config.Config{})
	loadProxyConnObject(t, h)
	cfg := config.New()
	cfg.ProxyPort = 0
	p := proxy.New(zap.NewNop(), h, cfg)
	p.SetListenerCounter(h)
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan error, 1), make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	a := agentsvc.New(zap.NewNop(), h, p, nil, nil, cfg)
	c, err := a.ProxyConns(ctx)
	if err != nil || c.Established != 1 || c.Accepted != 1 {
		t.Fatalf("once ready: ProxyConns = %+v, %v; want the canary, counted and accepted", c, err)
	}
	port := p.Listener.Addr().(*net.TCPAddr).Port
	for i := 0; i < 5; i++ {
		host := "127.0.0.1"
		if i%2 == 1 {
			host = "::1"
		}
		conn, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
	}
	// Each connect has returned; the kernel counts each connection at the
	// proxy side's handshake, a moment later. No deadline but -timeout.
	for {
		if c, err = a.ProxyConns(ctx); err != nil {
			t.Fatal(err)
		}
		if c.Established == 6 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := a.WaitProxyAccepted(ctx, c.Established); err != nil {
		t.Fatalf("WaitProxyAccepted(%d): %v", c.Established, err)
	}
	if c, err = a.ProxyConns(ctx); err != nil || c.Accepted != 6 || c.Established != 6 {
		t.Fatalf("after the wait: ProxyConns = %+v, %v; want 6 accepted of 6", c, err)
	}
}

// appSocket is a TCP socket of the application's that keploy redirected: an
// orig_dst_by_cookie entry, as connect4 leaves one.
func appSocket(t *testing.T, h *Hooks) int {
	t.Helper()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	cookie, err := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_COOKIE)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.objects.OrigDstByCookie.Update(cookie, make([]byte, 32), ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	return fd
}

// The application's own connections, through keploy's object, the proxy's
// real loop and the agent: one whose connect() has returned while the
// proxy's side of it waits for its data (TCP_DEFER_ACCEPT on the proxy's
// listener holds it) is counted, and WaitProxySettled waits for it, until its
// data completes the proxy's side and the proxy accepts it. One that resets
// before that is lost: a capture loss, and settled.
func TestTheApplicationsConnectionIsWaitedForBeforeTheProxySeesIt(t *testing.T) {
	h := NewHooks(zap.NewNop(), &config.Config{})
	loadProxyConnObject(t, h)
	cfg := config.New()
	cfg.ProxyPort = 0
	p := proxy.New(zap.NewNop(), h, cfg)
	p.SetListenerCounter(h)
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan error, 1), make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	port := p.Listener.Addr().(*net.TCPAddr).Port
	info := structs.AgentInfo{DNSPort: -1, IsDocker: 1}
	info.Proxy = structs.ProxyInfo{IP4: 0x7f000001, IP6: [4]uint32{0, 0, 0, 1}, Port: uint32(port)}
	if err := h.SendAgentInfo(info); err != nil {
		t.Fatal(err)
	}
	rc := mustRaw(t, mustTCPListenerConn(t, p.Listener))
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT, 30) }); err != nil || serr != nil {
		t.Fatalf("TCP_DEFER_ACCEPT: %v %v", err, serr)
	}
	a := agentsvc.New(zap.NewNop(), h, p, nil, nil, cfg)

	held := appSocket(t, h)
	if err := unix.Connect(held, &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	c, err := a.ProxyConns(ctx)
	if err != nil || c.AppConns != 1 || c.AppSettled != 0 || c.Established != 1 {
		t.Fatalf("with the connection's proxy side held: ProxyConns = %+v, %v; want 1 application connection, unsettled, and only the canary on the proxy's side", c, err)
	}
	settled := make(chan error, 1)
	go func() { settled <- a.WaitProxySettled(ctx, c) }()
	if _, err := unix.Write(held, []byte("x")); err != nil {
		t.Fatal(err)
	}
	// No deadline but the test binary's -timeout.
	if err := <-settled; err != nil {
		t.Fatalf("WaitProxySettled: %v", err)
	}
	if c, err = a.ProxyConns(ctx); err != nil || c.AppSettled != 1 || c.Accepted != 2 || c.Established != 2 {
		t.Fatalf("after the wait: ProxyConns = %+v, %v; want the application connection settled, and accepted with the canary", c, err)
	}

	gone := appSocket(t, h)
	if err := unix.Connect(gone, &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if c, err = a.ProxyConns(ctx); err != nil || c.AppConns != 2 {
		t.Fatalf("ProxyConns = %+v, %v; want a second application connection", c, err)
	}
	if err := unix.SetsockoptLinger(gone, unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 1, Linger: 0}); err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(gone)
	if err := a.WaitProxySettled(ctx, c); err != nil {
		t.Fatalf("WaitProxySettled across a connection closed before its pair: %v", err)
	}
	// It sent nothing: no call was made on it, so nothing was lost.
	if lost := h.CaptureLosses(); len(lost) != 0 {
		t.Fatalf("CaptureLosses = %+v, want none for a connection that sent nothing", lost)
	}
}

func mustTCPListenerConn(t *testing.T, ln net.Listener) syscallConner {
	t.Helper()
	return ln.(*net.TCPListener)
}

type syscallConner interface {
	SyscallConn() (syscall.RawConn, error)
}

// A NAT between the application and the proxy (here a DNAT of the proxy's
// address, 127.0.0.1 to 127.0.0.2, for its port) fails safe: the proxy's side of the connection shows
// other addresses than the application's, so the kernel ties it to nothing.
// The proxy refuses it (a capture loss), the application's connection stays
// unsettled while it lives, and is lost when it closes: never settled as if
// the proxy had it, so the test that made it is not verified.
func TestANATedConnectionIsNotVerified(t *testing.T) {
	iptables, err := exec.LookPath("iptables")
	if err != nil {
		t.Skipf("no iptables here: %v", err)
	}
	h := NewHooks(zap.NewNop(), &config.Config{})
	loadProxyConnObject(t, h)
	cfg := config.New()
	cfg.ProxyPort = 0
	p := proxy.New(zap.NewNop(), h, cfg)
	p.SetListenerCounter(h)
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan error, 1), make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	port := p.Listener.Addr().(*net.TCPAddr).Port
	info := structs.AgentInfo{DNSPort: -1, IsDocker: 1}
	info.Proxy = structs.ProxyInfo{IP4: 0x7f000001, IP6: [4]uint32{0, 0, 0, 1}, Port: uint32(port)}
	if err := h.SendAgentInfo(info); err != nil {
		t.Fatal(err)
	}
	rule := []string{"OUTPUT", "-p", "tcp", "-d", "127.0.0.1", "--dport", fmt.Sprint(port), "-j", "DNAT", "--to-destination", fmt.Sprintf("127.0.0.2:%d", port)}
	if out, err := exec.Command(iptables, append([]string{"-t", "nat", "-A"}, rule...)...).CombinedOutput(); err != nil {
		t.Skipf("cannot add a DNAT rule: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command(iptables, append([]string{"-t", "nat", "-D"}, rule...)...).Run() })
	a := agentsvc.New(zap.NewNop(), h, p, nil, nil, cfg)

	app := appSocket(t, h)
	if err := unix.Connect(app, &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	// The proxy accepts its side (numbered, the canary's after it) and,
	// with no pair, refuses it before the application sent anything: a FIN,
	// not a reset, so the application's connection is still alive
	// (CLOSE_WAIT).
	if n, err := unix.Read(app, make([]byte, 1)); n != 0 || err != nil {
		t.Fatalf("the application's read on the refused connection: %d, %v; want EOF", n, err)
	}
	c, err := a.ProxyConns(ctx)
	if err != nil || c.AppConns != 1 || c.AppSettled != 0 || c.Accepted != 2 {
		t.Fatalf("with the NATed connection alive: ProxyConns = %+v, %v; want 1 application connection, unsettled, and its proxy side accepted with the canary", c, err)
	}
	var unpaired bool
	for _, l := range p.CaptureLosses() {
		unpaired = unpaired || (l.Kind == models.CaptureLossUnpairedConn && l.Count == 1)
	}
	if !unpaired {
		t.Fatalf("the proxy's capture losses %+v: want the unpaired connection", p.CaptureLosses())
	}
	// The application sends its call on it and closes: its connection, which
	// sent its call, is lost, never settled as the proxy's.
	if _, err := unix.Write(app, []byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = unix.Close(app)
	if err := a.WaitProxySettled(ctx, c); err != nil {
		t.Fatalf("WaitProxySettled after the close: %v", err)
	}
	if lost := h.CaptureLosses(); len(lost) != 1 || lost[0].Kind != models.CaptureLossAppConnLost {
		t.Fatalf("the hooks' capture losses %+v: want the application connection lost", lost)
	}
}
