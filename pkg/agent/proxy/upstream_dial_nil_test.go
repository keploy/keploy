package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// closedTCPPort returns an address that is guaranteed to refuse connections:
// bind an ephemeral port, note it, then close the listener.
func closedTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// TestTLSDialReturnsTypedNil pins the stdlib hazard these tests exist for.
//
// tls.Dial's declared result is the CONCRETE *tls.Conn, so `dstConn, err =
// tls.Dial(...)` leaves dstConn a non-nil net.Conn interface holding a nil
// pointer when the dial fails. handleConnection declares `var dstConn
// net.Conn` and registers a deferred `if dstConn != nil { dstConn.Close() }`
// before dialing, so that guard passes and (*tls.Conn).Close dereferences a
// nil receiver — losing that connection's captured traffic while the run
// still reports success.
func TestTLSDialReturnsTypedNil(t *testing.T) {
	var raw net.Conn
	var err error
	raw, err = tls.Dial("tcp", closedTCPPort(t), &tls.Config{InsecureSkipVerify: true}) // nolint:gosec
	if err == nil {
		t.Fatalf("expected the dial to a closed port to fail")
	}
	if raw == nil {
		t.Skipf("tls.Dial no longer yields a typed nil; these guards may be obsolete")
	}
	if _, ok := raw.(*tls.Conn); !ok {
		t.Fatalf("expected the typed nil to be a *tls.Conn, got %T", raw)
	}
}

// TestDialDestinationTLSFailedDialIsNilInterface is the assertion that keeps
// the hazard above out of handleConnection.
//
// The upstream TLS dial sites go through util.DialDestinationTLS, which builds
// on tls.Dialer rather than tls.Dial. tls.Dialer.DialContext returns an
// interface and explicitly refuses to put a typed nil in it ("Don't return c
// (a typed nil) in an interface", crypto/tls/tls.go), and every error return
// in DialDestinationWith is either that value or an explicit nil.
//
// Nothing pins this today: the util package's own tests cover the loopback
// family fallback, not the nil shape. Switch either helper back to tls.Dial
// and the panic returns silently — this is what catches that.
func TestDialDestinationTLSFailedDialIsNilInterface(t *testing.T) {
	conn, err := util.DialDestinationTLS(context.Background(), zap.NewNop(), "tcp",
		util.DialTarget{Addr: closedTCPPort(t)}, &tls.Config{InsecureSkipVerify: true}) // nolint:gosec
	if err == nil {
		t.Fatalf("expected the dial to a closed port to fail")
	}
	if conn != nil {
		t.Fatalf("a failed dial must yield a nil net.Conn interface, got %T", conn)
	}
}

// TestUpstreamDialFailureDeferredCloseDoesNotPanic reproduces the panic in
// miniature: the same declare / defer-guarded-close / dial ordering
// handleConnection uses. With a typed nil this panicked on the deferred close,
// which util.Recover turned into "Recovered from panic in parser, closing
// active connections" — one connection's recording lost per occurrence.
func TestUpstreamDialFailureDeferredCloseDoesNotPanic(t *testing.T) {
	addr := closedTCPPort(t)

	dialErr := func() (err error) {
		var dstConn net.Conn
		defer func() {
			if dstConn != nil {
				_ = dstConn.Close()
			}
		}()
		dstConn, err = util.DialDestinationTLS(context.Background(), zap.NewNop(), "tcp",
			util.DialTarget{Addr: addr}, &tls.Config{InsecureSkipVerify: true}) // nolint:gosec
		if err != nil {
			return err
		}
		return nil
	}()

	if dialErr == nil {
		t.Fatalf("expected the dial to a closed port to fail")
	}
}

// TestDialDestinationTLSSuccess guards against the nil-safety above being
// bought by swallowing a good connection.
func TestDialDestinationTLSSuccess(t *testing.T) {
	ln, _ := newTLSTestServer(t, 0, []string{"http/1.1"}, nil)
	defer ln.Close()

	cfg := &tls.Config{
		InsecureSkipVerify: true, // nolint:gosec
		ServerName:         "test.local",
		NextProtos:         []string{"http/1.1"},
	}

	conn, err := util.DialDestinationTLS(context.Background(), zap.NewNop(), "tcp",
		util.DialTarget{Addr: ln.Addr().String()}, cfg)
	if err != nil {
		t.Fatalf("DialDestinationTLS: %v", err)
	}
	if conn == nil {
		t.Fatalf("expected a non-nil conn on success")
	}
	defer conn.Close()

	if _, ok := conn.(*tls.Conn); !ok {
		t.Fatalf("expected a *tls.Conn, got %T", conn)
	}
}
