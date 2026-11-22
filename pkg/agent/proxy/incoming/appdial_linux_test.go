//go:build linux

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
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// nonLoopbackIPv4 is an IPv4 address of one of this host's interfaces that
// is not the loopback's.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("no interface addresses: %v", err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	t.Skip("this host has no IPv4 address but the loopback's")
	return ""
}

// linkLocalIPv6 is an IPv6 link-local address of one of this host's
// interfaces, with its zone.
func linkLocalIPv6(t *testing.T) string {
	t.Helper()
	ifcs, err := net.Interfaces()
	if err != nil {
		t.Skipf("no interfaces: %v", err)
	}
	for _, ifc := range ifcs {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() == nil && n.IP.IsLinkLocalUnicast() {
				return n.IP.String() + "%" + ifc.Name
			}
		}
	}
	t.Skip("this host has no IPv6 link-local address")
	return ""
}

// callThroughHandler sends one HTTP/1 GET through handleConnection, for the
// app at the moved address addr; done closes when the handler returns and
// every capture it started is over (it starts them before it returns).
func callThroughHandler(t *testing.T, pm *IngressProxyManager, addr string) (client net.Conn, done <-chan struct{}) {
	t.Helper()
	client, proxied := tcpPairForWatch(t)
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Write([]byte("GET /where HTTP/1.1\r\nHost: api\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		pm.handleConnection(context.Background(), proxied, addr, pm.logger, make(chan *models.TestCase, 8), make(chan struct{}, 1), 8080)
		pm.captures.Wait()
	}()
	return client, ch
}

// An app whose bind keploy moved, but whose bind named another address of
// its network namespace than the one the ingress dials (127.0.0.1): [::1]
// alone (Node 17 and later often bind listen(port, "localhost") there),
// another address of 127/8, or an interface's (a pod's or container's IP).
// The ingress shares the namespace, so each is local to it: the socket table
// says where the app listens, and the call is forwarded there and answered,
// with nothing said above DEBUG. Without it the call was refused (main: an
// ERROR after 3 s), and with only a wait it would have waited for ever.
func TestACallToAnAppThatListensAtAnotherAddressOfItsNamespaceIsAnswered(t *testing.T) {
	for _, tc := range []struct {
		name, network string
		host          func(*testing.T) string
	}{
		{"the IPv6 loopback alone", "tcp6", func(*testing.T) string { return "::1" }},
		{"another address of 127/8", "tcp4", func(*testing.T) string { return "127.0.0.2" }},
		{"an interface's address", "tcp4", nonLoopbackIPv4},
		{"an interface's IPv6 link-local address, on its device", "tcp6", linkLocalIPv6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := tc.host(t)
			port := testnet.SharedPort(t)
			ln, err := testnet.OpenShared(tc.network, net.JoinHostPort(host, strconv.Itoa(port)))
			if err != nil {
				t.Skipf("cannot listen at %s here: %v", host, err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			serveApp(t, ln, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "AT "+host) })
			core, logs := observer.New(zapcore.DebugLevel)
			pm := New(zap.New(core), nil, &config.Config{})

			client, done := callThroughHandler(t, pm, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatalf("the call to an app at %s was not answered: %v; logged: %v", host, err, atOrAbove(logs, zapcore.DebugLevel))
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "AT "+host {
				t.Fatalf("answer %s %q, want the app's", resp.Status, body)
			}
			<-done
			if e := atOrAbove(logs, zapcore.InfoLevel); len(e) != 0 {
				t.Errorf("logged at INFO or above: %v", e)
			}
		})
	}
}

// listenFreebind opens a listener at host:port, a port SharedPort holds,
// with IP_FREEBIND: at an address the namespace need not have.
func listenFreebind(t *testing.T, host string, port int) net.Listener {
	t.Helper()
	ln, err := openFreebind(host, port)
	if err != nil {
		t.Skipf("cannot listen at %s with IP_FREEBIND here: %v", host, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// openFreebind is listenFreebind that returns its error, for a goroutine the
// test starts; the caller closes the listener.
func openFreebind(host string, port int) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); serr == nil {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_FREEBIND, 1)
			}
		}); err != nil {
			return err
		}
		return serr
	}}
	return lc.Listen(context.Background(), "tcp4", net.JoinHostPort(host, strconv.Itoa(port)))
}

