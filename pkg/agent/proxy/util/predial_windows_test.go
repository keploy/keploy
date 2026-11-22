//go:build windows

package util

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"golang.org/x/sys/windows"
)

// TestClosedBeforeAnswering_Winsock pins how Winsock socket errors classify on
// Windows: peer-reset and shutdown codes (WSAECONNRESET, WSAENETRESET,
// WSAECONNABORTED, ERROR_NETNAME_DELETED, ERROR_CONNECTION_ABORTED,
// WSAESHUTDOWN) trigger a pre-dial redial while timeouts and unrelated Winsock
// errors do not.
func TestClosedBeforeAnswering_Winsock(t *testing.T) {
	cases := []struct {
		name  string
		errno syscall.Errno
		want  bool
	}{
		{"WSAECONNRESET", windows.WSAECONNRESET, true},
		{"WSAENETRESET", windows.WSAENETRESET, true},
		{"WSAECONNABORTED", windows.WSAECONNABORTED, true},
		{"ERROR_NETNAME_DELETED", windows.ERROR_NETNAME_DELETED, true},
		{"ERROR_CONNECTION_ABORTED", windows.ERROR_CONNECTION_ABORTED, true},
		{"WSAESHUTDOWN", windows.WSAESHUTDOWN, true},
		{"WSAETIMEDOUT", windows.WSAETIMEDOUT, false},
		{"WSAECONNREFUSED", windows.WSAECONNREFUSED, false},
		{"WSAEHOSTUNREACH", windows.WSAEHOSTUNREACH, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", tc.errno)}
			if got := closedBeforeAnswering(err); got != tc.want {
				t.Fatalf("closedBeforeAnswering(%s/%d) = %v, want %v", tc.name, uintptr(tc.errno), got, tc.want)
			}
		})
	}
}

// TestDialTLSRedialsOnWinsockReset deterministically verifies that DialTLS
// redials when the pre-dialled connection fails with Winsock's WSAECONNRESET
// (10054) or WSAECONNABORTED (10053, reset after peer FIN) — neither of which
// syscall.ECONNRESET matches on Windows — and does not redial on WSAETIMEDOUT.
func TestDialTLSRedialsOnWinsockReset(t *testing.T) {
	cert := selfSigned(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
				_ = tc.Handshake()
				_, _ = io.Copy(io.Discard, tc)
				_ = tc.Close()
			}()
		}
	}()
	addr := l.Addr().String()

	for _, tc := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"WSAECONNRESET redials", windows.WSAECONNRESET},
		{"WSAECONNABORTED redials", windows.WSAECONNABORTED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := accepted.Load()
			resetErr := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", tc.errno)}
			fake := &readErrConn{err: resetErr}
			conn, err := DialTLS(context.Background(), nil, "tcp", addr, &tls.Config{InsecureSkipVerify: true}, Adopt(addr, fake)) //nolint:gosec // test server
			if err != nil {
				t.Fatalf("DialTLS: %v; must redial on %s", err, tc.name)
			}
			t.Cleanup(func() { _ = conn.Close() })
			if !fake.closed.Load() {
				t.Fatal("DialTLS did not close the reset pre-dialled connection before redialling")
			}
			if connseq.Of(conn) == nil {
				t.Fatal("the redialled connection has no connseq.Upstream under its TLS")
			}
			waitCount(t, &accepted, before+1)
		})
	}

	t.Run("WSAETIMEDOUT does not redial", func(t *testing.T) {
		before := accepted.Load()
		timeoutErr := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("wsarecv", windows.WSAETIMEDOUT)}
		fake := &readErrConn{err: timeoutErr}
		_, err := DialTLS(context.Background(), nil, "tcp", addr, &tls.Config{InsecureSkipVerify: true}, Adopt(addr, fake)) //nolint:gosec // test server
		if err == nil {
			t.Fatal("DialTLS succeeded on WSAETIMEDOUT")
		}
		if !fake.closed.Load() {
			t.Fatal("DialTLS leaked the timed-out pre-dialled connection")
		}
		if got := accepted.Load(); got != before {
			t.Fatalf("destination saw %d extra connections on WSAETIMEDOUT, want 0", got-before)
		}
	})
}
