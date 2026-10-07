package tls

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

// ipSANTestCA mints a throwaway signing CA for these cases.
func ipSANTestCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "keploy-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	crt, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return key, crt
}

// helloFrom builds a ClientHelloInfo whose Conn has a real TCP source port,
// since CertForClient keys its source-port bookkeeping off it.
func helloFrom(t *testing.T, sni string, srcPort int) *tls.ClientHelloInfo {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	return &tls.ClientHelloInfo{
		ServerName: sni,
		Conn:       &portConn{Conn: c1, port: srcPort},
	}
}

type portConn struct {
	net.Conn
	port int
}

func (p *portConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p.port}
}

// An app dialling an IP literal sends no SNI (RFC 6066). Before the destHost
// fallback the leaf was minted with CN="" and no SAN at all, so any client that
// verifies the peer — client-go against the API server ClusterIP, for one —
// rejected it with "doesn't contain any IP SANs".
func TestCertForClient_IPLiteralDestinationGetsIPSAN(t *testing.T) {
	key, ca := ipSANTestCA(t)
	logger := zap.NewNop()

	for _, tc := range []struct {
		name     string
		destHost string
		wantIP   string
	}{
		{"ipv4 apiserver clusterIP", "10.96.0.1", "10.96.0.1"},
		{"ipv6", "::1", "::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SrcPortToDstURL.Delete(40000)
			getCertCache().Remove(tc.destHost)

			cert, err := CertForClient(logger, helloFrom(t, "", 40000), key, ca, time.Time{}, tc.destHost)
			if err != nil {
				t.Fatalf("CertForClient: %v", err)
			}
			leaf, err := x509.ParseCertificate(cert.Certificate[0])
			if err != nil {
				t.Fatalf("parse leaf: %v", err)
			}
			want := net.ParseIP(tc.wantIP)
			var found bool
			for _, got := range leaf.IPAddresses {
				if got.Equal(want) {
					found = true
				}
			}
			if !found {
				t.Errorf("leaf has no IP SAN for %s: IPAddresses=%v DNSNames=%v",
					tc.wantIP, leaf.IPAddresses, leaf.DNSNames)
			}
			// A bare IP must never be filed as a DNS name: that is exactly the
			// silent demotion a "host:port" or bracketed value would cause.
			for _, dns := range leaf.DNSNames {
				if dns == tc.wantIP {
					t.Errorf("IP %s was minted as a DNS SAN, not an IP SAN", tc.wantIP)
				}
			}
		})
	}
}

// With SNI present the destHost must be ignored entirely, so the overwhelmingly
// common path keeps minting exactly the certificate it did before.
func TestCertForClient_SNIWinsOverDestHost(t *testing.T) {
	key, ca := ipSANTestCA(t)
	logger := zap.NewNop()
	SrcPortToDstURL.Delete(40001)
	getCertCache().Remove("api.example.com")

	cert, err := CertForClient(logger, helloFrom(t, "api.example.com", 40001), key, ca, time.Time{}, "10.96.0.1")
	if err != nil {
		t.Fatalf("CertForClient: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if len(leaf.IPAddresses) != 0 {
		t.Errorf("destHost leaked into an SNI handshake: IPAddresses=%v", leaf.IPAddresses)
	}
	var ok bool
	for _, d := range leaf.DNSNames {
		if d == "api.example.com" {
			ok = true
		}
	}
	if !ok {
		t.Errorf("SNI host missing from leaf: DNSNames=%v", leaf.DNSNames)
	}
}

// nonTCPAddr is what the v2 relay's fakeconn reports: a valid net.Addr that is
// not a *net.TCPAddr. CertForClient used to assert the concrete type without
// checking, so this shape panicked and took the agent down.
type nonTCPAddr struct{}

func (nonTCPAddr) Network() string { return "pipe" }
func (nonTCPAddr) String() string  { return "pipe" }

type fakeAddrConn struct{ net.Conn }

func (fakeAddrConn) RemoteAddr() net.Addr { return nonTCPAddr{} }

func TestCertForClient_NonTCPRemoteAddrDoesNotPanic(t *testing.T) {
	key, ca := ipSANTestCA(t)
	logger := zap.NewNop()
	getCertCache().Remove("relay.example.com")

	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	hello := &tls.ClientHelloInfo{
		ServerName: "relay.example.com",
		Conn:       fakeAddrConn{Conn: c1},
	}

	cert, err := CertForClient(logger, hello, key, ca, time.Time{}, "")
	if err != nil {
		t.Fatalf("CertForClient on a non-TCP RemoteAddr: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	var ok bool
	for _, d := range leaf.DNSNames {
		if d == "relay.example.com" {
			ok = true
		}
	}
	if !ok {
		t.Errorf("leaf not named from SNI: DNSNames=%v", leaf.DNSNames)
	}
}

// With no usable source port the port-keyed map must be left alone entirely,
// or every such connection would file under key 0 and read back a destination
// belonging to a different connection.
func TestCertForClient_NonTCPRemoteAddrDoesNotTouchPortMap(t *testing.T) {
	key, ca := ipSANTestCA(t)
	logger := zap.NewNop()
	SrcPortToDstURL.Delete(0)
	getCertCache().Remove("a.example.com")

	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	if _, err := CertForClient(logger,
		&tls.ClientHelloInfo{ServerName: "a.example.com", Conn: fakeAddrConn{Conn: c1}},
		key, ca, time.Time{}, ""); err != nil {
		t.Fatalf("CertForClient: %v", err)
	}
	if v, ok := SrcPortToDstURL.Load(0); ok {
		t.Errorf("port 0 was written to the destination map: %v", v)
	}
}

// destHost may NAME the certificate, but it must not masquerade as a captured
// SNI: SrcPortToDstURL is the captured-SNI/CONNECT-host bookkeeping, and before
// this pin the IP fallback was written into it, so every consumer of the map
// saw a value the client never sent.
func TestCertForClient_DestHostDoesNotPolluteCapturedSNI(t *testing.T) {
	key, ca := ipSANTestCA(t)
	logger := zap.NewNop()
	const port = 40777

	SrcPortToDstURL.Delete(port)
	cert, err := CertForClient(logger, helloFrom(t, "", port), key, ca, time.Time{}, "10.9.9.9")
	if err != nil {
		t.Fatalf("CertForClient: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if len(leaf.IPAddresses) == 0 || leaf.IPAddresses[0].String() != "10.9.9.9" {
		t.Fatalf("the leaf must still carry the destination IP SAN, got %v", leaf.IPAddresses)
	}
	if v, ok := SrcPortToDstURL.Load(port); ok {
		if s, _ := v.(string); s == "10.9.9.9" {
			t.Fatalf("destHost leaked into SrcPortToDstURL as a captured SNI: %q", s)
		}
	}
}
