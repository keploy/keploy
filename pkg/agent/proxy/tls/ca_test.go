package tls

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pavlo-v-chernykh/keystore-go/v4"
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
// assert on emitted log lines. The observer captures Info and above so tests
// can distinguish Info-level "no system bundle found" notices from higher-
// severity output.
func newObservedLogger(t *testing.T) (*zap.Logger, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.InfoLevel)
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

// withDistroLayout temporarily forces the loader's "is this a distro-shaped
// image?" probe to a fixed boolean. Restores the original on test cleanup.
// Used by the no-sources tests to assert ERROR-vs-INFO selection.
func withDistroLayout(t *testing.T, present bool) {
	t.Helper()
	orig := hasDistroTrustLayoutFn
	hasDistroTrustLayoutFn = func() bool { return present }
	t.Cleanup(func() { hasDistroTrustLayoutFn = orig })
}

// TestLoadSystemCABundle_NoSources_DistroImageErrors covers the regression
// captured in keploy/k8s-proxy#375: a Debian/Ubuntu/Alpine/RHEL-shaped agent
// image where the bundle should have been installed by the
// `ca-certificates` package but isn't readable at runtime (build regression
// or runtime overlay shadow). This is a real misconfiguration that causes
// CERTIFICATE_VERIFY_FAILED on every public-endpoint HTTPS call from
// any orphan-mutated app pod — log at ERROR so alert pipelines catch it.
// (We intentionally do NOT abort the agent: the proxy still works for
// redirected traffic; the loud log is the operator-facing signal.)
func TestLoadSystemCABundle_NoSources_DistroImageErrors(t *testing.T) {
	withDistroLayout(t, true)
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
		t.Fatalf("expected exactly 1 log entry, got %d", logs.Len())
	}
	entry := logs.All()[0]
	if entry.Level != zap.ErrorLevel {
		t.Fatalf("expected error level (image looks distro-shaped → missing bundle is a regression), got %s", entry.Level)
	}
	if !strings.Contains(entry.Message, "No system CA bundle found") {
		t.Fatalf("unexpected log message: %q", entry.Message)
	}
	// severity_reason is a stable enum string (alert rules key off it).
	// Assert exact equality so an accidental wording change in production
	// fails this test instead of slipping past — that's the whole reason
	// the field exists separately from the human-readable explanation.
	var sawReason bool
	for _, f := range entry.Context {
		if f.Key == "severity_reason" {
			sawReason = true
			if f.String != severityReasonDistroLayoutPresent {
				t.Fatalf("severity_reason: expected exact %q, got %q", severityReasonDistroLayoutPresent, f.String)
			}
		}
	}
	if !sawReason {
		t.Fatalf("expected a severity_reason field on the ERROR entry; got context=%+v", entry.Context)
	}
}

// TestLoadSystemCABundle_EmbeddedFallback_ReturnsBytes is the headline
// regression for keploy/k8s-proxy#375 defense-in-depth. When every disk
// path comes up empty, the production entry point must NOT return (nil, "")
// — it must return the embedded Mozilla NSS roots. Apps mutated by keploy
// then have a real trust store no matter what the agent image's filesystem
// looks like (broken build, weird volume mount, SELinux denial,
// distroless).
func TestLoadSystemCABundle_EmbeddedFallback_ReturnsBytes(t *testing.T) {
	// Force the disk path to fail by pointing the search at temp-dir paths
	// that don't exist. Pass embeddedFallbackRoots as the fallback (the
	// same blob the production loadSystemCABundle uses).
	dir := t.TempDir()
	missing1 := filepath.Join(dir, "a.crt")
	missing2 := filepath.Join(dir, "b.crt")

	logger, _ := newObservedLogger(t)
	data, source := loadSystemCABundleFromPathsAndFallback(logger, []string{missing1, missing2}, embeddedFallbackRoots)

	if len(data) == 0 {
		t.Fatalf("expected non-empty data from embedded fallback, got empty")
	}
	if !bytes.Equal(data, embeddedFallbackRoots) {
		t.Fatalf("expected returned bytes to equal embeddedFallbackRoots; got %d bytes vs %d bytes", len(data), len(embeddedFallbackRoots))
	}
	if source != systemCABundleSourceEmbedded {
		t.Fatalf("expected source=%q, got %q", systemCABundleSourceEmbedded, source)
	}
}

