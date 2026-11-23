package tls

import (
	"context"
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

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func selfSignedClientCert(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}
}

// An mTLS client certificate's CommonName is traffic identity: HandleTLSConnection
// logs it as a host shape plus its per-session tag, never the raw CN.
func TestHandleTLSConnection_LogsMTLSClientSubjectAsShapeAndSessionTag(t *testing.T) {
	resetCertCacheForTest()
	const s = logleak.Canary
	clientCN := "billing." + s + ".internal"
	clientCert := selfSignedClientCert(t, clientCN)

	keys := models.NewLogKeySession()
	hsCtx := models.WithLogKeySession(WithDstIP(context.Background(), net.ParseIP("127.0.0.1")), keys)
	logger, logs := logleak.Observe()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	var isMTLS bool
	var srvErr error
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		c, aerr := ln.Accept()
		if aerr != nil {
			srvErr = aerr
			return
		}
		defer func() { _ = c.Close() }()
		var tc net.Conn
		tc, isMTLS, srvErr = HandleTLSConnection(hsCtx, logger, c, time.Time{})
		if tc != nil {
			_ = tc.Close()
		}
	}()

	roots := x509.NewCertPool()
	roots.AddCert(getActiveCA(zap.NewNop()).cert)
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := tls.Client(raw, &tls.Config{
		ServerName:   "127.0.0.1",
		RootCAs:      roots,
		Certificates: []tls.Certificate{clientCert},
	})
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	_ = client.Close()
	<-srvDone

	if srvErr != nil {
		t.Fatalf("HandleTLSConnection: %v", srvErr)
	}
	if !isMTLS {
		t.Fatal("precondition: expected isMTLS == true")
	}
	said := logs.FilterMessage("mTLS Handshake Success").All()
	if len(said) != 1 {
		t.Fatalf("precondition: want 1 mTLS Handshake Success line, got %d (%+v)", len(said), logs.All())
	}
	want := models.HostLogName(hsCtx, clientCN)
	if got := said[0].ContextMap()["client_subject"]; got != want {
		t.Errorf("client_subject = %q, want %q", got, want)
	}
	logleak.Check(t, logs, s, "mTLS handshake")
}
