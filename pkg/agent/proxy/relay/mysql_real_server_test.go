package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"go.keploy.io/server/v3/pkg/agent/proxy/directive"
	"go.uber.org/zap"
)

// byteCountingConn counts bytes written to the wire. The relay writes the
// flushed client hold through this before the TLS upgrade wraps it, so a
// snapshot taken at upgrade time is exactly the number of CLEARTEXT bytes
// that reached mysqld.
type byteCountingConn struct {
	net.Conn
	written atomic.Int64
}

func (c *byteCountingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

// TestClientWriteHold_AgainstARealMySQLServer drives a REAL TLS-enabled
// mysqld through the REAL relay with a REAL TLS upgrade on both sides.
//
// The assertion that earns this test its place is upstreamCleartext: the
// relay must forward the 36-byte SSLRequest to the server and NOTHING
// after it. Everything past that byte belongs to the client's ClientHello,
// which the hold must keep back for keploy's own handshake — if it leaks,
// mysqld reads TLS bytes as protocol and the connection is corrupted.
//
// That number is measured on the destination socket, not on the tee. An
// earlier version read it off ClientStream, where it was always 36
// regardless of the hold (the tee delivers client bytes either way) — it
// asserted a property of go-sql-driver, not of keploy, and stayed green
// with the flush written nowhere at all.
//
// Skips unless a TLS-capable MySQL is reachable; where the lane promised
// one (KEPLOY_TEST_MYSQL_REQUIRED) a missing server is a hard failure so
// the coverage cannot rot unnoticed.
func TestClientWriteHold_AgainstARealMySQLServer(t *testing.T) {
	addr, user, pass, dbName := mysqlTestTarget(t)

	mitm := selfSignedCert(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	type result struct {
		upstreamCleartext  int64
		parserAfterUpgrade []byte
		err                error
	}
	resCh := make(chan result, 1)

	// The relay must outlive the report: the query runs AFTER the upgrade,
	// so tearing the context down at report time kills the connection
	// mid-query and the driver returns "invalid connection".
	release := make(chan struct{})
	defer close(release)

	go func() {
		clientConn, err := ln.Accept()
		if err != nil {
			resCh <- result{err: fmt.Errorf("accept: %w", err)}
			return
		}
		defer func() { _ = clientConn.Close() }()

		rawDest, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			resCh <- result{err: fmt.Errorf("dial mysql: %w", err)}
			return
		}
		defer func() { _ = rawDest.Close() }()
		destConn := &byteCountingConn{Conn: rawDest}

		// Snapshot the upstream byte count at the instant of the upgrade,
		// before the TLS handshake writes anything of its own.
		var cleartextAtUpgrade atomic.Int64
		upgrade := func(_ context.Context, c net.Conn, isClient bool, _ *tls.Config) (net.Conn, error) {
			if isClient {
				cleartextAtUpgrade.Store(destConn.written.Load())
				tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // MITM dial to a test server
				if err := tc.Handshake(); err != nil {
					return nil, fmt.Errorf("dest handshake: %w", err)
				}
				return tc, nil
			}
			ts := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{mitm}})
			if err := ts.Handshake(); err != nil {
				return nil, fmt.Errorf("client handshake: %w", err)
			}
			return ts, nil
		}

		r := New(Config{
			Logger:           zap.NewNop(),
			HoldClientWrites: true,
			TLSUpgradeFn:     upgrade,
		}, clientConn, destConn)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		go func() { _ = r.Run(ctx) }()
		defer func() { <-release }()

		if _, err := readMySQLPacket(r.DestStream()); err != nil {
			resCh <- result{err: fmt.Errorf("read greeting: %w", err)}
			return
		}
		sslReq, err := readMySQLPacket(r.ClientStream())
		if err != nil {
			resCh <- result{err: fmt.Errorf("read SSLRequest: %w", err)}
			return
		}

		// A real parser does not answer instantly: it decodes the packet,
		// consults session state and only then emits the directive. That
		// gap is the whole race. With a scripted parser on loopback the
		// directive lands before the client's ClientHello has even
		// arrived, which hides the defect completely — the unfixed path
		// passes 5/5 with this sleep at 0 and fails 5/5 with it set.
		time.Sleep(parserThinkTime)

		d := directive.UpgradeTLS(&tls.Config{}, &tls.Config{}, "mysql client_ssl")
		d.TLS.ClientFlushBytes = len(sslReq)
		r.Directives() <- d
		ack := <-r.Acks()
		if !ack.OK {
			resCh <- result{err: fmt.Errorf("upgrade ack: %+v", ack)}
			return
		}

		next, err := readMySQLPacket(r.ClientStream())
		if err != nil {
			resCh <- result{err: fmt.Errorf("read post-TLS packet: %w", err)}
			return
		}
		resCh <- result{upstreamCleartext: cleartextAtUpgrade.Load(), parserAfterUpgrade: next}
	}()

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?tls=skip-verify&timeout=15s&readTimeout=15s",
		user, pass, ln.Addr().String(), dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var got int
	qErr := db.QueryRow("SELECT 42").Scan(&got)

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("relay side: %v", res.err)
		}
		if res.upstreamCleartext != 36 {
			t.Errorf("%d cleartext bytes reached mysqld before the upgrade, want exactly the "+
				"36-byte SSLRequest. Anything beyond it is the client's ClientHello leaking "+
				"upstream, which corrupts the server's view of the connection.",
				res.upstreamCleartext)
		}
		if len(res.parserAfterUpgrade) > 0 && res.parserAfterUpgrade[0] == 0x16 {
			t.Fatalf("after the upgrade the parser read TLS handshake bytes (% x...). Against a "+
				"REAL server this is the hang: a MySQL header of 16 03 01 00 declares "+
				"payloadLength=66326, so the parser blocks until the watchdog retires it and "+
				"the connection records zero mocks.", res.parserAfterUpgrade[:4])
		}
	case <-time.After(30 * time.Second):
		t.Fatal("relay side never reported")
	}

	if qErr != nil {
		t.Fatalf("the query failed through the relay: %v", qErr)
	}
	if got != 42 {
		t.Fatalf("SELECT 42 returned %d", got)
	}
}

