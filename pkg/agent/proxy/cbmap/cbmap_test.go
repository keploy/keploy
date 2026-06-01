package cbmap

import (
	"bytes"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
)

func TestPublishWritesPair(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cbmap.txt")
	t.Setenv("KEPLOY_CBMAP_PATH", path)

	// Reset in-memory state between tests (package globals).
	mu.Lock()
	entries = map[string]pair{}
	mu.Unlock()

	mitm := []byte("MITM-CERT-DER-BYTES")
	real := []byte("REAL-PG-CERT-DER-BYTES")

	if _, err := Publish(zap.NewNop(), mitm, real, x509.SHA256WithRSA); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	line := strings.TrimSpace(string(data))
	parts := strings.Fields(line)
	if len(parts) != 2 {
		t.Fatalf("expected 2 hex tokens, got %q", line)
	}
	if got, _ := hex.DecodeString(parts[0]); !bytes.Equal(got, sha256sum(mitm)) {
		t.Errorf("mitm hash mismatch: got %x want %x", got, sha256sum(mitm))
	}
	if got, _ := hex.DecodeString(parts[1]); !bytes.Equal(got, sha256sum(real)) {
		t.Errorf("real hash mismatch: got %x want %x", got, sha256sum(real))
	}
}

func TestPublishDeduplicates(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cbmap.txt")
	t.Setenv("KEPLOY_CBMAP_PATH", path)
	mu.Lock()
	entries = map[string]pair{}
	mu.Unlock()

	for i := 0; i < 5; i++ {
		if _, err := Publish(zap.NewNop(), []byte("M"), []byte("R"), x509.SHA256WithRSA); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	data, _ := os.ReadFile(path)
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Fatalf("expected 1 line after 5 identical Publishes, got %d:\n%s", n, data)
	}
}

