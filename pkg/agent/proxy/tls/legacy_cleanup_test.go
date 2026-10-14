package tls

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

// TestFileIsExactlyCert is the core safety property of legacy cleanup: it matches
// a single-certificate file by fingerprint, and never matches a multi-cert bundle
// (so a shared system bundle is never deleted) or a non-matching cert.
func TestFileIsExactlyCert(t *testing.T) {
	dir := t.TempDir()
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	fp := sha256.Sum256(ca.der)

	// 1. A file that is exactly this cert matches its fingerprint.
	single := filepath.Join(dir, "single.crt")
	if err := os.WriteFile(single, ca.certPEM, 0644); err != nil {
		t.Fatal(err)
	}
	if !fileIsExactlyCert(single, fp) {
		t.Error("a single matching cert file should match its fingerprint")
	}

	// 2. A different fingerprint does not match.
	other, _ := generateRunCA()
	if fileIsExactlyCert(single, sha256.Sum256(other.der)) {
		t.Error("a non-matching fingerprint must not match")
	}

	// 3. A two-cert bundle never matches (guards against deleting a system bundle).
	bundle := filepath.Join(dir, "bundle.crt")
	twoCerts := append(append([]byte{}, ca.certPEM...), other.certPEM...)
	if err := os.WriteFile(bundle, twoCerts, 0644); err != nil {
		t.Fatal(err)
	}
	if fileIsExactlyCert(bundle, fp) {
		t.Error("a multi-cert bundle must never be matched for deletion")
	}

	// 4. A non-PEM file does not match.
	junk := filepath.Join(dir, "junk.crt")
	if err := os.WriteFile(junk, []byte("not a cert"), 0644); err != nil {
		t.Fatal(err)
	}
	if fileIsExactlyCert(junk, fp) {
		t.Error("a non-cert file must not match")
	}
}

// TestLegacyCAFingerprint checks the retired-CA fingerprint constant parses to 32
// bytes — a build-time invariant the whole sweep relies on.
func TestLegacyCAFingerprint(t *testing.T) {
	fp, ok := legacyCAFingerprint()
	if !ok {
		t.Fatal("legacy CA fingerprint constant failed to parse")
	}
	var zero [32]byte
	if fp == zero {
		t.Fatal("legacy CA fingerprint parsed to all zeros")
	}
}
