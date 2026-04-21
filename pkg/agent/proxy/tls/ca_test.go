package tls

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const (
	// Small dummy PEM blocks. The tests verify byte-level merge behaviour,
	// not x509 parsing, so self-signed strings are fine.
	systemBundleA = "-----BEGIN CERTIFICATE-----\nSYSTEM-A\n-----END CERTIFICATE-----\n"
	systemBundleB = "-----BEGIN CERTIFICATE-----\nSYSTEM-B\n-----END CERTIFICATE-----\n"
)

// newObservedLogger returns a zap logger backed by an observer so tests can
// assert on emitted log lines.
func newObservedLogger(t *testing.T) (*zap.Logger, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.WarnLevel)
	return zap.New(core), logs
}

// TestLoadSystemCABundle_HappyPath verifies the first readable, non-empty
// path in the ranked list wins.
func TestLoadSystemCABundle_HappyPath(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first-ca.crt")
	second := filepath.Join(dir, "second-ca.crt")
	if err := os.WriteFile(first, []byte(systemBundleA), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte(systemBundleB), 0644); err != nil {
		t.Fatal(err)
	}

	logger, _ := newObservedLogger(t)
	data, source := loadSystemCABundleFromPaths(logger, []string{first, second})
	if source != first {
		t.Fatalf("expected first path to win, got %q", source)
	}
	if string(data) != systemBundleA {
		t.Fatalf("wrong bytes: got %q", string(data))
	}
}

// TestLoadSystemCABundle_FallbackOnMissing verifies that a missing first path
// is skipped and the next readable path wins.
func TestLoadSystemCABundle_FallbackOnMissing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.crt")
	second := filepath.Join(dir, "real-ca.crt")
	if err := os.WriteFile(second, []byte(systemBundleB), 0644); err != nil {
		t.Fatal(err)
	}

	logger, _ := newObservedLogger(t)
	data, source := loadSystemCABundleFromPaths(logger, []string{missing, second})
	if source != second {
		t.Fatalf("expected fallback to second path, got %q", source)
	}
	if string(data) != systemBundleB {
		t.Fatalf("wrong bytes: got %q", string(data))
	}
}

// TestLoadSystemCABundle_FallbackOnEmpty verifies that a present-but-empty
// first path is skipped in favour of the next non-empty one.
func TestLoadSystemCABundle_FallbackOnEmpty(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty-ca.crt")
	second := filepath.Join(dir, "real-ca.crt")
	if err := os.WriteFile(empty, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte(systemBundleB), 0644); err != nil {
		t.Fatal(err)
	}

	logger, _ := newObservedLogger(t)
	data, source := loadSystemCABundleFromPaths(logger, []string{empty, second})
	if source != second {
		t.Fatalf("expected empty first path to be skipped, got %q", source)
	}
	if !bytes.Equal(data, []byte(systemBundleB)) {
		t.Fatalf("wrong bytes: got %q", string(data))
	}
}

// TestLoadSystemCABundle_NoSources verifies that when no path is readable the
// function logs a warning and returns (nil, "") — no error.
func TestLoadSystemCABundle_NoSources(t *testing.T) {
	dir := t.TempDir()
	missing1 := filepath.Join(dir, "a.crt")
	missing2 := filepath.Join(dir, "b.crt")

	logger, logs := newObservedLogger(t)
	data, source := loadSystemCABundleFromPaths(logger, []string{missing1, missing2})
	if data != nil {
		t.Fatalf("expected nil data, got %d bytes", len(data))
	}
	if source != "" {
		t.Fatalf("expected empty source, got %q", source)
	}
	if logs.Len() != 1 {
		t.Fatalf("expected exactly 1 warning log, got %d", logs.Len())
	}
	entry := logs.All()[0]
	if entry.Level != zap.WarnLevel {
		t.Fatalf("expected warn level, got %s", entry.Level)
	}
	if !strings.Contains(entry.Message, "No system CA bundle found") {
		t.Fatalf("unexpected warn message: %q", entry.Message)
	}
}

