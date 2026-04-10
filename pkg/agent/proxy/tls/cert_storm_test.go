package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
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
// Returns (hello, cleanup, err). The caller must call cleanup() when done.
// Uses error return instead of t.Fatal so it is safe to call from goroutines.
func helperClientHello(hostname string) (*tls.ClientHelloInfo, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, func() {}, err
	}
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		conn, dialErr := net.Dial("tcp", ln.Addr().String())
		if dialErr != nil {
			return
		}
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
		conn.Close()
	}()
	srvConn, err := ln.Accept()
	if err != nil {
		ln.Close()
		<-dialDone
		return nil, func() {}, err
	}
	cleanup := func() {
		srvConn.Close()
		ln.Close()
		<-dialDone
	}
	return &tls.ClientHelloInfo{ServerName: hostname, Conn: srvConn}, cleanup, nil
}

// resetCertCacheForTest resets the cert cache so tests start with a clean state.
// nolint:govet // intentional sync.Once reassignment for test isolation
func resetCertCacheForTest() {
	certCacheOnce = sync.Once{} //nolint:govet // test-only: resetting Once for isolation
	certCache = nil
}

// TestCertCacheHit verifies that 42 concurrent connections to the same hostname
// reuse a single cached certificate instead of generating 42 unique ones.
func TestCertCacheHit(t *testing.T) {
	resetCertCacheForTest()
	logger := zap.NewNop()
	caKey, caCert := helperCA(t)

	const hostname = "api.wise-sandbox.com"
	const concurrency = 42

	var (
		wg        sync.WaitGroup
		certCount atomic.Int32
		serials   sync.Map
		errCount  atomic.Int32
	)

	start := time.Now()
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hello, cleanup, err := helperClientHello(hostname)
			if err != nil {
				errCount.Add(1)
				return
			}
			defer cleanup()

			cert, err := CertForClient(logger, hello, caKey, caCert, time.Time{})
			if err != nil {
				errCount.Add(1)
				return
			}
			certCount.Add(1)
			leaf, err := x509.ParseCertificate(cert.Certificate[0])
			if err != nil {
				errCount.Add(1)
				return
			}
			serials.Store(leaf.SerialNumber.String(), true)
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if errCount.Load() > 0 {
		t.Errorf("%d goroutines encountered errors", errCount.Load())
	}

	uniqueSerials := 0
	serials.Range(func(_, _ interface{}) bool { uniqueSerials++; return true })

	t.Logf("Concurrency: %d, Unique certs: %d, Errors: %d, Time: %s",
		concurrency, uniqueSerials, errCount.Load(), elapsed)

	if certCount.Load() == 0 {
		t.Fatal("no certificates were generated")
	}

	// The cache now coalesces concurrent cold misses for the same hostname,
	// so every goroutine should receive the same generated certificate.
	if uniqueSerials != 1 {
		t.Errorf("expected exactly 1 unique cert from cache reuse, got %d", uniqueSerials)
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
	seen := make(map[string]string)

	for _, h := range hostnames {
		hello, cleanup, err := helperClientHello(h)
		if err != nil {
			t.Fatalf("helperClientHello(%q): %v", h, err)
		}
		cert, err := CertForClient(logger, hello, caKey, caCert, time.Time{})
		cleanup()
		if err != nil {
			t.Fatalf("CertForClient(%q): %v", h, err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatalf("ParseCertificate(%q): %v", h, err)
		}
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

	hello, cleanup, err := helperClientHello("")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	cert, err := CertForClient(logger, hello, caKey, caCert, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if cert == nil {
		t.Fatal("expected a cert for empty ServerName")
	}
}

// BenchmarkCertForClient measures cert generation cost per call by using
// unique hostnames to bypass the cache and exercise the full signing path.
func BenchmarkCertForClient(b *testing.B) {
	resetCertCacheForTest()
	logger := zap.NewNop()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Bench CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	if err != nil {
		b.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hello, cleanup, hErr := helperClientHello(fmt.Sprintf("bench-%d.example.com", i))
		if hErr != nil {
			b.Fatal(hErr)
		}
		_, _ = CertForClient(logger, hello, caKey, caCert, time.Time{})
		cleanup()
	}
}