// TestLoadSystemCABundle_EmbeddedFallback_ContainsMozillaRoots is a
// build-time sanity check that the embedded blob actually parses as a
// PEM bundle and contains a non-trivial number of x509 root
// certificates. If a future bump of x509roots/fallback accidentally
// truncates or replaces the file with garbage, CI catches it here.
//
// We assert >=100 roots — Mozilla NSS typically ships ~140-150. Below
// 100 strongly indicates a bad refresh.
func TestLoadSystemCABundle_EmbeddedFallback_ContainsMozillaRoots(t *testing.T) {
	if len(embeddedFallbackRoots) == 0 {
		t.Fatalf("embeddedFallbackRoots is empty; the go:embed directive may have failed at build time")
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(embeddedFallbackRoots) {
		t.Fatalf("AppendCertsFromPEM rejected embeddedFallbackRoots — bundle is not valid PEM or contains no certs")
	}

	// Count BEGIN CERTIFICATE blocks as a cheap proxy for the cert count
	// (CertPool doesn't expose a count post-1.20). Mozilla's NSS bundle
	// has been ~140 since 2024; >=100 is a generous lower bound that
	// catches truncation and accidental empty-file commits without
	// being brittle when a few CAs are added/removed in a refresh.
	const minCerts = 100
	count := bytes.Count(embeddedFallbackRoots, []byte("-----BEGIN CERTIFICATE-----"))
	if count < minCerts {
		t.Fatalf("embeddedFallbackRoots has only %d cert(s); expected >=%d (Mozilla NSS bundle is typically ~140). Refresh may have produced a truncated file.", count, minCerts)
	}
}

// distrustedRootFingerprints are SHA-256 fingerprints (of the DER cert) of roots
// Mozilla has removed from NSS for cause. The embedded bundle is handed to
// recorded apps as their trust store when they have none of their own, so a
// root that is still in it stays trusted by those apps even after every browser
// has dropped it.
var distrustedRootFingerprints = map[string]string{
	// eMudhra intermediate key compromise (Mozilla bug 2083449, Oct 2026): the
	// eight compromised intermediates chain to these roots; NSS 3.131.1 removed them.
	"86a1ecba089c4a8d3bbe2734c612ba341d813e043cf9e8a862cd5c57a36bbe6b": "emSign ECC Root CA - G3",
	"125609aa301da0a249b97a8239cb6a34216f44dcac9f3954b14292f2e8c8608f": "emSign Root CA - C1",
	"bc4d809b15189d78db3e1d8cf4f9726a795da1643ca5f1358e1ddb0edc0d7eb3": "emSign ECC Root CA - C3",
}

// TestLoadSystemCABundle_EmbeddedFallback_ExcludesDistrustedRoots fails when the
// embedded Mozilla bundle still carries a root Mozilla has distrusted, i.e. when
// golang.org/x/crypto/x509roots/fallback was not bumped after a CA incident
// (or was downgraded).
func TestLoadSystemCABundle_EmbeddedFallback_ExcludesDistrustedRoots(t *testing.T) {
	rest := embeddedFallbackRoots
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		sum := sha256.Sum256(block.Bytes)
		if name, bad := distrustedRootFingerprints[fmt.Sprintf("%x", sum)]; bad {
			t.Errorf("embedded NSS bundle still contains distrusted root %q (sha256 %x); bump golang.org/x/crypto/x509roots/fallback", name, sum)
		}
	}
}

// TestLoadSystemCABundle_DiskTakesPrecedence verifies the embedded fallback
// is ONLY consulted after the disk search exhausts. When a disk path has
// content, the function must return that content (not the embedded blob).
// Otherwise operators who deliberately mount a custom CA store at one of
// the search paths would see their override silently replaced.
func TestLoadSystemCABundle_DiskTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "primary.crt")
	if err := os.WriteFile(primary, []byte(systemBundleA), 0644); err != nil {
		t.Fatal(err)
	}

	logger, _ := newObservedLogger(t)
	data, source := loadSystemCABundleFromPathsAndFallback(logger, []string{primary}, embeddedFallbackRoots)
	if string(data) != systemBundleA {
		t.Fatalf("disk path should win over embedded fallback; got %q", string(data[:min(len(data), 40)]))
	}
	if source != primary {
		t.Fatalf("expected source=%q, got %q", primary, source)
	}
}

// TestLoadSystemCABundle_NoSources_DistrolessImageStaysInfo verifies that a
// truly distroless / scratch agent image (no /etc/ssl/certs, no /etc/pki/tls)
// still logs at INFO. Distroless deployments deliberately ship without a
// trust store, and bumping severity there would create alert fatigue.
func TestLoadSystemCABundle_NoSources_DistrolessImageStaysInfo(t *testing.T) {
	withDistroLayout(t, false)
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
		t.Fatalf("expected exactly 1 log entry, got %d", logs.Len())
	}
	entry := logs.All()[0]
	if entry.Level != zap.InfoLevel {
		t.Fatalf("expected info level (image is distroless → missing bundle is expected), got %s", entry.Level)
	}
	if !strings.Contains(entry.Message, "No system CA bundle found") {
		t.Fatalf("unexpected log message: %q", entry.Message)
	}
	// Exact-equality assertion on the stable enum value — see the
	// matching block in TestLoadSystemCABundle_NoSources_DistroImageErrors
	// for the rationale.
	var sawReason bool
	for _, f := range entry.Context {
		if f.Key == "severity_reason" {
			sawReason = true
			if f.String != severityReasonNoDistroLayout {
				t.Fatalf("severity_reason: expected exact %q, got %q", severityReasonNoDistroLayout, f.String)
			}
		}
	}
	if !sawReason {
		t.Fatalf("expected a severity_reason field on the INFO entry; got context=%+v", entry.Context)
	}
}

