package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

// appTLSCert mints the certificate the APPLICATION serves. The point of the
// passthrough is that the client validates against this, never against
// anything keploy minted.
func appTLSCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "the-app"},
		DNSNames:              []string{"the-app"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// An HTTPS client must reach the application through the ingress forwarder and
// complete a handshake against the APPLICATION's own certificate.
//
// Before the TLS branch the ClientHello fell through to the HTTP/1.1 parser,
// which answered in plaintext — the kubelet reported "server gave HTTP response
// to HTTPS client", the startup probe never passed, and the pod was dropped
// from its Service for the whole recording.
func TestIngress_TLSClientHelloIsPassedThroughToTheApp(t *testing.T) {
	cert, pool := appTLSCert(t)

	// The application: a TLS listener on its relocated port.
	appLn, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("app listen: %v", err)
	}
	defer appLn.Close()

	appDone := make(chan error, 1)
	go func() {
		c, err := appLn.Accept()
		if err != nil {
			appDone <- err
			return
		}
		defer c.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(c, buf); err != nil {
			appDone <- err
			return
		}
		_, err = c.Write([]byte("pong!"))
		appDone <- err
	}()

	// The forwarder: listens where the app used to, dials where it now is.
	proxyLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	defer proxyLn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pm := &IngressProxyManager{logger: zap.NewNop()}
	go func() {
		c, err := proxyLn.Accept()
		if err != nil {
			return
		}
		pm.handleConnection(ctx, c, appLn.Addr().String(), zap.NewNop(), nil, nil, 8443)
	}()

	// The client validates against the APP's CA only. If anything terminated
	// TLS in the middle, this handshake fails.
	conn, err := tls.Dial("tcp4", proxyLn.Addr().String(), &tls.Config{
		RootCAs:    pool,
		ServerName: "the-app",
	})
	if err != nil {
		t.Fatalf("TLS handshake through the ingress forwarder failed: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping!")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "pong!" {
		t.Errorf("payload corrupted through the pump: got %q want %q", got, "pong!")
	}
	if err := <-appDone; err != nil {
		t.Errorf("app side: %v", err)
	}

	// The handshake must be against the app's own leaf, not a minted one.
	if cn := conn.ConnectionState().PeerCertificates[0].Subject.CommonName; cn != "the-app" {
		t.Errorf("client was served a different certificate: CN=%q", cn)
	}
}