func TestPublishMultipleUpstreams(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cbmap.txt")
	t.Setenv("KEPLOY_CBMAP_PATH", path)
	mu.Lock()
	entries = map[string]pair{}
	mu.Unlock()

	pairs := [][2][]byte{
		{[]byte("MITM-A"), []byte("REAL-A")},
		{[]byte("MITM-B"), []byte("REAL-B")},
		{[]byte("MITM-C"), []byte("REAL-C")},
	}
	for _, p := range pairs {
		if _, err := Publish(zap.NewNop(), p[0], p[1], x509.SHA256WithRSA); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	data, _ := os.ReadFile(path)
	if n := strings.Count(string(data), "\n"); n != 3 {
		t.Fatalf("expected 3 entries, got %d:\n%s", n, data)
	}
}

func TestHashAlgoSelection(t *testing.T) {
	cases := []struct {
		alg  x509.SignatureAlgorithm
		size int
	}{
		{x509.SHA256WithRSA, 32},
		{x509.ECDSAWithSHA256, 32},
		{x509.SHA384WithRSA, 48},
		{x509.ECDSAWithSHA384, 48},
		{x509.SHA512WithRSA, 64},
		{x509.ECDSAWithSHA512, 64},
		// RFC 5929 normalizes MD5 / SHA1 to SHA-256
		{x509.MD5WithRSA, 32},
		{x509.SHA1WithRSA, 32},
		{x509.ECDSAWithSHA1, 32},
		// Default / unknown also -> SHA-256
		{x509.UnknownSignatureAlgorithm, 32},
	}
	for _, c := range cases {
		h := hashForCBSig(c.alg)
		got := h.Size()
		if got != c.size {
			t.Errorf("alg=%v: hash size %d, want %d", c.alg, got, c.size)
		}
	}
}

func sha256sum(b []byte) []byte {
	h := hashForCBSig(x509.SHA256WithRSA)
	h.Write(b)
	return h.Sum(nil)
}

// --- Deferred-publish API ---

func resetPending(t *testing.T) {
	t.Helper()
	mu.Lock()
	entries = map[string]pair{}
	mu.Unlock()
	pendingMu.Lock()
	pending = map[string]*pendingPair{}
	pendingMu.Unlock()
}

func TestRegisterMITMFirstThenReal(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cbmap.txt")
	t.Setenv("KEPLOY_CBMAP_PATH", path)
	resetPending(t)
	logger := zap.NewNop()

	RegisterMITM(logger, "1234", []byte("MITM-DER"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cbmap.txt exists after only MITM registered: err=%v", err)
	}
	if n, _ := pendingSnapshot(); n != 1 {
		t.Fatalf("expected 1 pending entry, got %d", n)
	}

	RegisterReal(logger, "1234", []byte("REAL-DER"), x509.SHA256WithRSA)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cbmap.txt not written: %v", err)
	}
	if !strings.Contains(string(data), hex.EncodeToString(sha256sum([]byte("MITM-DER")))) {
		t.Fatalf("MITM hash missing from cbmap.txt:\n%s", data)
	}
	if !strings.Contains(string(data), hex.EncodeToString(sha256sum([]byte("REAL-DER")))) {
		t.Fatalf("REAL hash missing from cbmap.txt:\n%s", data)
	}
	if n, _ := pendingSnapshot(); n != 0 {
		t.Fatalf("expected 0 pending after publish, got %d", n)
	}
}

func TestRegisterRealFirstThenMITM(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cbmap.txt")
	t.Setenv("KEPLOY_CBMAP_PATH", path)
	resetPending(t)
	logger := zap.NewNop()

	RegisterReal(logger, "5678", []byte("REAL-DER-B"), x509.SHA256WithRSA)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cbmap.txt exists after only REAL registered: err=%v", err)
	}

	RegisterMITM(logger, "5678", []byte("MITM-DER-B"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cbmap.txt not written: %v", err)
	}
	if !strings.Contains(string(data), hex.EncodeToString(sha256sum([]byte("REAL-DER-B")))) {
		t.Fatalf("REAL hash missing")
	}
}

func TestRegisterIgnoresEmptyInputs(t *testing.T) {
	resetPending(t)
	RegisterMITM(zap.NewNop(), "", []byte("anything"))
	RegisterMITM(zap.NewNop(), "1", nil)
	RegisterReal(zap.NewNop(), "", []byte("anything"), x509.SHA256WithRSA)
	RegisterReal(zap.NewNop(), "1", nil, x509.SHA256WithRSA)
	if n, _ := pendingSnapshot(); n != 0 {
		t.Fatalf("empty inputs created %d pending entries; expected 0", n)
	}
}

func TestCleanupConnectionReapsOrphan(t *testing.T) {
	resetPending(t)
	RegisterMITM(zap.NewNop(), "orphan", []byte("MITM"))
	if n, _ := pendingSnapshot(); n != 1 {
		t.Fatalf("expected 1 pending before cleanup, got %d", n)
	}
	CleanupConnection("orphan")
	if n, _ := pendingSnapshot(); n != 0 {
		t.Fatalf("expected 0 pending after cleanup, got %d", n)
	}
}

func TestConcurrentRegistrationsAreSafe(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cbmap.txt")
	t.Setenv("KEPLOY_CBMAP_PATH", path)
	resetPending(t)
	logger := zap.NewNop()

	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(2)
		connID := fmt.Sprintf("conn-%d", i)
		mitm := []byte(fmt.Sprintf("MITM-%d", i))
		real := []byte(fmt.Sprintf("REAL-%d", i))
		go func() { defer wg.Done(); RegisterMITM(logger, connID, mitm) }()
		go func() { defer wg.Done(); RegisterReal(logger, connID, real, x509.SHA256WithRSA) }()
	}
	wg.Wait()

	if n, _ := pendingSnapshot(); n != 0 {
		t.Fatalf("expected 0 pending after all registrations, got %d", n)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cbmap.txt: %v", err)
	}
	lines := strings.Count(string(data), "\n")
	if lines != N {
		t.Fatalf("expected %d lines in cbmap.txt, got %d:\n%s", N, lines, data)
	}
}
