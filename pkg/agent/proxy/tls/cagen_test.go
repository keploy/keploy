package tls

import (
	"crypto/ecdsa"
	"crypto/x509"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestGenerateRunCA_Properties pins the shape of the generated CA that other
// parts of the design depend on: it is a usable CA, ECDSA, and backdated far
// enough that a frozen-time replay of an old recording still chains.
func TestGenerateRunCA_Properties(t *testing.T) {
	ca, err := generateRunCA()
	if err != nil {
		t.Fatalf("generateRunCA: %v", err)
	}
	if !ca.cert.IsCA || !ca.cert.BasicConstraintsValid {
		t.Fatalf("generated cert is not a valid CA: IsCA=%v BCValid=%v", ca.cert.IsCA, ca.cert.BasicConstraintsValid)
	}
	if ca.cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatal("generated CA lacks KeyUsageCertSign")
	}
	if _, ok := ca.signer.(*ecdsa.PrivateKey); !ok {
		t.Fatalf("signer is not ECDSA: %T", ca.signer)
	}
	if !strings.HasPrefix(ca.cert.Subject.CommonName, caCNPrefix) {
		t.Fatalf("CN %q does not carry the keploy prefix", ca.cert.Subject.CommonName)
	}
	// NotBefore must be well in the past (design: now-20y) so frozen-time leaves
	// (NotBefore = recordTime-1y) still fall inside the CA's validity.
	if ca.cert.NotBefore.After(time.Now().AddDate(-15, 0, 0)) {
		t.Fatalf("NotBefore %v is not backdated enough for frozen-time replays", ca.cert.NotBefore)
	}
	if ca.cert.NotAfter.Before(time.Now().AddDate(5, 0, 0)) {
		t.Fatalf("NotAfter %v expires too soon", ca.cert.NotAfter)
	}
	// Self-signed: it verifies against itself.
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := ca.cert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Fatalf("generated CA does not verify against itself: %v", err)
	}
}

// TestGenerateRunCA_Unique proves each run gets a distinct CA (fresh key, serial
// and id) — the whole point of per-run generation.
func TestGenerateRunCA_Unique(t *testing.T) {
	a, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if a.runID == b.runID {
		t.Fatal("two runs produced the same run id")
	}
	if a.fp == b.fp {
		t.Fatal("two runs produced the same certificate")
	}
	if a.cert.SerialNumber.Cmp(b.cert.SerialNumber) == 0 {
		t.Fatal("two runs produced the same serial number")
	}
}

// TestCAStateFromPEM_RoundTrips confirms the persist/reload path (fileStore) is
// lossless and keeps the run id derived from the CN.
func TestCAStateFromPEM_RoundTrips(t *testing.T) {
	orig, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := caStateFromPEM(orig.certPEM, orig.keyPEM)
	if err != nil {
		t.Fatalf("caStateFromPEM: %v", err)
	}
	if reloaded.runID != orig.runID {
		t.Fatalf("run id not preserved across reload: %q != %q", reloaded.runID, orig.runID)
	}
	if reloaded.fp != orig.fp {
		t.Fatal("certificate changed across reload")
	}
}

// TestGetActiveCA_LazyInit covers the safety net: a HandleTLSConnection caller
// that never ran SetupCAForApp still gets a usable CA instead of a nil deref.
func TestGetActiveCA_LazyInit(t *testing.T) {
	// Save and clear the package CA so this test is independent of ordering.
	saved := activeCA.Load()
	activeCA.Store(nil)
	t.Cleanup(func() { activeCA.Store(saved) })

	ca := getActiveCA(zap.NewNop())
	if ca == nil {
		t.Fatal("getActiveCA returned nil with no active CA set")
	}
	if ca.signer == nil || ca.cert == nil {
		t.Fatal("lazily-generated CA is incomplete")
	}
	// A second call returns the same CA (it was stored, not regenerated).
	if again := getActiveCA(zap.NewNop()); again.fp != ca.fp {
		t.Fatal("getActiveCA regenerated instead of reusing the stored CA")
	}
}

// TestCertCacheKey_BucketsAndCA pins the cache-key fix: distinct backdates never
// collide, a zero backdate buckets to one "live" key, and a different CA yields a
// different key.
func TestCertCacheKey_BucketsAndCA(t *testing.T) {
	ca1, _ := generateRunCA()
	ca2, _ := generateRunCA()

	live1 := certCacheKey("example.com", time.Time{}, ca1.cert)
	live2 := certCacheKey("example.com", time.Time{}, ca1.cert)
	if live1 != live2 {
		t.Fatal("two live connections to the same host must share a cache key")
	}

	frozen := certCacheKey("example.com", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), ca1.cert)
	if frozen == live1 {
		t.Fatal("a frozen-time backdate must not share the live cache key")
	}

	otherCA := certCacheKey("example.com", time.Time{}, ca2.cert)
	if otherCA == live1 {
		t.Fatal("a different signing CA must produce a different cache key")
	}

	otherHost := certCacheKey("other.com", time.Time{}, ca1.cert)
	if otherHost == live1 {
		t.Fatal("a different host must produce a different cache key")
	}
}
