//go:build linux && pidmap_integration

// Root-only, like pidmaps_integration_linux_test.go: it loads keploy's BPF
// object and attaches k_sockops, so it is behind the pidmap_integration tag.

package linux

import (
	"context"
	"errors"
	"fmt"
	"net"
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
		seq, err := h.ProxyConnSeq(mustRaw(t, c))
		if err != nil {
			t.Fatal(err)
		}
		accepts.Add(seq)
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

func mustRaw(t *testing.T, c net.Conn) syscall.RawConn {
	t.Helper()
	rc, err := c.(*net.TCPConn).SyscallConn()
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
