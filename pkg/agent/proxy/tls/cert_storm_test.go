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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// helperCA creates a throwaway CA key+cert for tests.
func helperCA(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return caKey, cert
}

// helperClientHello creates a mock ClientHelloInfo backed by a real TCP connection.
func helperClientHello(t *testing.T, hostname string) (*tls.ClientHelloInfo, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		<-done
		conn.Close()
	}()
	srvConn, err := ln.Accept()
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	cleanup := func() {
		srvConn.Close()
		ln.Close()
	}
	return &tls.ClientHelloInfo{ServerName: hostname, Conn: srvConn}, cleanup
}

// resetCertCacheForTest resets the cert cache so tests start with a clean state.
func resetCertCacheForTest() {
	certCacheOnce = sync.Once{}
	certCache = nil
}

// TestCertCacheHit verifies that 42 concurrent connections to the same hostname
// reuse a single cached certificate instead of generating 42 unique ones.
func TestCertCacheHit(t *testing.T) {
	resetCertCacheForTest()
	logger, _ := zap.NewDevelopment()
	caKey, caCert := helperCA(t)

	const hostname = "api.wise-sandbox.com"
	const concurrency = 42

	var (
		wg        sync.WaitGroup
		certCount atomic.Int32
		serials   sync.Map
	)

	start := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hello, cleanup := helperClientHello(t, hostname)
			defer cleanup()

			cert, err := CertForClient(logger, hello, caKey, caCert, time.Time{})
			if err != nil {
				t.Errorf("CertForClient failed: %v", err)
				return
			}
			certCount.Add(1)
			leaf, _ := x509.ParseCertificate(cert.Certificate[0])
			serials.Store(leaf.SerialNumber.String(), true)
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// Count unique serials
	uniqueSerials := 0
	serials.Range(func(_, _ interface{}) bool { uniqueSerials++; return true })

	t.Logf("Concurrency: %d, Unique certs: %d, Time: %s", concurrency, uniqueSerials, elapsed)

	if certCount.Load() != concurrency {
		t.Fatalf("expected %d certs, got %d", concurrency, certCount.Load())
	}

	// With caching, the vast majority should share 1 cert.
	// Due to concurrent first-access race, a few goroutines may generate before
	// the first one stores the result. Allow up to 5 unique certs (generous).
	if uniqueSerials > 5 {
		t.Errorf("expected at most 5 unique certs (cache hit), got %d — cache not working", uniqueSerials)
	}

	if elapsed > 10*time.Second {
		t.Errorf("cert storm took %s — expected <10s with caching", elapsed)
	}
}

// TestCertCacheDistinctHostnames verifies different hostnames get different certs.
func TestCertCacheDistinctHostnames(t *testing.T) {
	resetCertCacheForTest()
	logger := zap.NewNop()
	caKey, caCert := helperCA(t)

	hostnames := []string{"a.example.com", "b.example.com", "c.example.com"}
	seen := make(map[string]string) // serial → hostname

	for _, h := range hostnames {
		hello, cleanup := helperClientHello(t, h)
		cert, err := CertForClient(logger, hello, caKey, caCert, time.Time{})
		cleanup()
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(cert.Certificate[0])
		serial := leaf.SerialNumber.String()

		if prev, dup := seen[serial]; dup {
			t.Errorf("hostname %q got same serial as %q — cache key collision", h, prev)
		}
		seen[serial] = h
	}

	if len(seen) != len(hostnames) {
		t.Errorf("expected %d unique certs, got %d", len(hostnames), len(seen))
	}
}

// TestCertCacheEmptyServerName verifies that an empty SNI still generates a cert
// (not cached, since we can't key on empty string).
func TestCertCacheEmptyServerName(t *testing.T) {
	resetCertCacheForTest()
	logger := zap.NewNop()
	caKey, caCert := helperCA(t)

	hello, cleanup := helperClientHello(t, "")
	defer cleanup()

	cert, err := CertForClient(logger, hello, caKey, caCert, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if cert == nil {
		t.Fatal("expected a cert for empty ServerName")
	}
}

// BenchmarkCertForClient measures cert generation cost per call.
func BenchmarkCertForClient(b *testing.B) {
	resetCertCacheForTest()
	logger := zap.NewNop()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Bench CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(der)

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		go func() {
			conn, _ := net.Dial("tcp", ln.Addr().String())
			if conn != nil {
				buf := make([]byte, 1)
				conn.Read(buf)
				conn.Close()
			}
		}()
		srvConn, _ := ln.Accept()
		hello := &tls.ClientHelloInfo{ServerName: "bench.example.com", Conn: srvConn}
		_, _ = CertForClient(logger, hello, caKey, caCert, time.Time{})
		srvConn.Close()
	}
}
