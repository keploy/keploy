package tls

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
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
	// The structured field is what alert rules will key off; verify it's set
	// and points at the right reason.
	var sawReason bool
	for _, f := range entry.Context {
		if f.Key == "severity_reason" {
			sawReason = true
			if !strings.Contains(f.String, "image looks distro-shaped") {
				t.Fatalf("severity_reason has wrong text: %q", f.String)
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
// certificates. If a future refresh of mozilla_roots.pem accidentally
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
	var sawReason bool
	for _, f := range entry.Context {
		if f.Key == "severity_reason" {
			sawReason = true
			if !strings.Contains(f.String, "no distro trust-store layout") {
				t.Fatalf("severity_reason has wrong text: %q", f.String)
			}
		}
	}
	if !sawReason {
		t.Fatalf("expected a severity_reason field on the INFO entry; got context=%+v", entry.Context)
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
	merged = append(merged, caCrt...)

	bundlePath := filepath.Join(dir, "merged.crt")
	if err := os.WriteFile(bundlePath, merged, 0644); err != nil {
		t.Fatalf("write merged: %v", err)
	}
	jksPath := filepath.Join(dir, "truststore.jks")

	if err := generateTrustStore(bundlePath, jksPath); err != nil {
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
	if err := generateTrustStore(empty, jks); err == nil {
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
	err := generateTrustStore(bundlePath, jksPath)
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