// parserThinkTime models the real parser's decode latency, which is what
// opens the window for the client's ClientHello to reach mysqld.
const parserThinkTime = 100 * time.Millisecond

// readMySQLPacket reads one length-prefixed MySQL packet (4-byte header,
// 3-byte LE payload length) and returns header+payload.
func readMySQLPacket(c net.Conn) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return nil, err
	}
	n := int(uint32(hdr[0]) | uint32(hdr[1])<<8 | uint32(hdr[2])<<16)
	buf := make([]byte, 4+n)
	copy(buf, hdr)
	if n > 0 {
		if _, err := io.ReadFull(c, buf[4:]); err != nil {
			return nil, err
		}
	}
	return buf, nil
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "keploy-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// mysqlTestTarget resolves the server to test against and PROVES it is
// usable with a direct, unproxied query first.
//
// Without that preflight, an unrelated MySQL on 3306 (wrong password,
// missing database) fails the test through keploy and gets diagnosed as
// "client bytes reached the server in cleartext" — a confident, wrong
// answer. Only a failure that the direct connection did NOT hit belongs to
// keploy.
//
// A missing server is a hard failure only where the lane PROMISED one,
// signalled by KEPLOY_TEST_MYSQL_REQUIRED (set by go-test.yaml, which
// provides the service). Keying this on the generic CI var instead would
// redden every fork and every unrelated lane over a database they were
// never offered, while still not making the test run anywhere.
func mysqlTestTarget(t *testing.T) (addr, user, pass, dbName string) {
	t.Helper()
	addr = envOr("KEPLOY_TEST_MYSQL_ADDR", "127.0.0.1:3306")
	user = envOr("KEPLOY_TEST_MYSQL_USER", "root")
	pass = envOr("KEPLOY_TEST_MYSQL_PASS", "rootpass")
	dbName = envOr("KEPLOY_TEST_MYSQL_DB", "sampledb")

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?tls=skip-verify&timeout=5s&readTimeout=5s",
		user, pass, addr, dbName)
	db, err := sql.Open("mysql", dsn)
	if err == nil {
		defer func() { _ = db.Close() }()
		var probe int
		err = db.QueryRow("SELECT 1").Scan(&probe)
	}
	if err != nil {
		msg := fmt.Sprintf("no usable TLS-capable MySQL at %s (%v); set KEPLOY_TEST_MYSQL_ADDR/"+
			"USER/PASS/DB", addr, err)
		if os.Getenv("KEPLOY_TEST_MYSQL_REQUIRED") != "" {
			t.Fatalf("%s. KEPLOY_TEST_MYSQL_REQUIRED is set, so this lane promised a MySQL: "+
				"the real MySQL TLS handshake, and a skip would let them rot unnoticed.", msg)
		}
		t.Skip(msg)
	}
	return addr, user, pass, dbName
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
