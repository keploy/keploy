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
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// callThroughHandler sends one HTTP/1 GET through handleConnection, for the
// app at the moved address addr; done closes when the handler returns.
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
	}()
	return client, ch
}

// An app whose bind keploy moved, but whose bind named another loopback
// address than the one the ingress dials (127.0.0.1): [::1] alone (Node 17
// and later often bind listen(port, "localhost") there), or another address
// of 127/8. The socket table says where it listens, and the call is
// forwarded there and answered, with nothing said above DEBUG. Without it
// the call was refused (main: an ERROR after 3 s), and with only a wait it
// would have waited for ever.
func TestACallToAnAppThatListensAtAnotherLoopbackAddressIsAnswered(t *testing.T) {
	for _, tc := range []struct{ name, network, host string }{
		{"the IPv6 loopback alone", "tcp6", "::1"},
		{"another address of 127/8", "tcp4", "127.0.0.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := testnet.SharedPort(t)
			ln, err := testnet.OpenShared(tc.network, net.JoinHostPort(tc.host, strconv.Itoa(port)))
			if err != nil {
				t.Skipf("cannot listen at %s here: %v", tc.host, err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			serveApp(t, ln, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "AT "+tc.host) })
			core, logs := observer.New(zapcore.DebugLevel)
			pm := New(zap.New(core), nil, &config.Config{})

			client, done := callThroughHandler(t, pm, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatalf("the call to an app at %s was not answered: %v; logged: %v", tc.host, err, atOrAbove(logs, zapcore.DebugLevel))
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) != "AT "+tc.host {
				t.Fatalf("answer %s %q, want the app's", resp.Status, body)
			}
			select {
			case <-done:
			case <-never(t):
				t.Fatal("the handler did not return after the call")
			}
			if e := atOrAbove(logs, zapcore.InfoLevel); len(e) != 0 {
				t.Errorf("logged at INFO or above: %v", e)
			}
		})
	}
}

// An app that listens on its moved port only at an interface's address (a
// pod's or container's IP), which the ingress does not dial: the call fails
// at once, with one ERROR that names where the app listens and what to
// change, and waits for nothing (its client stays, its context never ends).
// The app sees no connection.
func TestACallToAnAppThatListensOnlyAtANonLoopbackAddressFailsAtOnceNamingIt(t *testing.T) {
	ip := nonLoopbackIPv4(t)
	port := testnet.SharedPort(t)
	at := net.JoinHostPort(ip, strconv.Itoa(port))
	ln := testnet.ListenShared(t, "tcp4", at)
	core, logs := observer.New(zapcore.DebugLevel)
	pm := New(zap.New(core), nil, &config.Config{})
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	_, done := callThroughHandler(t, pm, addr)
	select {
	case <-done:
	case <-never(t):
		t.Fatal("the call still waits for an app that listens, only elsewhere")
	}
	errs := atOrAbove(logs, zapcore.ErrorLevel)
	if len(errs) != 1 {
		t.Fatalf("want one ERROR, got %v (all: %v)", errs, logs.All())
	}
	msg, _ := errs[0].ContextMap()["error"].(string)
	for _, want := range []string{at, "0.0.0.0", "127.0.0.1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the ERROR's error %q does not name %s", msg, want)
		}
	}
	if n := accepted(t, ln); n != 0 {
		t.Errorf("the app saw %d connections", n)
	}

	r := result(t, dialAsync(context.Background(), pm, addr, nil))
	var elsewhere *appListensElsewhereError
	if !errors.As(r.err, &elsewhere) || len(elsewhere.at) != 1 || elsewhere.at[0].String() != at {
		t.Fatalf("dialApp: %v, want the app listening only at %s", r.err, at)
	}
	if _, waits := waitersAt(pm, addr); waits != 0 {
		t.Errorf("%d waits left", waits)
	}
}