// notLocal is an address of TEST-NET-1 that this namespace does not have
// (the test is skipped where it does).
func notLocal(t *testing.T) string {
	t.Helper()
	const ip = "192.0.2.77"
	if ok, err := util.IsLocalAddr(netip.MustParseAddr(ip)); err != nil || ok {
		t.Skipf("%s is this namespace's own here, or its routes cannot be read (%v)", ip, err)
	}
	return ip
}

// An app that listens on its moved port only where no connection reaches it:
// at an address its namespace does not have (bound with IP_FREEBIND), which
// a connection would leave the host for. The call fails at once, with one
// ERROR that names the address and why no connection reaches it (and tells
// no one to change the app), and waits for nothing (its client stays, its
// context never ends).
func TestACallToAnAppThatListensOnlyWhereNoConnectionReachesFailsAtOnceNamingIt(t *testing.T) {
	ip := notLocal(t)
	port := testnet.SharedPort(t)
	at := net.JoinHostPort(ip, strconv.Itoa(port))
	_ = listenFreebind(t, ip, port)
	core, logs := observer.New(zapcore.DebugLevel)
	pm := New(zap.New(core), nil, &config.Config{})
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	_, done := callThroughHandler(t, pm, addr)
	<-done
	errs := atOrAbove(logs, zapcore.ErrorLevel)
	if len(errs) != 1 {
		t.Fatalf("want one ERROR, got %v (all: %v)", errs, logs.All())
	}
	msg, _ := errs[0].ContextMap()["error"].(string)
	for _, want := range []string{at, "IP_FREEBIND", "routed off the host"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the ERROR's error %q does not say %s", msg, want)
		}
	}
	if strings.Contains(strings.ToLower(msg), "make the application") {
		t.Errorf("the ERROR's error %q tells the user to change the app", msg)
	}

	r := result(t, dialAsync(context.Background(), pm, addr, nil))
	var unreachable *appUnreachableError
	if !errors.As(r.err, &unreachable) || len(unreachable.at) != 1 || unreachable.at[0].at.String() != at || unreachable.at[0].why != whyNotLocal {
		t.Fatalf("dialApp: %v, want the app listening only at %s, which is not local", r.err, at)
	}
	if _, waits := waitersAt(pm, addr); waits != 0 {
		t.Errorf("%d waits left", waits)
	}
}

// Calls that wait for an app that has not listened yet are answered when it
// listens at another address of its namespace (another loopback address, or
// an interface's): the poll looks at the socket table on each look, not only
// at the first refusal. Its connection is one call's and the other dials its
// own there, so the app sees one connection a call. When the app listens
// only where no connection reaches instead, the wait ends with the error
// that names it.
func TestAWaitEndsWhereTheAppListens(t *testing.T) {
	for _, tc := range []struct {
		name string
		host func(*testing.T) string
	}{
		{"another loopback address", func(*testing.T) string { return "127.0.0.2" }},
		{"an interface's address", nonLoopbackIPv4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := tc.host(t)
			port := testnet.SharedPort(t)
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			pm := New(zap.NewNop(), nil, &config.Config{})
			calls := []<-chan dialed{dialAsync(context.Background(), pm, addr, nil), dialAsync(context.Background(), pm, addr, nil)}
			awaitWaiters(t, pm, addr, 2)
			ln := testnet.ListenShared(t, "tcp4", net.JoinHostPort(host, strconv.Itoa(port)))
			for _, ch := range calls {
				r := result(t, ch)
				if r.err != nil {
					t.Fatalf("a waiting call: %v", r.err)
				}
				if got := r.conn.RemoteAddr().(*net.TCPAddr).IP.String(); got != host {
					t.Errorf("a waiting call reached %s, want %s", got, host)
				}
				defer func() { _ = r.conn.Close() }()
			}
			if n := accepted(t, ln); n != 2 {
				t.Errorf("the app saw %d connections for 2 calls", n)
			}
		})
	}
	t.Run("only where no connection reaches", func(t *testing.T) {
		ip := notLocal(t)
		port := testnet.SharedPort(t)
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		pm := New(zap.NewNop(), nil, &config.Config{})
		ch := dialAsync(context.Background(), pm, addr, nil)
		awaitWaiters(t, pm, addr, 1)
		_ = listenFreebind(t, ip, port)
		var unreachable *appUnreachableError
		if r := result(t, ch); !errors.As(r.err, &unreachable) {
			t.Fatalf("the waiting call: %v, want the app listening only at %s", r.err, ip)
		}
	})
}

