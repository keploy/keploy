//go:build linux

package proxy

import (
	"context"
	"crypto/tls"
	"net"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/models"
)

// The MITM names the leaf of a client that sends no SNI from that client's own
// connection. Another connection's host used to reach it through a map keyed
// by the client's source port that every connection shared, in two ways the
// tests below make: a handshake made earlier from the same port whose cleanup
// had not run (or lost to the new connection's claim on the port), and a live
// connection from another client with the same port (pods of one node reaching
// the DaemonSet agent). Both gave an IP-literal client a leaf for that host,
// which it rejects: "cannot validate certificate for 10.96.0.1 because it
// doesn't contain any IP SANs".

// dialFromPort dials addr from ip:port (port 0 for an ephemeral one) with
// SO_REUSEADDR, so a later connection can take the port an earlier one used.
func dialFromPort(t *testing.T, ip net.IP, port int, addr string) *net.TCPConn {
	t.Helper()
	d := net.Dialer{
		LocalAddr: &net.TCPAddr{IP: ip, Port: port},
		Control: func(_, _ string, rc syscall.RawConn) error {
			var serr error
			if err := rc.Control(func(fd uintptr) {
				serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			}); err != nil {
				return err
			}
			return serr
		},
	}
	c, err := d.Dial("tcp4", addr)
	if err != nil {
		t.Fatalf("dial %s from %s:%d: %v", addr, ip, port, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*net.TCPConn)
}

// acceptFromPort is both ends of a loopback connection from ip:port, as
// dialFromPort makes it: the application's, and the one a listener accepted.
func acceptFromPort(t *testing.T, ip net.IP, port int) (app *net.TCPConn, proxySide net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	app = dialFromPort(t, ip, port, ln.Addr().String())
	proxySide, err = ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxySide.Close() })
	return app, proxySide
}

func TestHandleConnectionNamesTheLeafForItsOwnClientAfterAnotherOnThePort(t *testing.T) {
	const ip = "10.96.0.1"
	loopback := net.IPv4(127, 0, 0, 1)

	// A handshake by keploy's MITM with SNI dep.example from an ephemeral
	// port, with nothing run after it: a connection whose cleanup has not run.
	before, beforeSrv := acceptFromPort(t, loopback, 0)
	port := before.LocalAddr().(*net.TCPAddr).Port
	beforeErr := make(chan error, 1)
	go func() {
		beforeErr <- tls.Client(before, &tls.Config{ServerName: "dep.example", InsecureSkipVerify: true}).Handshake() //nolint:gosec // the client of a handshake the test only makes
	}()
	if _, _, err := pTls.HandleTLSConnection(context.Background(), zap.NewNop(), beforeSrv, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := awaitEvent(t, beforeErr, "the earlier client's handshake"); err != nil {
		t.Fatal(err)
	}
	_ = before.Close()
	_ = beforeSrv.Close()

	// The next connection from that port, replayed to an IP literal.
	app, s := acceptFromPort(t, loopback, port)
	p := replayProxyTo(t, ip+":443", models.OutgoingOptions{})
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), &closeCountingConn{Conn: s}) }()
	client := leafCheckingClient(app, "", ip)
	if err := client.Handshake(); err != nil {
		t.Fatalf("source port %d, used before by a client with SNI dep.example: client handshake with %s: %v", port, ip, err)
	}
	_ = client.Close()
	awaitEvent(t, done, "handleConnection returning after the client left")
}

func TestHandleConnectionNamesTheLeafForItsOwnClientBesideAnotherOnThePort(t *testing.T) {
	const ip = "10.96.0.1"
	app, s := acceptedConn(t) // from 127.0.0.1:X
	port := app.LocalAddr().(*net.TCPAddr).Port

	// Another client, live, from 127.0.0.2:X: its handshake with SNI
	// dep.example is done and its connection waits for its first request.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2), Port: port}}
	other, err := d.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial from 127.0.0.2:%d (all of 127/8 is local on Linux): %v", port, err)
	}
	t.Cleanup(func() { _ = other.Close() })
	otherSrv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherSrv.Close() })
	otherProxy := replayProxyTo(t, ip+":443", models.OutgoingOptions{})
	otherDone := make(chan error, 1)
	go func() {
		otherDone <- otherProxy.handleConnection(context.Background(), &closeCountingConn{Conn: otherSrv})
	}()
	otherClient := tls.Client(other, &tls.Config{ServerName: "dep.example", InsecureSkipVerify: true}) //nolint:gosec // the other client's leaf is not under test
	if err := otherClient.Handshake(); err != nil {
		t.Fatalf("the other client's handshake: %v", err)
	}

	p := replayProxyTo(t, ip+":443", models.OutgoingOptions{})
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), s) }()
	client := leafCheckingClient(app, "", ip)
	if err := client.Handshake(); err != nil {
		t.Fatalf("127.0.0.1:%d, beside a live client on 127.0.0.2:%d with SNI dep.example: client handshake with %s: %v", port, port, ip, err)
	}
	_ = client.Close()
	awaitEvent(t, done, "handleConnection returning after the client left")
	_ = otherClient.Close()
	awaitEvent(t, otherDone, "the other client's handleConnection returning after it left")
}
