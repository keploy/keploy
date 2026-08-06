package recorder

import (
	"net"
	"testing"
)

// stubAddr is a net.Addr with an arbitrary string form, so the table below can
// exercise address shapes (IPv6, portless, unix path) without opening sockets.
type stubAddr struct{ s string }

func (a stubAddr) Network() string { return "tcp" }
func (a stubAddr) String() string  { return a.s }

// stubConn is a net.Conn that only answers RemoteAddr. Embedding net.Conn
// leaves every other method nil — any call would panic, which is the point:
// resolveDestServerName must touch nothing else.
type stubConn struct {
	net.Conn
	addr net.Addr
}

func (c stubConn) RemoteAddr() net.Addr { return c.addr }

// TestResolveDestServerName covers the ServerName decision for the MySQL
// destination TLS upgrade — the site with no fallback of any kind before this
// change, and the one most likely to hit the empty-ServerName hard error
// because MySQL clients dial by IP and RFC 6066 forbids IP literals in SNI.
//
// The verify=false rows are the byte-identical guarantee: with the flag off,
// the result must be exactly the captured SNI, empty string included, which is
// what the code did unconditionally before.
func TestResolveDestServerName(t *testing.T) {
	t.Parallel()

	tcpConn := stubConn{addr: stubAddr{"127.0.0.1:3306"}}
	v6Conn := stubConn{addr: stubAddr{"[::1]:3306"}}
	portless := stubConn{addr: stubAddr{"/var/run/mysqld/mysqld.sock"}}

	cases := []struct {
		name        string
		capturedSNI string
		destConn    net.Conn
		verify      bool
		want        string
	}{
		// --- verify OFF: exactly the old behaviour, no fallback at all ---
		{"off: captured SNI passes through", "db.example.com", tcpConn, false, "db.example.com"},
		{"off: empty SNI stays empty (no SNI on the wire)", "", tcpConn, false, ""},
		{"off: empty SNI stays empty even with no conn", "", nil, false, ""},

		// --- verify ON: the fallback that makes the flag usable ---
		{"on: captured SNI still wins", "db.example.com", tcpConn, true, "db.example.com"},
		// The keploy e2e shape: MYSQL_DSN=...@tcp(127.0.0.1:3306)/app.
		{"on: no SNI falls back to the peer IPv4", "", tcpConn, true, "127.0.0.1"},
		{"on: no SNI falls back to the peer IPv6", "", v6Conn, true, "::1"},
		{"on: portless address used verbatim", "", portless, true, "/var/run/mysqld/mysqld.sock"},

		// --- degenerate inputs must not panic on the hot path ---
		{"on: nil conn yields empty rather than panicking", "", nil, true, ""},
		{"on: nil RemoteAddr yields empty rather than panicking", "", stubConn{addr: nil}, true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := resolveDestServerName(tc.capturedSNI, tc.destConn, tc.verify)
			if got != tc.want {
				t.Fatalf("resolveDestServerName(%q, %v, %v) = %q; want %q",
					tc.capturedSNI, tc.destConn, tc.verify, got, tc.want)
			}
		})
	}
}

// TestResolveDestServerName_NeverEmptyWhenVerifyingARealConn is the invariant:
// given a real connection, a verifying MySQL dial always gets a ServerName.
// Empty would make crypto/tls reject the config with "either ServerName or
// InsecureSkipVerify must be specified" before any certificate is examined,
// which is precisely how record.upstreamTls.verify would have been unusable
// against every IP-dialled MySQL.
func TestResolveDestServerName_NeverEmptyWhenVerifyingARealConn(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if got := resolveDestServerName("", conn, true); got != "127.0.0.1" {
		t.Fatalf("resolveDestServerName(\"\", loopbackConn, true) = %q; want 127.0.0.1", got)
	}
	// And the default must not acquire an SNI it never had.
	if got := resolveDestServerName("", conn, false); got != "" {
		t.Fatalf("resolveDestServerName(\"\", loopbackConn, false) = %q; want empty — the default must stay byte-identical", got)
	}
}