// The address the table found is remembered for the address dialed and
// dialed first: a call after the first goes there even once a listener takes
// the address dialed too, with no wait. A refusal there forgets it, and the
// call is dialed from the address dialed again.
func TestTheAddressTheAppWasFoundAtIsDialedFirstUntilItRefuses(t *testing.T) {
	port := testnet.SharedPort(t)
	v6, err := testnet.OpenShared("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
	if err != nil {
		t.Skipf("no [::1] here: %v", err)
	}
	pm := New(zap.NewNop(), nil, &config.Config{})
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	reached := func() string {
		t.Helper()
		r := result(t, dialAsync(context.Background(), pm, addr, nil))
		if r.err != nil {
			t.Fatalf("dialApp: %v", r.err)
		}
		defer func() { _ = r.conn.Close() }()
		return r.conn.RemoteAddr().(*net.TCPAddr).IP.String()
	}
	if got := reached(); got != "::1" {
		t.Fatalf("the first call reached %s, want ::1", got)
	}
	_ = testnet.ListenShared(t, "tcp4", addr)
	if got := reached(); got != "::1" {
		t.Fatalf("a later call reached %s, want ::1, where the app was found", got)
	}
	_ = v6.Close()
	if got := reached(); got != "127.0.0.1" {
		t.Fatalf("after [::1] refused, a call reached %s, want 127.0.0.1, the address dialed", got)
	}
	pm.waitsMu.Lock()
	at, ok := pm.listensAt[addr]
	pm.waitsMu.Unlock()
	if ok {
		t.Errorf("after [::1] refused, %s is still remembered at %s", addr, at)
	}
	if _, waits := waitersAt(pm, addr); waits != 0 {
		t.Errorf("%d waits left", waits)
	}
}

// An address the app was found at that the namespace no longer has (an
// interface's address that went) is not dialed: the dial would leave the
// host. It is forgotten, and the call is dialed at the address dialed.
func TestARememberedAddressTheNamespaceNoLongerHasIsNotDialed(t *testing.T) {
	ip := notLocal(t)
	port := testnet.SharedPort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	_ = testnet.ListenShared(t, "tcp4", addr)
	pm := New(zap.NewNop(), nil, &config.Config{})
	pm.rememberListener(addr, net.JoinHostPort(ip, strconv.Itoa(port)))
	r := result(t, dialAsync(context.Background(), pm, addr, nil))
	if r.err != nil {
		t.Fatalf("dialApp: %v", r.err)
	}
	defer func() { _ = r.conn.Close() }()
	if got := r.conn.RemoteAddr().(*net.TCPAddr).IP.String(); got != "127.0.0.1" {
		t.Errorf("the call reached %s, want 127.0.0.1", got)
	}
	pm.waitsMu.Lock()
	at, ok := pm.listensAt[addr]
	pm.waitsMu.Unlock()
	if ok {
		t.Errorf("%s is still remembered at %s, which the namespace does not have", addr, at)
	}
}

// While the recording is stopping, a refusal is final, but the socket table
// is read once first: an app that still listens at another address of its
// namespace (on [::1] alone, or at an interface's address) serves until it
// is stopped, as one on 127.0.0.1 does, and its call is answered. With
// nothing listening, the refusal is returned at once.
func TestWhileStoppingACallToAnAppThatStillListensElsewhereIsAnswered(t *testing.T) {
	for _, tc := range []struct {
		name, network string
		host          func(*testing.T) string
	}{
		{"the IPv6 loopback alone", "tcp6", func(*testing.T) string { return "::1" }},
		{"an interface's address", "tcp4", nonLoopbackIPv4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := tc.host(t)
			port := testnet.SharedPort(t)
			ln, err := testnet.OpenShared(tc.network, net.JoinHostPort(host, strconv.Itoa(port)))
			if err != nil {
				t.Skipf("cannot listen at %s here: %v", host, err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			core, logs := observer.New(zapcore.DebugLevel)
			pm := New(zap.New(core), nil, &config.Config{})
			pm.SetStopping(true)
			r := result(t, dialAsync(context.Background(), pm, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil))
			if r.err != nil {
				t.Fatalf("stopping: the call to an app that still listens at %s: %v", host, r.err)
			}
			_ = r.conn.Close()
			if got := r.conn.RemoteAddr().(*net.TCPAddr).IP.String(); got != host {
				t.Errorf("the call reached %s, want %s", got, host)
			}
			if e := atOrAbove(logs, zapcore.InfoLevel); len(e) != 0 {
				t.Errorf("logged at INFO or above: %v", e)
			}
		})
	}
	t.Run("nothing listens", func(t *testing.T) {
		pm := New(zap.NewNop(), nil, &config.Config{})
		pm.SetStopping(true)
		addr := sharedAppAddr(t)
		r := result(t, dialAsync(context.Background(), pm, addr, nil))
		if r.err == nil || !isConnRefused(r.err) {
			t.Fatalf("stopping, nothing listening: %v, want the refusal", r.err)
		}
		if _, waits := waitersAt(pm, addr); waits != 0 {
			t.Errorf("%d waits left", waits)
		}
	})
}

// listenerFor's choice, by the addresses the table has listeners at, with
// 10.1.2.3, fd00::6 and fe80::1%2 the namespace's own.
func TestListenerForPicksTheAddressTheAppListensAt(t *testing.T) {
	dialed := netip.MustParseAddrPort("127.0.0.1:41119")
	a := netip.MustParseAddr
	own := map[netip.Addr]bool{a("10.1.2.3"): true, a("fd00::6"): true, a("fe80::1%2"): true}
	local := func(l netip.Addr) bool { return own[l] }
	for _, tc := range []struct {
		name        string
		listeners   []netip.Addr
		target      string
		unreachable string
	}{
		{"nothing listens", nil, "", ""},
		{"0.0.0.0 takes the address dialed", []netip.Addr{a("0.0.0.0")}, "127.0.0.1:41119", ""},
		{"the address dialed, v4-mapped", []netip.Addr{a("::ffff:127.0.0.1")}, "127.0.0.1:41119", ""},
		{"[::1] alone", []netip.Addr{a("::1")}, "[::1]:41119", ""},
		{":: (IPV6_V6ONLY) is dialed at [::1]", []netip.Addr{a("::")}, "[::1]:41119", ""},
		{"127/8 before [::1]", []netip.Addr{a("::1"), a("127.0.0.3"), a("127.0.0.2")}, "127.0.0.2:41119", ""},
		{"the address dialed before the rest", []netip.Addr{a("127.0.0.2"), a("0.0.0.0")}, "127.0.0.1:41119", ""},
		{"a loopback address before an interface's", []netip.Addr{a("10.1.2.3"), a("::1")}, "[::1]:41119", ""},
		{"an interface's address", []netip.Addr{a("10.1.2.3")}, "10.1.2.3:41119", ""},
		{"an IPv4 interface address before an IPv6 one", []netip.Addr{a("fd00::6"), a("10.1.2.3")}, "10.1.2.3:41119", ""},
		{"a link-local address on its device", []netip.Addr{a("fe80::1%2")}, "[fe80::1%2]:41119", ""},
		{"an address the namespace has before one it lacks", []netip.Addr{a("192.0.2.1"), a("fd00::6")}, "[fd00::6]:41119", ""},
		{"only where no connection reaches", []netip.Addr{a("fd00::5"), a("192.0.2.1"), a("192.0.2.1"), a("fe80::9")},
			"", "192.0.2.1:41119 not-local, [fd00::5]:41119 not-local, [fe80::9]:41119 no-device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, ok, unreachable := listenerFor(dialed, tc.listeners, local)
			got := ""
			if ok {
				got = target.String()
			}
			if got != tc.target {
				t.Errorf("target %q, want %q", got, tc.target)
			}
			var us []string
			for _, u := range unreachable {
				why := map[string]string{whyNotLocal: "not-local", whyNoDevice: "no-device"}[u.why]
				us = append(us, u.at.String()+" "+why)
			}
			if strings.Join(us, ", ") != tc.unreachable {
				t.Errorf("unreachable %v, want %q", us, tc.unreachable)
			}
		})
	}
}

// keepAliveApp answers one request on its first connection and keeps it;
// then it closes that connection and its listener, as an app that restarts,
// listens again where relisten does (nil: not at all), and closes closed.
func keepAliveApp(t *testing.T, ln net.Listener, relisten func()) (closed <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		br := bufio.NewReader(c)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Type: text/plain\r\n\r\nFIRST")
		time.Sleep(150 * time.Millisecond)
		_ = ln.Close()
		_ = c.Close()
		if relisten != nil {
			relisten()
		}
		close(ch)
	}()
	return ch
}