// TestSetupSharedVolume_MergesSystemAndKeploy asserts that the output ca.crt
// contains both the system bundle and the embedded Keploy MITM CA concatenated
// (in that order, with an intervening newline if necessary), and that
// keploy-ca.crt contains only the Keploy CA.
func TestSetupSharedVolume_MergesSystemAndKeploy(t *testing.T) {
	exportDir := t.TempDir()

	// Stub the system-bundle loader with a system bundle that DOES have a
	// trailing newline already (the common case on Debian).
	orig := loadSystemCABundleFn
	defer func() { loadSystemCABundleFn = orig }()
	stubSource := "/stub/ca-certificates.crt"
	loadSystemCABundleFn = func(_ *zap.Logger) ([]byte, string) {
		return []byte(systemBundleA), stubSource
	}

	logger := zap.NewNop()
	if err := setupSharedVolume(nil, logger, exportDir); err != nil {
		t.Fatalf("setupSharedVolume: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(exportDir, "ca.crt"))
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	// The merged bundle must start with the system block and end with the
	// Keploy CA bytes (no reordering, no truncation).
	if !bytes.HasPrefix(got, []byte(systemBundleA)) {
		t.Fatalf("merged ca.crt does not start with system bundle; got prefix %q", string(got[:min(len(got), 80)]))
	}
	if !bytes.HasSuffix(got, caCrt) {
		t.Fatalf("merged ca.crt does not end with the Keploy CA; len(got)=%d len(caCrt)=%d", len(got), len(caCrt))
	}
	// The system block already has a trailing '\n'; the merger must NOT
	// insert a second one — otherwise tools that count exact boundaries
	// between PEM entries (rare but real) see an extra empty block.
	expected := []byte(systemBundleA)
	expected = append(expected, caCrt...)
	if !bytes.Equal(got, expected) {
		t.Fatalf("merged bundle differs from expected; len(got)=%d len(expected)=%d", len(got), len(expected))
	}

	// keploy-ca.crt must be exactly the embedded CA bytes (no system bundle
	// mixed in) — this is what NODE_EXTRA_CA_CERTS consumers read.
	keployOnly, err := os.ReadFile(filepath.Join(exportDir, "keploy-ca.crt"))
	if err != nil {
		t.Fatalf("read keploy-ca.crt: %v", err)
	}
	if !bytes.Equal(keployOnly, caCrt) {
		t.Fatalf("keploy-ca.crt must be keploy CA alone; len(got)=%d len(caCrt)=%d", len(keployOnly), len(caCrt))
	}

	// truststore.jks is generated from keploy-ca.crt — just confirm it was
	// created (full JKS decode is covered by generateTrustStore's own tests).
	jksInfo, err := os.Stat(filepath.Join(exportDir, "truststore.jks"))
	if err != nil {
		t.Fatalf("stat truststore.jks: %v", err)
	}
	if jksInfo.Size() == 0 {
		t.Fatal("truststore.jks is empty")
	}
}

// TestSetupSharedVolume_InsertsNewlineWhenSystemBundleLacksOne verifies that
// when the system bundle does NOT end in a newline, the merge inserts one so
// the BEGIN marker of the Keploy CA starts on a fresh line.
func TestSetupSharedVolume_InsertsNewlineWhenSystemBundleLacksOne(t *testing.T) {
	exportDir := t.TempDir()
	orig := loadSystemCABundleFn
	defer func() { loadSystemCABundleFn = orig }()

	// Drop the trailing newline — this simulates a hand-rolled bundle.
	noTrailing := strings.TrimRight(systemBundleA, "\n")
	loadSystemCABundleFn = func(_ *zap.Logger) ([]byte, string) {
		return []byte(noTrailing), "/stub/ca.crt"
	}

	logger := zap.NewNop()
	if err := setupSharedVolume(nil, logger, exportDir); err != nil {
		t.Fatalf("setupSharedVolume: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(exportDir, "ca.crt"))
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	// Expect: <noTrailing> + "\n" + <keploy CA>
	expected := append([]byte(noTrailing), '\n')
	expected = append(expected, caCrt...)
	if !bytes.Equal(got, expected) {
		t.Fatalf("missing inserted newline; len(got)=%d len(expected)=%d", len(got), len(expected))
	}
}

// TestSetupSharedVolume_FallsBackWhenSystemBundleMissing verifies the fallback
// path — when loadSystemCABundleFn returns (nil, ""), the output ca.crt is
// just the Keploy CA (matches legacy behaviour) and no leading newline is
// prepended.
func TestSetupSharedVolume_FallsBackWhenSystemBundleMissing(t *testing.T) {
	exportDir := t.TempDir()
	orig := loadSystemCABundleFn
	defer func() { loadSystemCABundleFn = orig }()
	loadSystemCABundleFn = func(_ *zap.Logger) ([]byte, string) { return nil, "" }

	logger := zap.NewNop()
	if err := setupSharedVolume(nil, logger, exportDir); err != nil {
		t.Fatalf("setupSharedVolume: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(exportDir, "ca.crt"))
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	if !bytes.Equal(got, caCrt) {
		t.Fatalf("fallback output should equal keploy CA alone; len(got)=%d len(caCrt)=%d", len(got), len(caCrt))
	}
}