// TestLoadSystemCABundle_NoSources_WithEmbeddedFallback_DistroErrors mirrors
// TestLoadSystemCABundle_NoSources_DistroImageErrors but exercises the
// PRODUCTION entry shape — loadSystemCABundleFromPathsAndFallback with a
// non-nil fallback — so that we cover the real outcome an operator sees
// when /etc/ssl/certs is missing on a distro-shaped image: the agent
// returns the embedded Mozilla NSS bytes and the source flips to the
// "<embedded:...>" sentinel, on top of the same ERROR severity as the
// disk-only test asserts. The disk-only twin remains so we keep the
// no-fallback behaviour pinned for any future caller that opts out of
// the embedded blob.
func TestLoadSystemCABundle_NoSources_WithEmbeddedFallback_DistroErrors(t *testing.T) {
	withDistroLayout(t, true)
	dir := t.TempDir()
	missing1 := filepath.Join(dir, "a.crt")
	missing2 := filepath.Join(dir, "b.crt")

	logger, logs := newObservedLogger(t)
	data, source := loadSystemCABundleFromPathsAndFallback(logger, []string{missing1, missing2}, embeddedFallbackRoots)

	if !bytes.Equal(data, embeddedFallbackRoots) {
		t.Fatalf("expected embedded fallback bytes when fallback is supplied; got %d bytes vs %d", len(data), len(embeddedFallbackRoots))
	}
	if source != systemCABundleSourceEmbedded {
		t.Fatalf("expected source=%q, got %q", systemCABundleSourceEmbedded, source)
	}
	if logs.Len() != 1 {
		t.Fatalf("expected exactly 1 log entry, got %d", logs.Len())
	}
	entry := logs.All()[0]
	if entry.Level != zap.ErrorLevel {
		t.Fatalf("expected ERROR (distro layout present), got %s", entry.Level)
	}
	if !strings.Contains(entry.Message, "No system CA bundle found") {
		t.Fatalf("unexpected log message: %q", entry.Message)
	}
	// The fallback-supplied branch's message must reference the embedded
	// roots — that's the operator-facing distinction between this branch
	// and the no-fallback one (see message-conditioning in
	// loadSystemCABundleFromPathsAndFallback). Asserting on the
	// "embedded Mozilla NSS roots" phrase pins the message body so a
	// future refactor can't silently revert to the misleading "falling
	// back to embedded" wording when fallback is empty.
	if !strings.Contains(entry.Message, "embedded Mozilla NSS roots") {
		t.Fatalf("expected message to mention embedded Mozilla NSS roots when fallback is supplied; got %q", entry.Message)
	}
	// severity_reason still pins to the distro_layout_present enum.
	var sawReason bool
	for _, f := range entry.Context {
		if f.Key == "severity_reason" {
			sawReason = true
			if f.String != severityReasonDistroLayoutPresent {
				t.Fatalf("severity_reason: expected exact %q, got %q", severityReasonDistroLayoutPresent, f.String)
			}
		}
	}
	if !sawReason {
		t.Fatalf("expected a severity_reason field on the ERROR entry; got context=%+v", entry.Context)
	}
	// embedded_fallback_bytes must report the supplied fallback length,
	// not zero — otherwise a future regression that drops the fallback
	// silently would still claim the production path was exercised.
	var sawBytes bool
	for _, f := range entry.Context {
		if f.Key == "embedded_fallback_bytes" {
			sawBytes = true
			if f.Integer != int64(len(embeddedFallbackRoots)) {
				t.Fatalf("embedded_fallback_bytes: got %d, want %d", f.Integer, len(embeddedFallbackRoots))
			}
		}
	}
	if !sawBytes {
		t.Fatalf("expected embedded_fallback_bytes field; got context=%+v", entry.Context)
	}
}

