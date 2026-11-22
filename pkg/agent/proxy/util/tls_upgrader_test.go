package util

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestClientServerName: the SNI a client sent is read off the TLS connection
// keploy served it on, through the proxy's own wrappers, and anything else
// reads as "".
func TestClientServerName(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "keploy.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	served := func(sni string) *tls.Conn {
		t.Helper()
		app, keploySide := net.Pipe()
		t.Cleanup(func() { _ = app.Close(); _ = keploySide.Close() })
		srv := tls.Server(keploySide, &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
		appErr := make(chan error, 1)
		go func() {
			appErr <- tls.Client(app, &tls.Config{ServerName: sni, InsecureSkipVerify: true}).Handshake() //nolint:gosec // a test client
		}()
		if err := srv.Handshake(); err != nil {
			t.Fatal(err)
		}
		if err := <-appErr; err != nil {
			t.Fatal(err)
		}
		return srv
	}

	withSNI := served("db.internal")
	for name, conn := range map[string]net.Conn{
		"the TLS connection":           withSNI,
		"a SafeConn over it":           NewSafeConn(withSNI, zap.NewNop()),
		"a Conn over a SafeConn on it": &Conn{Conn: NewSafeConn(withSNI, zap.NewNop()), Logger: zap.NewNop()},
	} {
		if got := ClientServerName(conn); got != "db.internal" {
			t.Errorf("%s: ClientServerName = %q, want db.internal", name, got)
		}
	}
	if got := ClientServerName(served("")); got != "" {
		t.Errorf("a client that sent no SNI: ClientServerName = %q, want empty", got)
	}
	plain, other := net.Pipe()
	t.Cleanup(func() { _ = plain.Close(); _ = other.Close() })
	for name, conn := range map[string]net.Conn{
		"nil":                       nil,
		"a nil *tls.Conn":           (*tls.Conn)(nil),
		"a nil *SafeConn":           (*SafeConn)(nil),
		"a nil *Conn":               (*Conn)(nil),
		"a Conn over no connection": &Conn{},
		"a plain connection":        plain,
		"a SafeConn over plain":     NewSafeConn(plain, zap.NewNop()),
	} {
		if got := ClientServerName(conn); got != "" {
			t.Errorf("%s: ClientServerName = %q, want empty", name, got)
		}
	}
}
