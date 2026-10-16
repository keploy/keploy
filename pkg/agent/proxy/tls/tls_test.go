package tls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// mitmHandshakeIPLiteral runs the REAL keploy MITM (HandleTLSConnection) against
// a Go TLS client that dials an IP literal and trusts this run's CA, the way a
// client-go app reaches https://10.96.0.1. It returns the client's handshake
// error.
func mitmHandshakeIPLiteral(t *testing.T, hsCtx context.Context, ip string) error {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		_, _, _ = HandleTLSConnection(hsCtx, zap.NewNop(), c, time.Time{})
	}()
	defer func() { <-srvDone }()

	roots := x509.NewCertPool()
	roots.AddCert(getActiveCA(zap.NewNop()).cert)
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// ServerName is the IP literal: crypto/tls sends no SNI for it and verifies
	// the leaf against an IP SAN.
	client := tls.Client(raw, &tls.Config{ServerName: ip, RootCAs: roots})
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	return client.Handshake()
}

// TestHandleTLSConnection_IPLiteralClientVerifiesLeaf: with the destination IP
// on the context (WithDstIP), a verifying client that dialled an IP literal
// accepts the MITM leaf; without it the leaf has no IP SAN and the client fails
// exactly as replayed proxy A did (dsmon F7).
func TestHandleTLSConnection_IPLiteralClientVerifiesLeaf(t *testing.T) {
	resetCertCacheForTest()
	const ip = "10.96.0.1"

	if err := mitmHandshakeIPLiteral(t, WithDstIP(context.Background(), net.ParseIP(ip)), ip); err != nil {
		t.Fatalf("client handshake with the destination IP known: %v", err)
	}

	err := mitmHandshakeIPLiteral(t, context.Background(), ip)
	if err == nil || !strings.Contains(err.Error(), "IP SANs") {
		t.Fatalf("client handshake without the destination IP: err = %v, want the missing-IP-SAN failure", err)
	}
}