// secondCallOverAStalePool sends a first call through serveWatched's handler
// (keploy mock) to an app that answers it and then restarts (keepAliveApp),
// and a second call once the pooled connection to it has gone stale.
func secondCallOverAStalePool(t *testing.T, addr string, relisten func()) (pm *IngressProxyManager, client net.Conn, br *bufio.Reader, served <-chan struct{}, logs *observer.ObservedLogs) {
	t.Helper()
	first := testnet.ListenShared(t, "tcp4", addr)
	closed := keepAliveApp(t, first, relisten)
	pm, client, served, logs = serveWatched(t, false, first, nil)
	if _, err := client.Write([]byte("GET /one HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second))
	br = bufio.NewReader(client)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("the first call: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	<-closed
	// The app's FIN reaches the pooled connection, so the second call takes
	// the stale-pool redial (a write that fails first redials as well).
	time.Sleep(300 * time.Millisecond)
	if _, err := client.Write([]byte("GET /two HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	return pm, client, br, served, logs
}

// keploy mock: when the redial of a stale pooled connection finds the app
// back only where no connection reaches it (an address its namespace does
// not have), that is an ERROR that names the address, not keploy mock's
// DEBUG for a redial that ended.
func TestKeployMockLogsAnErrorWhenAStaleRedialFindsTheAppOnlyWhereNoConnectionReaches(t *testing.T) {
	ip := notLocal(t)
	_ = listenFreebind(t, ip, testnet.SharedPort(t)) // skips here where IP_FREEBIND is refused
	addr := sharedAppAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	pm, client, br, served, logs := secondCallOverAStalePool(t, addr, func() {
		if l, err := openFreebind(ip, p); err == nil {
			t.Cleanup(func() { _ = l.Close() })
		}
	})
	_, _ = http.ReadResponse(br, nil)
	_ = client.Close()
	<-served
	pm.captures.Wait()
	found := false
	for _, e := range atOrAbove(logs, zapcore.ErrorLevel) {
		if msg, _ := e.ContextMap()["error"].(string); strings.Contains(msg, net.JoinHostPort(ip, port)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no ERROR names %s; logged %v", net.JoinHostPort(ip, port), logs.All())
	}
}

// keploy mock: the redial of a stale pooled connection to an app that went
// and has not listened again waits for it, and a client that leaves during
// that wait ends the handler (the redial watches the call's client).
func TestAClientThatLeavesWhileARedialWaitsEndsTheHandler(t *testing.T) {
	addr := sharedAppAddr(t)
	pm, client, _, served, _ := secondCallOverAStalePool(t, addr, nil)
	awaitWaiters(t, pm, addr, 1)
	_ = client.Close()
	<-served
	pm.captures.Wait()
	if _, waits := waitersAt(pm, addr); waits != 0 {
		t.Errorf("%d waits left", waits)
	}
}

// A call given an address no connection to stays on the host is not dialed:
// an address the namespace does not have (an interface's that went: the
// redial of a connection first made there), one whose device the kernel
// cannot be asked about, and a name, which cannot be asked about at all.
// Each fails at once with the reason, and nothing is sent off the host.
func TestACallGivenAnAddressNoConnectionToStaysOnTheHostIsNotDialed(t *testing.T) {
	ip := notLocal(t)
	port := strconv.Itoa(testnet.SharedPort(t))
	for _, c := range []struct{ addr, why string }{
		{net.JoinHostPort(ip, port), whyNotOwn},
		{net.JoinHostPort("fe80::1%keploy-no-such-dev", port), whyNotAsked},
		{net.JoinHostPort("localhost", port), whyNotAnIP},
	} {
		pm := New(zap.NewNop(), nil, &config.Config{})
		r := result(t, dialAsync(context.Background(), pm, c.addr, nil))
		if r.conn != nil {
			_ = r.conn.Close()
		}
		var unreachable *appUnreachableError
		if !errors.As(r.err, &unreachable) || unreachable.dialed != c.addr || len(unreachable.at) != 0 || !strings.HasPrefix(unreachable.why, c.why) {
			t.Errorf("dialApp(%s) = %v, want it not dialed because %s", c.addr, r.err, c.why)
		}
	}
}

// elsewhereConn is a connection whose far end reads as at.
type elsewhereConn struct {
	net.Conn
	at net.Addr
}

func (c elsewhereConn) RemoteAddr() net.Addr { return c.at }

// The HTTP/1 ingress redials the address its first dial reached. When that
// was an interface's address the namespace no longer has, the redial is not
// made (it would leave the host): the call fails at once, with an ERROR that
// says why.
func TestAnHTTP1RedialToAnAddressTheNamespaceNoLongerHasIsNotMade(t *testing.T) {
	ip := notLocal(t)
	ln := testnet.ListenShared(t, "tcp4", sharedAppAddr(t))
	up, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = up.Close() // the request's write fails, so the call is redialed
	port := ln.Addr().(*net.TCPAddr).Port
	gone := net.JoinHostPort(ip, strconv.Itoa(port))
	upConn := elsewhereConn{Conn: up, at: &net.TCPAddr{IP: net.ParseIP(ip), Port: port}}
	core, logs := observer.New(zapcore.InfoLevel)
	pm := New(zap.New(core), nil, &config.Config{})
	client, proxied := tcpPairForWatch(t)
	served := make(chan struct{})
	go func() {
		defer close(served)
		defer func() { _ = proxied.Close() }()
		pm.handleHttp1ZeroCopy(context.Background(), proxied, upConn, pm.logger, nil, 8080, false, nil)
	}()
	if _, err := client.Write([]byte("GET /where HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	<-served
	pm.captures.Wait()
	found := false
	for _, e := range atOrAbove(logs, zapcore.ErrorLevel) {
		if msg, _ := e.ContextMap()["error"].(string); strings.Contains(msg, gone) && strings.Contains(msg, whyNotOwn) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no ERROR says the redial to %s was not made; logged %v", gone, logs.All())
	}
}