// TestLoadSystemCABundle_NoSources_WithEmbeddedFallback_DistrolessStaysInfo
// is the INFO twin of the test above — distroless image, no disk bundle,
// embedded fallback supplied. Production behaviour: log at INFO (no alert
// fatigue), return embedded bytes + the "<embedded:...>" source.
func TestLoadSystemCABundle_NoSources_WithEmbeddedFallback_DistrolessStaysInfo(t *testing.T) {
	withDistroLayout(t, false)
	dir := t.TempDir()
	missing1 := filepath.Join(dir, "a.crt")
	missing2 := filepath.Join(dir, "b.crt")

	logger, logs := newObservedLogger(t)
	data, source := loadSystemCABundleFromPathsAndFallback(logger, []string{missing1, missing2}, embeddedFallbackRoots)

	if !bytes.Equal(data, embeddedFallbackRoots) {
		t.Fatalf("expected embedded fallback bytes; got %d bytes vs %d", len(data), len(embeddedFallbackRoots))
	}
	if source != systemCABundleSourceEmbedded {
		t.Fatalf("expected source=%q, got %q", systemCABundleSourceEmbedded, source)
	}
	if logs.Len() != 1 {
		t.Fatalf("expected exactly 1 log entry, got %d", logs.Len())
	}
	entry := logs.All()[0]
	if entry.Level != zap.InfoLevel {
		t.Fatalf("expected INFO (distroless image), got %s", entry.Level)
	}
	if !strings.Contains(entry.Message, "embedded Mozilla NSS roots") {
		t.Fatalf("expected message to mention embedded Mozilla NSS roots when fallback is supplied; got %q", entry.Message)
	}
	var sawReason bool
	for _, f := range entry.Context {
		if f.Key == "severity_reason" {
			sawReason = true
			if f.String != severityReasonNoDistroLayout {
				t.Fatalf("severity_reason: expected exact %q, got %q", severityReasonNoDistroLayout, f.String)
			}
		}
	}
	if !sawReason {
		t.Fatalf("expected a severity_reason field on the INFO entry; got context=%+v", entry.Context)
	}
}

