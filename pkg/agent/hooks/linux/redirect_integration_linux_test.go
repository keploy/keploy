//go:build linux && proxyconns_integration

// Root-only: it loads keploy's BPF object and attaches k_sockops. Behind its
// own tag, apart from pidmap_integration: it links the proxy, whose package
// initialisation opens sockets that a PID-map count would see.

package linux

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/models"
)

// redirectedTo is a TCP socket of the application's that keploy's connect
// hook redirected, away from dst (an orig_dst_by_cookie entry, as connect4
// leaves one), bound to port (0: any) in family v6 or IPv4, and a func that
// closes it once: the test's end closes it if nothing did.
func redirectedTo(t *testing.T, h *Hooks, v6 bool, port int, dst structs.DestInfo) (int, func()) {
	t.Helper()
	family := unix.AF_INET
	if v6 {
		family = unix.AF_INET6
	}
	fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	closeFD := func() { once.Do(func() { _ = unix.Close(fd) }) }
	t.Cleanup(closeFD)
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		t.Fatal(err)
	}
	if v6 {
		if err := unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 1); err != nil {
			t.Fatal(err)
		}
	}
	if port != 0 {
		var sa unix.Sockaddr = &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
		if v6 {
			sa = &unix.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}
		}
		if err := unix.Bind(fd, sa); err != nil {
			t.Fatal(err)
		}
	}
	cookie, err := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_COOKIE)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.objects.OrigDstByCookie.Update(cookie, dst, ebpf.UpdateAny); err != nil {
		t.Fatal(err)
	}
	return fd, closeFD
}

// connectTo connects fd to the loopback of its family at port and returns
// its local port.
func connectTo(t *testing.T, fd, port int) int {
	t.Helper()
	sa, _ := unix.Getsockname(fd)
	var to unix.Sockaddr = &unix.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	if _, v6 := sa.(*unix.SockaddrInet6); v6 {
		to = &unix.SockaddrInet6{Port: port, Addr: [16]byte{15: 1}}
	}
	if err := unix.Connect(fd, to); err != nil {
		t.Fatalf("connect to %d: %v", port, err)
	}
	sa, _ = unix.Getsockname(fd)
	switch a := sa.(type) {
	case *unix.SockaddrInet4:
		return a.Port
	case *unix.SockaddrInet6:
		return a.Port
	}
	return 0
}

// callOver makes one HTTP call on the connected socket fd and returns the
// body, "" when the connection was closed instead.
func callOver(t *testing.T, fd int) string {
	t.Helper()
	dup, err := unix.Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(dup), "app")
	c, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: dependency\r\nConnection: close\r\n\r\n"); err != nil {
		return ""
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func namedDependency(t *testing.T, name string) structs.DestInfo {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, name)
	}))
	t.Cleanup(srv.Close)
	ap := strings.TrimPrefix(srv.URL, "http://")
	_, p, _ := net.SplitHostPort(ap)
	var port uint32
	_, _ = fmt.Sscan(p, &port)
	return structs.DestInfo{IPVersion: 4, DestIP4: 0x7f000001, DestPort: port}
}

// The leak on keploy main, as a regression, through keploy's object, the
// proxy's real loop and its routing: 10,000 TCP DNS connections (connect4
// sends them to keploy's DNS server, not the proxy) leave no redirect behind,
// and the 50 calls after them all reach their dependency. Before, each DNS
// connection left an entry, and after 1024 of them the proxy could route
// almost no new connection.
func TestTCPDNSNeverStopsTheProxyRouting(t *testing.T) {
	h := NewHooks(zap.NewNop(), &config.Config{})
	loadProxyConnObject(t, h)
	cfg := config.New()
	cfg.ProxyPort = 0
	cfg.Agent.Mode = models.MODE_RECORD // no session yet: calls pass through
	p := proxy.New(zap.NewNop(), h, cfg)
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan error, 1), make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	proxyPort := p.Listener.Addr().(*net.TCPAddr).Port
	info := structs.AgentInfo{DNSPort: -1, IsDocker: 1}
	info.Proxy = structs.ProxyInfo{IP4: 0x7f000001, IP6: [4]uint32{0, 0, 0, 1}, Port: uint32(proxyPort)}
	if err := h.SendAgentInfo(info); err != nil {
		t.Fatal(err)
	}
	dns, err := net.Listen("tcp", "127.0.0.1:0") // keploy's DNS server, here
	if err != nil {
		t.Fatal(err)
	}
	defer dns.Close()
	go func() {
		for {
			c, err := dns.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	dnsPort := dns.Addr().(*net.TCPAddr).Port
	for i := 0; i < 10000; i++ {
		fd, closeFD := redirectedTo(t, h, false, 0, structs.DestInfo{IPVersion: 4, DestIP4: 0x7f000035, DestPort: 53})
		connectTo(t, fd, dnsPort)
		closeFD()
	}
	dep := namedDependency(t, "ok")
	answered := 0
	for i := 0; i < 50; i++ {
		fd, _ := redirectedTo(t, h, false, 0, dep)
		connectTo(t, fd, proxyPort)
		if callOver(t, fd) == "ok" {
			answered++
		}
	}
	if answered != 50 {
		t.Fatalf("after 10000 TCP DNS connections, %d of 50 calls reached their dependency, want 50", answered)
	}
}

// An IPv4 and an IPv6 connection on one local port, both live, each reach
// their own dependency: their redirects are two.
func TestTwoConnectionsOnOnePortReachTheirOwnDependencies(t *testing.T) {
	h := NewHooks(zap.NewNop(), &config.Config{})
	loadProxyConnObject(t, h)
	cfg := config.New()
	cfg.ProxyPort = 0
	cfg.Agent.Mode = models.MODE_RECORD
	p := proxy.New(zap.NewNop(), h, cfg)
	if err := p.InitIntegrations(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan error, 1), make(chan error, 1)
	go func() { done <- p.Serve(ctx, ready) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	proxyPort := p.Listener.Addr().(*net.TCPAddr).Port
	info := structs.AgentInfo{DNSPort: -1, IsDocker: 1}
	info.Proxy = structs.ProxyInfo{IP4: 0x7f000001, IP6: [4]uint32{0, 0, 0, 1}, Port: uint32(proxyPort)}
	if err := h.SendAgentInfo(info); err != nil {
		t.Fatal(err)
	}
	four := namedDependency(t, "four")
	six := namedDependency(t, "six")
	v4, _ := redirectedTo(t, h, false, 0, four)
	lport := connectTo(t, v4, proxyPort)
	v6, _ := redirectedTo(t, h, true, lport, six)
	if got := connectTo(t, v6, proxyPort); got != lport {
		t.Fatalf("the IPv6 connection's port %d, want %d", got, lport)
	}
	if got := callOver(t, v6); got != "six" {
		t.Fatalf("the IPv6 connection reached %q, want its own dependency (\"six\")", got)
	}
	if got := callOver(t, v4); got != "four" {
		t.Fatalf("the IPv4 connection reached %q, want its own dependency (\"four\")", got)
	}
}
