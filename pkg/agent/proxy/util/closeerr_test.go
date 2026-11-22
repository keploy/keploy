package util

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// closeErrConn is a net.Conn whose Close returns a fixed error.
type closeErrConn struct {
	net.Conn
	err error
}

func (c closeErrConn) Close() error { return c.err }

const destCloseMsg = "failed to close the destination connection"

// destCloseLevel closes dest the way the proxy's parser-panic cleanup does and
// returns the level the close failure was logged at. It fails the test unless
// exactly one entry for the close was logged.
func destCloseLevel(t *testing.T, dest net.Conn) zapcore.Level {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	recovered(zap.New(core), "test panic", nil, dest)
	entries := logs.FilterMessage(destCloseMsg).All()
	if len(entries) != 1 {
		var msgs []string
		for _, e := range logs.All() {
			msgs = append(msgs, e.Message)
		}
		t.Fatalf("want exactly one %q entry, got %d (logged: %q)", destCloseMsg, len(entries), msgs)
	}
	return entries[0].Level
}

func writeOpErr(errno error) error {
	return &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", errno)}
}

// tlsCloseNotifyErr builds the error crypto/tls's Conn.Close returns when its
// close_notify write fails (crypto/tls/conn.go, Conn.Close).
func tlsCloseNotifyErr(err error) error {
	return fmt.Errorf("tls: failed to send closeNotify alert (but connection was closed anyway): %w", err)
}

// TestCloseErrorLevels pins which Close failures are a peer that had already
// gone (Debug) and which are real failures (Error).
func TestCloseErrorLevels(t *testing.T) {
	// A real net.ErrClosed: the second Close of a real TCP connection.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	doubleCloseErr := c.Close()
	if !errors.Is(doubleCloseErr, net.ErrClosed) {
		t.Fatalf("second Close = %v, want net.ErrClosed", doubleCloseErr)
	}

	timeout := &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}

	cases := []struct {
		name string
		err  error
		want zapcore.Level
	}{
		{"EPIPE", writeOpErr(syscall.EPIPE), zapcore.DebugLevel},
		{"ECONNRESET", writeOpErr(syscall.ECONNRESET), zapcore.DebugLevel},
		{"net.ErrClosed", doubleCloseErr, zapcore.DebugLevel},
		{"tls closeNotify wrapping EPIPE", tlsCloseNotifyErr(writeOpErr(syscall.EPIPE)), zapcore.DebugLevel},
		{"tls closeNotify wrapping ECONNRESET", tlsCloseNotifyErr(writeOpErr(syscall.ECONNRESET)), zapcore.DebugLevel},
		{"tls closeNotify wrapping net.ErrClosed", tlsCloseNotifyErr(doubleCloseErr), zapcore.DebugLevel},

		// Not a closed peer: these must stay errors.
		{"EIO", writeOpErr(syscall.EIO), zapcore.ErrorLevel},
		{"ECONNREFUSED", writeOpErr(syscall.ECONNREFUSED), zapcore.ErrorLevel},
		{"tls closeNotify timed out", tlsCloseNotifyErr(timeout), zapcore.ErrorLevel},
		{"plain error", errors.New("close failed"), zapcore.ErrorLevel},
		// Matching is by the error chain, never the text: an error that only
		// says "broken pipe" carries no errno and is not trusted as one.
		{"text-only broken pipe", errors.New("write: broken pipe"), zapcore.ErrorLevel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := destCloseLevel(t, closeErrConn{err: tc.err}); got != tc.want {
				t.Fatalf("close error %q logged at %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestCloseErrorLevelNilLogsNothing: a clean Close logs no close failure.
func TestCloseErrorLevelNilLogsNothing(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	recovered(zap.New(core), "test panic", nil, closeErrConn{})
	if n := logs.FilterMessage(destCloseMsg).Len(); n != 0 {
		t.Fatalf("clean Close logged %d %q entries, want 0", n, destCloseMsg)
	}
}

// TestRealTLSCloseAfterPeerResetIsDebug reproduces the failure from a real
// socket: the upstream resets a finished TLS connection, then the proxy
// closes its side, and tls.Conn.Close's close_notify write fails. That is a
// peer that had already gone and must be logged at Debug, not Error.
func TestRealTLSCloseAfterPeerResetIsDebug(t *testing.T) {
	cert := selfSignedCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	serverDone := make(chan error, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		srv := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := srv.Handshake(); err != nil {
			_ = raw.Close()
			serverDone <- err
			return
		}
		// Hang up without close_notify, with an RST: the upstream is gone.
		_ = raw.(*net.TCPConn).SetLinger(0)
		serverDone <- raw.Close()
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := tls.Client(raw, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test-only self-signed peer
	t.Cleanup(func() { _ = raw.Close() })
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("server: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("server did not finish")
	}
	// Block until the reset has arrived, so Close writes to a reset socket.
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("read after the peer reset succeeded")
	}

	core, logs := observer.New(zapcore.DebugLevel)
	recovered(zap.New(core), "test panic", nil, client)
	entries := logs.FilterMessage(destCloseMsg).All()
	if len(entries) != 1 {
		t.Fatalf("want exactly one %q entry, got %d", destCloseMsg, len(entries))
	}
	got := entries[0]
	closeErr, _ := got.ContextMap()["error"].(string)
	if !strings.Contains(closeErr, "closeNotify") {
		t.Fatalf("close error %q is not the close_notify failure this test reproduces", closeErr)
	}
	if got.Level != zapcore.DebugLevel {
		t.Fatalf("close error %q logged at %v, want debug", closeErr, got.Level)
	}
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestLogCloseErrNilLogger: a nil logger must not panic on either path.
func TestLogCloseErrNilLogger(t *testing.T) {
	LogCloseErr(nil, writeOpErr(syscall.EPIPE), destCloseMsg)
	LogCloseErr(nil, errors.New("close failed"), destCloseMsg)
}