// TestLoadSystemCABundle_NoSources_NilFallback_DoesNotClaimFallback locks in
// the message-conditioning Copilot flagged: when fallback is nil (the
// disk-only entry point loadSystemCABundleFromPaths) the log line MUST NOT
// say it's falling back to the embedded Mozilla NSS roots — there is no
// fallback to fall back to. A regression here would mislead operators
// into thinking the agent self-healed when it didn't.
func TestLoadSystemCABundle_NoSources_NilFallback_DoesNotClaimFallback(t *testing.T) {
	withDistroLayout(t, true)
	dir := t.TempDir()
	missing := filepath.Join(dir, "a.crt")

	logger, logs := newObservedLogger(t)
	data, source := loadSystemCABundleFromPathsAndFallback(logger, []string{missing}, nil)
	if data != nil {
		t.Fatalf("expected nil data when fallback is nil; got %d bytes", len(data))
	}
	if source != "" {
		t.Fatalf("expected empty source when fallback is nil; got %q", source)
	}
	if logs.Len() != 1 {
		t.Fatalf("expected exactly 1 log entry, got %d", logs.Len())
	}
	entry := logs.All()[0]
	// Must NOT claim a fallback happened.
	if strings.Contains(entry.Message, "falling back to the agent's embedded Mozilla NSS roots") {
		t.Fatalf("nil-fallback path must not claim a fallback occurred; got %q", entry.Message)
	}
	// Must explicitly state the resulting bundle is incomplete so an
	// operator reading this line knows the trust store is broken.
	if !strings.Contains(entry.Message, "only the Keploy MITM CA") {
		t.Fatalf("nil-fallback path should call out the keploy-only outcome; got %q", entry.Message)
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
	if err := setupSharedVolume(nil, logger, exportDir, testCA); err != nil {
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
	if !bytes.HasSuffix(got, testCACertPEM()) {
		t.Fatalf("merged ca.crt does not end with the Keploy CA; len(got)=%d len(testCACertPEM())=%d", len(got), len(testCACertPEM()))
	}
	// The system block already has a trailing '\n'; the merger must NOT
	// insert a second one — otherwise tools that count exact boundaries
	// between PEM entries (rare but real) see an extra empty block.
	expected := []byte(systemBundleA)
	expected = append(expected, testCACertPEM()...)
	if !bytes.Equal(got, expected) {
		t.Fatalf("merged bundle differs from expected; len(got)=%d len(expected)=%d", len(got), len(expected))
	}

	// keploy-ca.crt must be exactly the embedded CA bytes (no system bundle
	// mixed in) — this is what NODE_EXTRA_CA_CERTS consumers read.
	keployOnly, err := os.ReadFile(filepath.Join(exportDir, "keploy-ca.crt"))
	if err != nil {
		t.Fatalf("read keploy-ca.crt: %v", err)
	}
	if !bytes.Equal(keployOnly, testCACertPEM()) {
		t.Fatalf("keploy-ca.crt must be keploy CA alone; len(got)=%d len(testCACertPEM())=%d", len(keployOnly), len(testCACertPEM()))
	}

	// truststore.jks is generated from the MERGED bundle (system + keploy).
	// Full JKS-contents assertions live in TestGenerateTrustStore_MergesAllCerts
	// (which uses a real self-signed cert rather than the fake PEM blocks used
	// here). This test just confirms the file is produced and non-empty.
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
	if err := setupSharedVolume(nil, logger, exportDir, testCA); err != nil {
		t.Fatalf("setupSharedVolume: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(exportDir, "ca.crt"))
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	// Expect: <noTrailing> + "\n" + <keploy CA>
	expected := append([]byte(noTrailing), '\n')
	expected = append(expected, testCACertPEM()...)
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
	if err := setupSharedVolume(nil, logger, exportDir, testCA); err != nil {
		t.Fatalf("setupSharedVolume: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(exportDir, "ca.crt"))
	if err != nil {
		t.Fatalf("read ca.crt: %v", err)
	}
	if !bytes.Equal(got, testCACertPEM()) {
		t.Fatalf("fallback output should equal keploy CA alone; len(got)=%d len(testCACertPEM())=%d", len(got), len(testCACertPEM()))
	}
}

// makeSelfSignedPEM returns a newly-generated, throwaway self-signed CA
// certificate encoded as a single PEM block. It's used by tests that
// exercise the real x509 parse path in generateTrustStore — unlike the
// fake-PEM constants at the top of this file, the output of this helper
// survives pem.Decode + x509.ParseCertificate.
func makeSelfSignedPEM(t *testing.T, cn string) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("rand.Int: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// TestGenerateTrustStore_MergesAllCerts is the real principal-review
// regression for the Java-JKS fix: it builds a bundle that contains a
// freshly-minted "system" CA PLUS the embedded Keploy MITM CA, runs
// generateTrustStore against that bundle, loads the resulting JKS back
// via keystore-go (so the test stresses the whole round-trip), and
// asserts that:
//   - both certificates are present as trusted entries;
//   - the Keploy CA is aliased "keploy-root" (stable alias);
//   - the system CA is aliased "system-<sha256-hex>" and matches the
//     DER of the synthetic cert we generated.
//
// Before this PR the function decoded only the FIRST PEM block, so on a
// merged bundle structured as `<system CA>\n<keploy CA>` every
// subsequent CERTIFICATE block (notably the Keploy MITM root itself)
// was silently dropped from the JKS — exactly the Java edge case
// principal review flagged.
func TestGenerateTrustStore_MergesAllCerts(t *testing.T) {
	dir := t.TempDir()

	systemCAPEM := makeSelfSignedPEM(t, "Test-System-Root")

	// Compose a merged bundle: <system CA>\n<keploy CA>.
	merged := append([]byte{}, systemCAPEM...)
	merged = append(merged, testCACertPEM()...)

	bundlePath := filepath.Join(dir, "merged.crt")
	if err := os.WriteFile(bundlePath, merged, 0644); err != nil {
		t.Fatalf("write merged: %v", err)
	}
	jksPath := filepath.Join(dir, "truststore.jks")

	if err := generateTrustStore(bundlePath, jksPath, testCADER()); err != nil {
		t.Fatalf("generateTrustStore: %v", err)
	}

	// Load the JKS back and inspect its entries.
	f, err := os.Open(jksPath)
	if err != nil {
		t.Fatalf("open jks: %v", err)
	}
	defer f.Close()

	ks := keystore.New()
	if err := ks.Load(f, []byte("changeit")); err != nil {
		t.Fatalf("ks.Load: %v", err)
	}

	aliases := ks.Aliases()
	if len(aliases) < 2 {
		t.Fatalf("expected >=2 aliases, got %d: %v", len(aliases), aliases)
	}

	// Derive the expected system alias from the synthetic cert's DER.
	sysBlock, _ := pem.Decode(systemCAPEM)
	if sysBlock == nil {
		t.Fatal("could not re-decode synthetic system PEM")
	}
	sysSHA := sha256.Sum256(sysBlock.Bytes)
	expectedSysAlias := fmt.Sprintf("system-%x", sysSHA)

	// The JKS aliases are normalised to lowercase by keystore-go.
	// We already produce lowercase hex via %x; the "keploy-root"
	// literal is ASCII lowercase. So a direct set-containment check
	// is safe.
	have := map[string]bool{}
	for _, a := range aliases {
		have[a] = true
	}

	if !have["keploy-root"] {
		t.Fatalf("JKS missing 'keploy-root' alias; have=%v", aliases)
	}
	if !have[expectedSysAlias] {
		t.Fatalf("JKS missing synthetic system-root alias %q; have=%v", expectedSysAlias, aliases)
	}

	// Spot-check the system entry round-trips byte-for-byte so a
	// future refactor can't regress to "writes the entry but with
	// wrong DER".
	entry, err := ks.GetTrustedCertificateEntry(expectedSysAlias)
	if err != nil {
		t.Fatalf("GetTrustedCertificateEntry(%s): %v", expectedSysAlias, err)
	}
	if !bytes.Equal(entry.Certificate.Content, sysBlock.Bytes) {
		t.Fatalf("system entry DER mismatch: len(got)=%d len(want)=%d",
			len(entry.Certificate.Content), len(sysBlock.Bytes))
	}
}

// TestSetEnvForSharedVolume_SplitsNodeFromReplaceVars locks in the contract
// that NODE_EXTRA_CA_CERTS gets the Keploy-only bundle (it is ADDED to the
// default Node trust store) while the REPLACE-style env vars
// (REQUESTS_CA_BUNDLE / SSL_CERT_FILE / CARGO_HTTP_CAINFO) get the MERGED
// bundle so system roots stay trusted.
//
// A regression here would manifest as: Node workloads double-trust system
// roots (harmless but wasteful) OR Python/libcurl/Rust workloads lose trust
// in public roots (broken).
func TestSetEnvForSharedVolume_SplitsNodeFromReplaceVars(t *testing.T) {
	const (
		merged = "/tmp/keploy-tls/ca.crt"
		keploy = "/tmp/keploy-tls/keploy-ca.crt"
	)

	// Snapshot + restore the env vars this test mutates so parallel / follow-up
	// tests in the same binary don't see leaked state.
	keys := []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"}
	prev := make(map[string]string, len(keys))
	prevSet := make(map[string]bool, len(keys))
	for _, k := range keys {
		v, ok := os.LookupEnv(k)
		prev[k] = v
		prevSet[k] = ok
		_ = os.Unsetenv(k)
	}
	defer func() {
		for _, k := range keys {
			if prevSet[k] {
				_ = os.Setenv(k, prev[k])
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}()

	if err := setEnvForSharedVolume(zap.NewNop(), merged, keploy); err != nil {
		t.Fatalf("setEnvForSharedVolume: %v", err)
	}
	if got := os.Getenv("NODE_EXTRA_CA_CERTS"); got != keploy {
		t.Fatalf("NODE_EXTRA_CA_CERTS: got %q, want %q (keploy-only)", got, keploy)
	}
	for _, k := range []string{"REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"} {
		if got := os.Getenv(k); got != merged {
			t.Fatalf("%s: got %q, want %q (merged bundle)", k, got, merged)
		}
	}
}

// TestGenerateTrustStore_RejectsEmptyBundle guards against a silently-empty
// JKS: if no CERTIFICATE block was parseable the function MUST error rather
// than write a zero-entry keystore.
func TestGenerateTrustStore_RejectsEmptyBundle(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.crt")
	if err := os.WriteFile(empty, []byte("no PEM here\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	jks := filepath.Join(dir, "out.jks")
	if err := generateTrustStore(empty, jks, nil); err == nil {
		t.Fatal("expected generateTrustStore to error on a bundle with no CERTIFICATE blocks")
	}
}

// TestGenerateTrustStore_RejectsTruncatedTrailingBlock proves the
// partial-JKS defense Copilot flagged: a bundle with ONE valid
// certificate followed by a truncated/corrupted second CERTIFICATE
// block (missing its -----END----- armour) must fail loudly, not
// silently drop the truncated block. Previously pem.Decode's
// nil-on-malformed return would have let the loop exit cleanly with a
// single-entry JKS, obscuring the fact that a trust anchor is missing.
func TestGenerateTrustStore_RejectsTruncatedTrailingBlock(t *testing.T) {
	dir := t.TempDir()

	validPEM := makeSelfSignedPEM(t, "Valid-Root")

	// Synthesise a trailing armour without its END marker. Using a
	// literal prefix keeps the test readable; the exact bytes after
	// -----BEGIN don't matter because pem.Decode rejects it before we
	// can parse anything meaningful.
	truncated := []byte("\n-----BEGIN CERTIFICATE-----\nMIIBizCCATGgAwIBAgIRAJqOxytruncat\n")

	bundle := append([]byte{}, validPEM...)
	bundle = append(bundle, truncated...)

	bundlePath := filepath.Join(dir, "truncated-trailer.crt")
	if err := os.WriteFile(bundlePath, bundle, 0644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	jksPath := filepath.Join(dir, "truncated.jks")
	err := generateTrustStore(bundlePath, jksPath, nil)
	if err == nil {
		t.Fatal("expected generateTrustStore to reject a bundle with a malformed trailing -----BEGIN armour, got nil")
	}
	if !strings.Contains(err.Error(), "malformed PEM armour") {
		t.Fatalf("error should call out the truncated trailer, got %q", err.Error())
	}
	if _, statErr := os.Stat(jksPath); statErr == nil {
		t.Fatal("truststore file should not exist when generateTrustStore errored — leaving a partial JKS would defeat the guard")
	}
}

// An install the stop cuts short is still removed by the teardown. The stop
// cancels the context the install runs under, which kills the trust store's
// update (update-ca-certificates) once this run's anchor is already written;
// any other failure of the update leaves the anchor written just the same.
// The install recorded what it wrote only once the update had succeeded, so
// TeardownNativeCA removed nothing and the anchor stayed trusted -- for a
// run whose CA key is gone -- until the next start's sweep.
func TestTeardownRemovesAnInstallThatDidNotFinish(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the trust-store anchors are Linux's")
	}
	store, bin := t.TempDir(), t.TempDir()
	update := filepath.Join(bin, "keploy-test-update-ca")
	// The update the stop killed: it ends in failure.
	if err := os.WriteFile(update, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Nothing else on PATH: no JDK keystore is touched.
	t.Setenv("PATH", bin)
	t.Setenv("JAVA_HOME", "")
	oldPaths, oldCmds := caStorePath, caStoreUpdateCmd
	caStorePath, caStoreUpdateCmd = []string{store + "/"}, []string{"keploy-test-update-ca"}
	t.Cleanup(func() {
		caStorePath, caStoreUpdateCmd = oldPaths, oldCmds
		recordNativeInstall(nil)
	})

	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := setupNativeForApp(context.Background(), zap.NewNop(), ca, nil, 0, ""); err == nil {
		t.Fatal("the install succeeded with a trust-store update that failed")
	}
	anchor := filepath.Join(store, runCertFileName(ca))
	if _, err := os.Stat(anchor); err != nil {
		t.Fatalf("the install did not write this run's anchor before its update: %v", err)
	}

	TeardownNativeCA(zap.NewNop())
	if _, err := os.Stat(anchor); !os.IsNotExist(err) {
		t.Fatalf("the teardown left this run's anchor %s in the trust store (stat: %v)", anchor, err)
	}
}

// TestCertForClient_NoSNINamesDestinationIP: a client that dialled an IP literal
// sends no SNI, so the leaf must carry the destination IP as an IP SAN (Go
// checks it: "cannot validate certificate for 10.96.0.1 because it doesn't
// contain any IP SANs"). An SNI still names the leaf, and so does a CONNECT
// target when there is no SNI. SrcPortToDstURL, still filed for readers
// outside this module, holds what this handshake was asked for, "" included.
//
// Every leaf here is minted for one client end, in an order where an earlier
// one's name could stand in for a later one's: the name comes from what each
// handshake is handed, never from what an earlier one on the same end asked
// for. It used to: the SNI was filed under the client's source port and a
// ClientHello without one took it, so after "api.example.com" the IP literal
// got a leaf for "api.example.com" (the CI red of
// TestHandleConnectionNamesTheLeafForAnIPLiteralDestinationOnReplay).
func TestCertForClient_NoSNINamesDestinationIP(t *testing.T) {
	resetCertCacheForTest()
	logger := zap.NewNop()
	caKey, caCert := helperCA(t)
	hello, cleanup, err := helperClientHello("")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	port := hello.Conn.RemoteAddr().(*net.TCPAddr).Port
	defer SrcPortToDstURL.Delete(port)
	filed := func() any { v, _ := SrcPortToDstURL.Load(port); return v }

	leafFor := func(sni, connectTarget string, dstIP net.IP) *x509.Certificate {
		t.Helper()
		hello.ServerName = sni
		cert, err := certForClient(logger, hello, caKey, caCert, time.Time{}, connectTarget, dstIP)
		if err != nil {
			t.Fatalf("certForClient(%q, %q, %v): %v", sni, connectTarget, dstIP, err)
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return leaf
	}

	leaf := leafFor("api.example.com", "", net.ParseIP("10.96.0.1"))
	if err := leaf.VerifyHostname("api.example.com"); err != nil {
		t.Errorf("SNI set: leaf does not name it: %v", err)
	}
	if len(leaf.IPAddresses) != 0 {
		t.Errorf("SNI set: leaf has IP SANs %v, want the SNI alone", leaf.IPAddresses)
	}

	for _, ip := range []string{"10.96.0.1", "fd00::1"} {
		leaf := leafFor("", "", net.ParseIP(ip))
		if err := leaf.VerifyHostname(ip); err != nil {
			t.Errorf("no SNI, destination %s, after a leaf for an SNI on the same client end: leaf CN %q does not name it: %v (IP SANs %v)", ip, leaf.Subject.CommonName, err, leaf.IPAddresses)
		}
		if v := filed(); v != "" {
			t.Errorf("no SNI, destination %s: SrcPortToDstURL = %q, want \"\": the client asked for no name, and the IP is none", ip, v)
		}
	}

	leaf = leafFor("", "proxied.example.com", net.ParseIP("10.96.0.1"))
	if err := leaf.VerifyHostname("proxied.example.com"); err != nil {
		t.Errorf("no SNI, CONNECT target set: leaf does not name it: %v", err)
	}
	if v := filed(); v != "proxied.example.com" {
		t.Errorf("no SNI, CONNECT target set: SrcPortToDstURL = %q, want the target", v)
	}
	leaf = leafFor("api.example.com", "proxied.example.com", nil)
	if err := leaf.VerifyHostname("api.example.com"); err != nil {
		t.Errorf("SNI and CONNECT target set: leaf does not name the SNI: %v", err)
	}
	if v := filed(); v != "api.example.com" {
		t.Errorf("SNI and CONNECT target set: SrcPortToDstURL = %q, want the SNI", v)
	}

	leaf = leafFor("", "", nil)
	if leaf.Subject.CommonName != "" || len(leaf.IPAddresses) != 0 {
		t.Errorf("no SNI, no CONNECT target, no destination: leaf CN %q, IP SANs %v, want neither (as before)", leaf.Subject.CommonName, leaf.IPAddresses)
	}
}

// caTrustTempDir points os.TempDir, and so SetupCaCertEnv's files, at a
// directory of the test's own (it returns this user's private dir in it, where
// they go), and has every variable SetupCaCertEnv sets
// restored when the test ends (t.Setenv), so no later test or child is left
// pointed at a file in a temp dir that is gone.
func caTrustTempDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("os.TempDir does not honour TMPDIR on Windows")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	for _, k := range append([]string{EnvJavaToolOptions}, caTrustEnvVars...) {
		t.Setenv(k, "")
	}
	t.Cleanup(func() { _ = RemoveCaCertEnvFiles() })
	return privateDirIn(t, dir)
}

func globIn(t *testing.T, dir, pattern string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestRemoveCaCertEnvFiles_RemovesEveryTrustFileAndPutsTheEnvBack: the app's
// trust file stays for as long as the run, every one this process wrote goes
// when the run is done, and the variables that named them are as they were
// before keploy set them.
func TestRemoveCaCertEnvFiles_RemovesEveryTrustFileAndPutsTheEnvBack(t *testing.T) {
	dir := caTrustTempDir(t)
	orig := loadSystemCABundleFn
	t.Cleanup(func() { loadSystemCABundleFn = orig })
	loadSystemCABundleFn = func(*zap.Logger) ([]byte, string) { return testCACertPEM(), "test" }

	// The user's own bundle for one variable, nothing for another.
	t.Setenv("SSL_CERT_FILE", "/etc/the-users-bundle.pem")
	if err := os.Unsetenv("NODE_EXTRA_CA_CERTS"); err != nil {
		t.Fatal(err)
	}

	if err := SetupCaCertEnv(zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatalf("SetupCaCertEnv: %v", err)
	}
	first := os.Getenv("SSL_CERT_FILE")
	// An agent set up again in the same process: an app started before may
	// still read the first file, so it stays until the run is done.
	if err := SetupCaCertEnv(zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatalf("second SetupCaCertEnv: %v", err)
	}
	second := os.Getenv("SSL_CERT_FILE")
	if first == second || filepath.Dir(first) != dir || filepath.Dir(second) != dir {
		t.Fatalf("want two trust files in %s, have %q and %q", dir, first, second)
	}
	if left := globIn(t, dir, "keploy-ca-*.crt"); len(left) != 2 {
		t.Fatalf("want both trust files while the run is live, have %v", left)
	}
	for _, k := range caTrustEnvVars {
		if got := os.Getenv(k); got != second {
			t.Fatalf("%s = %q while the run is live, want the trust file %q", k, got, second)
		}
	}
	if left := globIn(t, dir, "keploy-java-cas-*.pem"); len(left) != 0 {
		t.Errorf("the merged Java PEM outlived the truststore it was built for: %v", left)
	}
	// Set by someone else since keploy set it: not keploy's to put back.
	t.Setenv("CARGO_HTTP_CAINFO", "/etc/set-since.pem")

	if err := RemoveCaCertEnvFiles(); err != nil {
		t.Fatalf("RemoveCaCertEnvFiles: %v", err)
	}
	if left := globIn(t, dir, "keploy-ca-*.crt"); len(left) != 0 {
		t.Errorf("the app's CA trust files outlived the run: %v", left)
	}
	if got := os.Getenv("SSL_CERT_FILE"); got != "/etc/the-users-bundle.pem" {
		t.Errorf("SSL_CERT_FILE = %q after the run, want the user's own bundle back", got)
	}
	if v, ok := os.LookupEnv("NODE_EXTRA_CA_CERTS"); ok {
		t.Errorf("NODE_EXTRA_CA_CERTS = %q after the run, want it unset as it was", v)
	}
	if got := os.Getenv("REQUESTS_CA_BUNDLE"); got != "" {
		t.Errorf("REQUESTS_CA_BUNDLE = %q after the run, want it empty as it was", got)
	}
	if got := os.Getenv("CARGO_HTTP_CAINFO"); got != "/etc/set-since.pem" {
		t.Errorf("CARGO_HTTP_CAINFO = %q, want the value set since left alone", got)
	}

	// Again, and with nothing set up: nothing to do, and no error.
	if err := RemoveCaCertEnvFiles(); err != nil {
		t.Errorf("a second RemoveCaCertEnvFiles: %v", err)
	}
}

// TestRemoveCaCertEnvFiles_ToleratesAFileAlreadyGone: a temp cleaner may have
// taken the file first; that is not a failure to remove it.
func TestRemoveCaCertEnvFiles_ToleratesAFileAlreadyGone(t *testing.T) {
	dir := caTrustTempDir(t)
	if err := SetupCaCertEnv(zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatalf("SetupCaCertEnv: %v", err)
	}
	path := os.Getenv("SSL_CERT_FILE")
	if filepath.Dir(path) != dir {
		t.Fatalf("the trust file %q is not in the test's temp dir %s", path, dir)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCaCertEnvFiles(); err != nil {
		t.Errorf("RemoveCaCertEnvFiles: %v", err)
	}
	if got := os.Getenv("SSL_CERT_FILE"); got != "" {
		t.Errorf("SSL_CERT_FILE = %q, want it put back to empty", got)
	}
}
