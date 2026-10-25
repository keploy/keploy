package tls

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

// TestLegacyTempSweepCapsTheRead: the boot sweep reads only a bounded prefix
// of a ca.crt* file in the temp dir. The old client's copy was one PEM
// certificate, about 1 KB; a file over the cap is not that copy, so it is
// kept, even when it starts with the very CA, and a huge or endless file can
// no longer make the root sweep read all of it into memory.
func TestLegacyTempSweepCapsTheRead(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	if runtime.GOOS == "windows" {
		// os.TempDir there reads TMP, then TEMP, never TMPDIR.
		t.Setenv("TMP", tmp)
		t.Setenv("TEMP", tmp)
	}
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	fp := sha256.Sum256(ca.der)

	// The cert, then padding with no PEM marker in it: pem.Decode passes over
	// trailing bytes, so read whole, this file still matches the fingerprint.
	big := filepath.Join(tmp, "ca.crt.big")
	content := append(append([]byte{}, ca.certPEM...), bytes.Repeat([]byte("x"), 1<<20)...)
	if err := os.WriteFile(big, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if len(content) <= maxLegacyTempCertBytes {
		t.Fatalf("the padded file (%d bytes) must exceed the cap (%d)", len(content), maxLegacyTempCertBytes)
	}
	if !fileIsExactlyCert(big, fp) {
		t.Fatal("the padded file must match the fingerprint when read whole, or this row proves nothing")
	}
	leaked := filepath.Join(tmp, "ca.crt.leaked")
	if err := os.WriteFile(leaked, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zap.DebugLevel)
	removeLegacyTempFiles(zap.New(core), fp, false)

	if _, err := os.Lstat(big); err != nil {
		t.Errorf("a ca.crt* file over the read cap must be kept: %v", err)
	}
	named := false
	for _, e := range logs.All() {
		if p, ok := e.ContextMap()["path"].(string); ok && p == big && strings.Contains(e.Message, "skipped") {
			named = true
		}
	}
	if !named {
		t.Errorf("no debug line names the skipped oversized file %s", big)
	}
	if _, err := os.Lstat(leaked); !os.IsNotExist(err) {
		t.Errorf("a real leaked copy of the CA must still be removed; lstat: %v", err)
	}
}