// Calls that wait for an app that has not listened yet are answered when it
// listens at another loopback address: the poll looks at the socket table on
// each look, not only at the first refusal. Its connection is one call's and
// the other dials its own there, so the app sees one connection a call.
// When the app listens only at an interface's address instead, the wait
// ends with the error that names it.
func TestAWaitEndsWhereTheAppListens(t *testing.T) {
	t.Run("another loopback address", func(t *testing.T) {
		port := testnet.SharedPort(t)
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		pm := New(zap.NewNop(), nil, &config.Config{})
		calls := []<-chan dialed{dialAsync(context.Background(), pm, addr, nil), dialAsync(context.Background(), pm, addr, nil)}
		awaitWaiters(t, pm, addr, 2)
		ln := testnet.ListenShared(t, "tcp4", net.JoinHostPort("127.0.0.2", strconv.Itoa(port)))
		for _, ch := range calls {
			r := result(t, ch)
			if r.err != nil {
				t.Fatalf("a waiting call: %v", r.err)
			}
			if got := r.conn.RemoteAddr().(*net.TCPAddr).IP.String(); got != "127.0.0.2" {
				t.Errorf("a waiting call reached %s, want 127.0.0.2", got)
			}
			defer func() { _ = r.conn.Close() }()
		}
		if n := accepted(t, ln); n != 2 {
			t.Errorf("the app saw %d connections for 2 calls", n)
		}
	})
	t.Run("only an interface's address", func(t *testing.T) {
		ip := nonLoopbackIPv4(t)
		port := testnet.SharedPort(t)
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		pm := New(zap.NewNop(), nil, &config.Config{})
		ch := dialAsync(context.Background(), pm, addr, nil)
		awaitWaiters(t, pm, addr, 1)
		ln := testnet.ListenShared(t, "tcp4", net.JoinHostPort(ip, strconv.Itoa(port)))
		var elsewhere *appListensElsewhereError
		if r := result(t, ch); !errors.As(r.err, &elsewhere) {
			t.Fatalf("the waiting call: %v, want the app listening only at %s", r.err, ip)
		}
		if n := accepted(t, ln); n != 0 {
			t.Errorf("the app saw %d connections", n)
		}
	})
}

// listenerFor's choice, by the addresses the table has listeners at.
func TestListenerForPicksTheLoopbackAddressTheAppListensAt(t *testing.T) {
	dialed := netip.MustParseAddrPort("127.0.0.1:41119")
	a := netip.MustParseAddr
	for _, tc := range []struct {
		name      string
		listeners []netip.Addr
		target    string
		elsewhere string
	}{
		{"nothing listens", nil, "", ""},
		{"0.0.0.0 takes the address dialed", []netip.Addr{a("0.0.0.0")}, "127.0.0.1:41119", ""},
		{"the address dialed, v4-mapped", []netip.Addr{a("::ffff:127.0.0.1")}, "127.0.0.1:41119", ""},
		{"[::1] alone", []netip.Addr{a("::1")}, "[::1]:41119", ""},
		{":: (IPV6_V6ONLY) is dialed at [::1]", []netip.Addr{a("::")}, "[::1]:41119", ""},
		{"127/8 before [::1]", []netip.Addr{a("::1"), a("127.0.0.3"), a("127.0.0.2")}, "127.0.0.2:41119", ""},
		{"the address dialed before the rest", []netip.Addr{a("127.0.0.2"), a("0.0.0.0")}, "127.0.0.1:41119", ""},
		{"a loopback address before an interface's", []netip.Addr{a("10.1.2.3"), a("::1")}, "[::1]:41119", ""},
		{"only interfaces' addresses", []netip.Addr{a("fd00::5"), a("10.1.2.3"), a("10.1.2.3")}, "", "10.1.2.3:41119 [fd00::5]:41119"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, ok, elsewhere := listenerFor(dialed, tc.listeners)
			got := ""
			if ok {
				got = target.String()
			}
			if got != tc.target {
				t.Errorf("target %q, want %q", got, tc.target)
			}
			var es []string
			for _, e := range elsewhere {
				es = append(es, e.String())
			}
			if strings.Join(es, " ") != tc.elsewhere {
				t.Errorf("elsewhere %v, want %q", es, tc.elsewhere)
			}
		})
	}
}
