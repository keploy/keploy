package proxy

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// pemOf is der in a PEM block of typ.
func pemOf(typ string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

// leafOf is c's certificate, parsed.
func leafOf(t *testing.T, c tls.Certificate) *x509.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// ecKeyPEM is c's ECDSA private key, as SEC 1 ("EC PRIVATE KEY") or PKCS#8.
func ecKeyPEM(t *testing.T, c tls.Certificate, pkcs8 bool) string {
	t.Helper()
	k := c.PrivateKey.(*ecdsa.PrivateKey)
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return pemOf("PRIVATE KEY", der)
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pemOf("EC PRIVATE KEY", der)
}

// rsaCert is a self-signed certificate on a new RSA key, and that key.
func rsaCert(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "kubernetes-admin"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

const (
	noKey      = "keploy was given no private key for the certificate the client sent (record.tlsPrivateKeyPath)"
	noPEM      = "record.tlsPrivateKeyPath holds no PEM block"
	notAKey    = "record.tlsPrivateKeyPath's PEM block is no private key keploy reads (PKCS1, PKCS8 or EC)"
	notItsOwn  = "record.tlsPrivateKeyPath's key is not the private key of the certificate the client sent"
	certUnsent = models.CaptureLossClientCertUnsent
)

// What keploy presents to the server for a client that sent it a
// certificate: that certificate, with the private key keploy was given, only
// when that key is the certificate's own (as PKCS#1, PKCS#8 or SEC 1);
// otherwise an error naming why. A key that is not the certificate's own was
// presented before, and the server refused the handshake: the call looked
// like the server's failure.
func TestClientCertIsTheClientsCertificateWithItsOwnKey(t *testing.T) {
	ec, other := selfSignedForDispatch(t), selfSignedForDispatch(t)
	ecLeaf := leafOf(t, ec)
	rsaLeaf, rsaKey := rsaCert(t)
	rsaPKCS1 := pemOf("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey))
	for _, tc := range []struct {
		name string
		peer *x509.Certificate
		key  string
		want string // the error; "" for the certificate presented
	}{
		{"no key", ecLeaf, "", noKey},
		{"a key that is not PEM", ecLeaf, "not a key", noPEM},
		{"a PEM block that is a certificate, not a key", ecLeaf, pemOf("CERTIFICATE", ecLeaf.Raw), notAKey},
		{"another certificate's EC key", ecLeaf, ecKeyPEM(t, other, false), notItsOwn},
		{"an RSA key for an EC certificate", ecLeaf, rsaPKCS1, notItsOwn},
		{"an EC key for an RSA certificate", rsaLeaf, ecKeyPEM(t, ec, true), notItsOwn},
		{"the certificate's own EC key (SEC 1)", ecLeaf, ecKeyPEM(t, ec, false), ""},
		{"the certificate's own EC key (PKCS#8)", ecLeaf, ecKeyPEM(t, ec, true), ""},
		{"the certificate's own RSA key (PKCS#1)", rsaLeaf, rsaPKCS1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, err := clientCert(tc.peer, tc.key)
			if tc.want != "" {
				if err == nil || err.Error() != tc.want {
					t.Fatalf("clientCert = %v, want the error %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("clientCert: %v", err)
			}
			if len(cert.Certificate) != 1 || !bytes.Equal(cert.Certificate[0], tc.peer.Raw) {
				t.Fatal("keploy would present another certificate than the client's")
			}
			if cert.PrivateKey == nil {
				t.Fatal("keploy would present the client's certificate with no key")
			}
		})
	}
}

// At record, a client that sends keploy's proxy a certificate (mutual TLS)
// has it presented to its server with the key record.tlsPrivateKeyPath
// names. Without that key, or with another certificate's, keploy cannot: it
// does not dial the server, and the call the client sent is a capture loss
// (client_cert_unsent) naming the destination and why, charged to the test
// it happened in, not the server's failure. Before, nothing was counted, and
// the test was charged as one that failed against its real infra.
func TestARecordedClientCertificateKeployCannotPresentIsACaptureLoss(t *testing.T) {
	serverCert, client, other := selfSignedForDispatch(t), selfSignedForDispatch(t), selfSignedForDispatch(t)
	for _, tc := range []struct {
		name string
		key  string
		lost string // the loss's reason; "" for the server getting the client's certificate
	}{
		{"no key", "", noKey},
		{"another certificate's key", ecKeyPEM(t, other, false), notItsOwn},
		{"the client certificate's own key", ecKeyPEM(t, client, true), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{
				Certificates: []tls.Certificate{serverCert},
				ClientAuth:   tls.RequireAnyClientCert,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			// The certificates the server got, one per handshake.
			got := make(chan []byte, 4)
			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					go func() {
						defer func() { _ = conn.Close() }()
						tc := conn.(*tls.Conn)
						if tc.Handshake() != nil {
							return
						}
						if peers := tc.ConnectionState().PeerCertificates; len(peers) > 0 {
							got <- peers[0].Raw
						}
						_, _ = conn.Read(make([]byte, 1024))
						_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
					}()
				}
			}()

			p := recordingProxy(t, ln.Addr().String(), zap.NewNop())
			if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{TLSPrivateKey: tc.key}); err != nil {
				t.Fatal(err)
			}
			app, src := acceptedConn(t)
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()
			c := tls.Client(app, &tls.Config{
				ServerName:         "localhost",
				Certificates:       []tls.Certificate{client},
				InsecureSkipVerify: true, //nolint:gosec // keploy's own certificate, in a test
			})
			if err := c.Handshake(); err != nil {
				t.Fatalf("the client's handshake with keploy's proxy: %v", err)
			}
			if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			if tc.lost == "" {
				if raw := awaitEvent(t, got, "the server's handshake"); !bytes.Equal(raw, client.Certificate[0]) {
					t.Fatal("the server got another certificate than the client's")
				}
				_ = c.Close()
				_ = awaitEvent(t, done, "handleConnection returning after the client left")
				for _, l := range p.CaptureLosses() {
					if l.Kind == certUnsent {
						t.Fatalf("a certificate keploy presented was counted lost: %+v", l)
					}
				}
				return
			}
			if err := awaitEvent(t, done, "handleConnection refusing the call"); err == nil || err.Error() != tc.lost {
				t.Fatalf("handleConnection returned %v, want %q", err, tc.lost)
			}
			_ = c.Close()
			var lost []models.CaptureLoss
			for _, l := range p.CaptureLosses() {
				if l.Kind == certUnsent {
					lost = append(lost, l)
				}
			}
			if want := "to " + ln.Addr().String() + ": " + tc.lost; len(lost) != 1 || lost[0].Count != 1 || lost[0].What != want {
				t.Fatalf("counted %+v, want one %s loss: %q", lost, certUnsent, want)
			}
			if !certUnsent.PerTest() || certUnsent.NotJudged() {
				t.Fatalf("%s must be charged to the test it happened in, and judge it", certUnsent)
			}
			if why := (models.CaptureLosses{lost[0]}).Text(); !strings.Contains(why, "record.tlsPrivateKeyPath") || !strings.Contains(why, "not the server's") {
				t.Fatalf("the loss reads %q; want it to say it is keploy's failure, not the server's, and to name record.tlsPrivateKeyPath", why)
			}
			select {
			case <-got:
				t.Fatal("keploy dialled the server with a certificate it could not present")
			default:
			}
		})
	}
}
