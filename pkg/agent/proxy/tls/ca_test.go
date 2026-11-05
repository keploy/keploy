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
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
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
	// Small dummy PEM blocks for the tests that stub the loader and verify
	// byte-level merge behaviour, not x509 parsing. The loader itself keeps
	// only certificates x509 parses, so its tests use makeSelfSignedPEM.
	systemBundleA = "-----BEGIN CERTIFICATE-----\nSYSTEM-A\n-----END CERTIFICATE-----\n"
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
	systemA := makeSelfSignedPEM(t, "system-a")
	systemB := makeSelfSignedPEM(t, "system-b")
	dir := t.TempDir()
	first := filepath.Join(dir, "first-ca.crt")
	second := filepath.Join(dir, "second-ca.crt")
	if err := os.WriteFile(first, systemA, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, systemB, 0644); err != nil {
		t.Fatal(err)
	}

	logger, _ := newObservedLogger(t)
	data, source := loadSystemCABundleFromPaths(logger, []string{first, second})
	if source != first {
		t.Fatalf("expected first path to win, got %q", source)
	}
	if !bytes.Equal(data, systemA) {
		t.Fatalf("wrong bytes: got %q", string(data))
	}
}

// TestLoadSystemCABundle_FallbackOnMissing verifies that a missing first path
// is skipped and the next readable path wins.
func TestLoadSystemCABundle_FallbackOnMissing(t *testing.T) {
	systemB := makeSelfSignedPEM(t, "system-b")
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.crt")
	second := filepath.Join(dir, "real-ca.crt")
	if err := os.WriteFile(second, systemB, 0644); err != nil {
		t.Fatal(err)
	}

	logger, _ := newObservedLogger(t)
	data, source := loadSystemCABundleFromPaths(logger, []string{missing, second})
	if source != second {
		t.Fatalf("expected fallback to second path, got %q", source)
	}
	if !bytes.Equal(data, systemB) {
		t.Fatalf("wrong bytes: got %q", string(data))
	}
}

// TestLoadSystemCABundle_FallbackOnEmpty verifies that a present-but-empty
// first path is skipped in favour of the next non-empty one.
func TestLoadSystemCABundle_FallbackOnEmpty(t *testing.T) {
	systemB := makeSelfSignedPEM(t, "system-b")
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty-ca.crt")
	second := filepath.Join(dir, "real-ca.crt")
	if err := os.WriteFile(empty, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, systemB, 0644); err != nil {
		t.Fatal(err)
	}

	logger, _ := newObservedLogger(t)
	data, source := loadSystemCABundleFromPaths(logger, []string{empty, second})
	if source != second {
		t.Fatalf("expected empty first path to be skipped, got %q", source)
	}
	if !bytes.Equal(data, systemB) {
		t.Fatalf("wrong bytes: got %q", string(data))
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
	data, source := loadCABundle(logger, certEnv{}, []string{missing1, missing2}, embeddedFallbackRoots)

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
	systemA := makeSelfSignedPEM(t, "system-a")
	dir := t.TempDir()
	primary := filepath.Join(dir, "primary.crt")
	if err := os.WriteFile(primary, systemA, 0644); err != nil {
		t.Fatal(err)
	}

	logger, _ := newObservedLogger(t)
	data, source := loadCABundle(logger, certEnv{}, []string{primary}, embeddedFallbackRoots)
	if !bytes.Equal(data, systemA) {
		t.Fatalf("disk path should win over embedded fallback; got %q", string(data[:min(len(data), 40)]))
	}
	if source != primary {
		t.Fatalf("expected source=%q, got %q", primary, source)
	}
}

// An image that ships no CA bundle (the keploy enterprise agent image, a
// distroless one, a k8s sidecar whose /etc/ssl/certs is an emptyDir) uses the
// embedded Mozilla roots as a matter of course. That is not a fault, so it must
// log nothing at Info or above, and nothing it logs may tell the operator to
// install `ca-certificates` in keploy's image: that package is the MPL-2.0
// license the enterprise image dropped it for, and the embedded roots already
// give the apps a full trust bundle. The one pointer it keeps, at Debug, is how
// to add a corporate CA.
func TestLoadSystemCABundle_NoSources_WithEmbeddedFallback_IsQuiet(t *testing.T) {
	dir := t.TempDir()
	missing := []string{filepath.Join(dir, "a.crt"), filepath.Join(dir, "b.crt")}

	core, logs := observer.New(zap.DebugLevel)
	data, source := loadCABundle(zap.New(core), certEnv{}, missing, embeddedFallbackRoots)

	if !bytes.Equal(data, embeddedFallbackRoots) || source != systemCABundleSourceEmbedded {
		t.Fatalf("expected the embedded roots; got %d bytes from %q", len(data), source)
	}
	if n := logs.Filter(func(e observer.LoggedEntry) bool { return e.Level >= zap.InfoLevel }).Len(); n != 0 {
		t.Fatalf("the embedded-roots path is the normal one for an image without a bundle and must log nothing at Info or above; got %v", logs.All())
	}
	assertNoCACertificatesAdvice(t, logs)
	entries := logs.FilterMessageSnippet("Mozilla root list built into keploy").All()
	if len(entries) != 1 {
		t.Fatalf("expected one Debug entry naming the embedded roots; got %v", logs.All())
	}
	// The hint names SSL_CERT_FILE with a bundle that holds the public roots:
	// it is read the same way by the apps' bundle and by keploy's own TLS.
	// SSL_CERT_DIR is not: with no bundle file, keploy's own TLS would trust
	// that directory alone.
	hint, _ := entries[0].ContextMap()["corporate_ca"].(string)
	if !strings.Contains(hint, "SSL_CERT_FILE") || !strings.Contains(hint, "public roots") {
		t.Fatalf("the corporate_ca hint must point at SSL_CERT_FILE and a bundle holding the public roots; got %q", hint)
	}
	if strings.Contains(hint, "SSL_CERT_DIR") {
		t.Fatalf("the corporate_ca hint must not offer SSL_CERT_DIR, which keploy's own TLS does not add to the built-in roots; got %q", hint)
	}
}

// assertNoCACertificatesAdvice fails if any logged message or field tells the
// operator to install a CA package in keploy's image.
func assertNoCACertificatesAdvice(t *testing.T, logs *observer.ObservedLogs) {
	t.Helper()
	for _, e := range logs.All() {
		texts := []string{e.Message}
		for _, v := range e.ContextMap() {
			if s, ok := v.(string); ok {
				texts = append(texts, s)
			}
		}
		for _, s := range texts {
			if strings.Contains(s, "ca-certificates") || strings.Contains(s, "apt-get") || strings.Contains(s, "apk add") {
				t.Fatalf("log tells the operator to install a CA package in keploy's image: %q", s)
			}
		}
	}
}

// TestLoadSystemCABundle_NoSources_NilFallback_DoesNotClaimFallback locks in
// the message-conditioning Copilot flagged: when fallback is nil (the
// disk-only entry point loadSystemCABundleFromPaths) the log line MUST NOT
// say it's falling back to the embedded Mozilla NSS roots — there is no
// fallback to fall back to. A regression here would mislead operators
// into thinking the agent self-healed when it didn't.
func TestLoadSystemCABundle_NoSources_NilFallback_DoesNotClaimFallback(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "a.crt")

	logger, logs := newObservedLogger(t)
	data, source := loadCABundle(logger, certEnv{}, []string{missing}, nil)
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
	if strings.Contains(entry.Message, "built into keploy") {
		t.Fatalf("nil-fallback path must not claim a fallback occurred; got %q", entry.Message)
	}
	// Must explicitly state the resulting bundle is incomplete so an
	// operator reading this line knows the trust store is broken.
	if !strings.Contains(entry.Message, "only the Keploy MITM CA") {
		t.Fatalf("nil-fallback path should call out the keploy-only outcome; got %q", entry.Message)
	}
}

// writePEMFile writes the concatenated PEM blocks to a new file under dir.
func writePEMFile(t *testing.T, dir, name string, pems ...[]byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, bytes.Join(pems, nil), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

// pemCertCount is how many distinct certificates a bundle holds.
func pemCertCount(data []byte) int {
	seen := map[[32]byte]bool{}
	forEachPEMCertificate(data, func(der []byte) { seen[sha256.Sum256(der)] = true })
	return len(seen)
}

// An operator who mounts a corporate bundle into the agent and names it with
// SSL_CERT_FILE must see the apps trust it. It replaces the default bundle
// paths, as it does for crypto/x509 and OpenSSL: the bundle on disk is not
// merged in, and neither are the embedded roots.
//
// FAILS BEFORE THE FIX: the loader searched only the fixed paths, so the
// operator's bundle never reached the merged ca.crt.
func TestLoadCABundle_SSLCertFile_ReplacesTheBundlePaths(t *testing.T) {
	dir := t.TempDir()
	corp := makeSelfSignedPEM(t, "corp-root")
	envFile := writePEMFile(t, dir, "corp.pem", corp)
	disk := writePEMFile(t, dir, "disk.crt", makeSelfSignedPEM(t, "disk-root"))

	for _, tc := range []struct {
		name     string
		paths    []string
		fallback []byte
	}{
		{"a bundle on disk", []string{disk}, embeddedFallbackRoots},
		{"no bundle on disk (embedded roots otherwise)", []string{filepath.Join(dir, "none.crt")}, embeddedFallbackRoots},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			data, source := loadCABundle(zap.New(core), certEnv{file: envFile}, tc.paths, tc.fallback)
			if !bytes.Equal(data, corp) {
				t.Fatalf("expected exactly the SSL_CERT_FILE bundle; got %d bytes (%d certs)", len(data), pemCertCount(data))
			}
			if source != envFile {
				t.Fatalf("source = %q, want %q", source, envFile)
			}
			if n := logs.Filter(func(e observer.LoggedEntry) bool { return e.Level >= zap.InfoLevel }).Len(); n != 0 {
				t.Fatalf("a usable SSL_CERT_FILE is the normal case and must not be reported; got %v", logs.All())
			}
		})
	}
}

// A file whose content is exactly the embedded list (what the enterprise agent
// writes and points SSL_CERT_FILE at on an image with no bundle) is taken as
// is: still the Mozilla roots, and no warning.
func TestLoadCABundle_SSLCertFile_HoldingTheEmbeddedList(t *testing.T) {
	dir := t.TempDir()
	envFile := writePEMFile(t, dir, "keploy-trust-roots.pem", embeddedFallbackRoots)

	core, logs := observer.New(zap.DebugLevel)
	data, _ := loadCABundle(zap.New(core), certEnv{file: envFile}, []string{filepath.Join(dir, "none.crt")}, embeddedFallbackRoots)
	if !bytes.Equal(data, embeddedFallbackRoots) {
		t.Fatalf("expected the embedded list back; got %d bytes", len(data))
	}
	if n := logs.Filter(func(e observer.LoggedEntry) bool { return e.Level >= zap.InfoLevel }).Len(); n != 0 {
		t.Fatalf("expected no Info+ logs; got %v", logs.All())
	}
}

// An SSL_CERT_FILE that names nothing usable (a missing mount, an empty file,
// a key instead of a certificate) is a real misconfiguration — the operator's
// CA is not in the apps' trust bundle — so it is a Warn with a next step. The
// apps still get a full bundle from the disk paths or the embedded roots,
// never the Keploy MITM CA alone (keploy/k8s-proxy#375).
func TestLoadCABundle_SSLCertFile_Unusable_WarnsAndFallsBack(t *testing.T) {
	dir := t.TempDir()
	disk := writePEMFile(t, dir, "disk.crt", makeSelfSignedPEM(t, "disk-root"))
	empty := writePEMFile(t, dir, "empty.pem")
	key := writePEMFile(t, dir, "key.pem", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("junk")}))

	for _, tc := range []struct {
		name       string
		envFile    string
		paths      []string
		want       []byte
		wantSource string
	}{
		{"missing, disk bundle present", filepath.Join(dir, "missing.pem"), []string{disk}, mustRead(t, disk), disk},
		{"empty, no disk bundle", empty, []string{filepath.Join(dir, "none.crt")}, embeddedFallbackRoots, systemCABundleSourceEmbedded},
		{"no certificate, no disk bundle", key, []string{filepath.Join(dir, "none.crt")}, embeddedFallbackRoots, systemCABundleSourceEmbedded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			data, source := loadCABundle(zap.New(core), certEnv{file: tc.envFile}, tc.paths, embeddedFallbackRoots)
			if !bytes.Equal(data, tc.want) || source != tc.wantSource {
				t.Fatalf("got %d bytes from %q; want %d bytes from %q", len(data), source, len(tc.want), tc.wantSource)
			}
			warns := logs.FilterLevelExact(zap.WarnLevel).All()
			if len(warns) != 1 {
				t.Fatalf("expected one Warn naming SSL_CERT_FILE; got %v", logs.All())
			}
			fields := warns[0].ContextMap()
			if fields["ssl_cert_file"] != tc.envFile {
				t.Fatalf("the Warn must name the file; got %v", fields)
			}
			if next, _ := fields["next_step"].(string); !strings.Contains(next, "SSL_CERT_FILE") {
				t.Fatalf("the Warn must say what to do; next_step=%q", next)
			}
			assertNoCACertificatesAdvice(t, logs)
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// SSL_CERT_DIR adds the certificates in its directories (':'-separated) to
// the bundle file, as crypto/x509 does — on an image with no bundle file, to
// the embedded roots, so the apps keep the public roots. A directory such as
// /etc/ssl/certs holds a root several times (bundle, per-root file, hash
// link), so the result holds each certificate once.
func TestLoadCABundle_SSLCertDir_AddsDeduplicatedCertificates(t *testing.T) {
	base := t.TempDir()
	corpA := makeSelfSignedPEM(t, "corp-root-a")
	corpB := makeSelfSignedPEM(t, "corp-root-b")
	diskRoot := makeSelfSignedPEM(t, "disk-root")
	disk := writePEMFile(t, base, "disk.crt", diskRoot)

	dir1 := filepath.Join(base, "d1")
	dir2 := filepath.Join(base, "d2")
	for _, d := range []string{dir1, dir2, filepath.Join(dir1, "sub")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writePEMFile(t, dir1, "corp-a.pem", corpA)
	writePEMFile(t, dir1, "bundle.pem", corpA, corpB) // corpA again
	writePEMFile(t, dir1, "disk-again.pem", diskRoot) // already in the bundle file
	writePEMFile(t, dir1, "notes.txt", []byte("not a certificate\n"))
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(dir1, "corp-a.pem"), filepath.Join(dir1, "abcd1234.0")); err != nil {
			t.Fatal(err)
		}
	}
	writePEMFile(t, dir2, "corp-b.pem", corpB)

	t.Run("with a bundle file", func(t *testing.T) {
		logger, logs := newObservedLogger(t)
		sep := string(filepath.ListSeparator)
		dirs := dir1 + sep + filepath.Join(base, "missing") + sep + sep + dir2
		data, source := loadCABundle(logger, certEnv{dir: dirs}, []string{disk}, embeddedFallbackRoots)
		if !bytes.HasPrefix(data, diskRoot) {
			t.Fatal("the bundle file must come first, unchanged")
		}
		if got := pemCertCount(data); got != 3 || bytes.Count(data, []byte("BEGIN CERTIFICATE")) != 3 {
			t.Fatalf("expected disk + corp-a + corp-b, each once; got %d distinct in %d blocks", got, bytes.Count(data, []byte("BEGIN CERTIFICATE")))
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) || !verifiesAgainst(t, corpA, pool) || !verifiesAgainst(t, corpB, pool) {
			t.Fatal("the SSL_CERT_DIR certificates are not trusted by the merged bundle")
		}
		// dir2's only certificate (corp-b) was already added from dir1, so
		// dir1 alone contributed.
		if source != disk+","+dir1 {
			t.Fatalf("source = %q", source)
		}
		if logs.Len() != 0 {
			t.Fatalf("expected no Info+ logs; got %v", logs.All())
		}
	})

	t.Run("no bundle file: added to the embedded roots", func(t *testing.T) {
		logger, _ := newObservedLogger(t)
		data, source := loadCABundle(logger, certEnv{dir: dir2}, []string{filepath.Join(base, "none.crt")}, embeddedFallbackRoots)
		if !bytes.HasPrefix(data, embeddedFallbackRoots) {
			t.Fatal("the embedded roots must stand in for the missing bundle file")
		}
		if got, want := pemCertCount(data), pemCertCount(embeddedFallbackRoots)+1; got != want {
			t.Fatalf("got %d certificates, want %d", got, want)
		}
		if source != systemCABundleSourceEmbedded+","+dir2 {
			t.Fatalf("source = %q", source)
		}
	})

	t.Run("names nothing usable", func(t *testing.T) {
		core, logs := observer.New(zap.DebugLevel)
		data, _ := loadCABundle(zap.New(core), certEnv{dir: filepath.Join(base, "missing")}, []string{disk}, embeddedFallbackRoots)
		if !bytes.Equal(data, diskRoot) {
			t.Fatal("an SSL_CERT_DIR with nothing in it must leave the bundle as it was")
		}
		if logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet("SSL_CERT_DIR").Len() != 1 {
			t.Fatalf("expected a Warn naming SSL_CERT_DIR; got %v", logs.All())
		}
		assertNoCACertificatesAdvice(t, logs)
	})
}

// The production entry point reads SSL_CERT_FILE from the environment.
func TestLoadSystemCABundle_ReadsSSLCertFileFromTheEnvironment(t *testing.T) {
	withSystemSearch(t, nil, nil) // the env file alone, whatever the host has
	corp := makeSelfSignedPEM(t, "corp-root")
	envFile := writePEMFile(t, t.TempDir(), "corp.pem", corp)
	t.Setenv(envSSLCertFile, envFile)
	t.Setenv(envSSLCertDir, "")

	data, source := loadSystemCABundle(zap.NewNop())
	if !bytes.Equal(data, corp) || source != envFile {
		t.Fatalf("got %d bytes from %q; want the SSL_CERT_FILE bundle", len(data), source)
	}
}

// isolateCertEnv restores, when the test ends, the trust-store variables keploy
// sets and its record of which SSL_CERT_FILE values are its own.
func isolateCertEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CARGO_HTTP_CAINFO", envSSLCertFile, envSSLCertDir} {
		t.Setenv(k, os.Getenv(k))
	}
	certFileEnv.Lock()
	ours := make(map[string]bool, len(certFileEnv.ours))
	for k, v := range certFileEnv.ours {
		ours[k] = v
	}
	hadOurs, operator := certFileEnv.ours != nil, certFileEnv.operator
	certFileEnv.Unlock()
	t.Cleanup(func() {
		certFileEnv.Lock()
		defer certFileEnv.Unlock()
		certFileEnv.ours, certFileEnv.operator = nil, operator
		if hadOurs {
			certFileEnv.ours = ours
		}
	})
}

// keploy points SSL_CERT_FILE at files it wrote itself (SetEnvForPath, the
// shared volume). Those must never be read back as the operator's trust store:
// in native mode the client builds the Java truststore right after
// SetEnvForPath pointed SSL_CERT_FILE at the lone keploy CA, and reading that
// would leave Java apps trusting nothing but keploy. The operator's value
// from before keploy replaced it is used instead.
func TestOperatorCertEnv_IgnoresTheFilesKeployPointedSSLCertFileAt(t *testing.T) {
	isolateCertEnv(t)
	withSystemSearch(t, nil, nil) // the env file alone, whatever the host has
	t.Setenv(envSSLCertDir, "")
	dir := t.TempDir()
	corp := makeSelfSignedPEM(t, "corp-root")
	operatorFile := writePEMFile(t, dir, "corp.pem", corp)
	keployOnly := writePEMFile(t, dir, "keploy-ca.crt", testCACertPEM())
	merged := writePEMFile(t, dir, "ca.crt", corp, testCACertPEM())
	t.Setenv(envSSLCertFile, operatorFile)

	if got := operatorCertEnv().file; got != operatorFile {
		t.Fatalf("before keploy sets it: %q, want %q", got, operatorFile)
	}
	if err := SetEnvForPath(zap.NewNop(), keployOnly); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(envSSLCertFile) != keployOnly {
		t.Fatal("SetEnvForPath did not set SSL_CERT_FILE")
	}
	if got := operatorCertEnv().file; got != operatorFile {
		t.Fatalf("after SetEnvForPath: %q, want the operator's %q", got, operatorFile)
	}
	if err := setEnvForSharedVolume(zap.NewNop(), merged, keployOnly); err != nil {
		t.Fatal(err)
	}
	if got := operatorCertEnv().file; got != operatorFile {
		t.Fatalf("after a second keploy write: %q, want the operator's %q", got, operatorFile)
	}
	data, source := loadSystemCABundle(zap.NewNop())
	if !bytes.Equal(data, corp) || source != operatorFile {
		t.Fatalf("the loader read %q (%d bytes), not the operator's bundle", source, len(data))
	}

	// Someone else changing SSL_CERT_FILE afterwards is honoured again.
	other := writePEMFile(t, dir, "other.pem", makeSelfSignedPEM(t, "other-root"))
	t.Setenv(envSSLCertFile, other)
	if got := operatorCertEnv().file; got != other {
		t.Fatalf("a value keploy did not set must be read as is: got %q, want %q", got, other)
	}
}

// TestSetupSharedVolume_MergesSystemAndKeploy asserts that the output ca.crt
// contains both the system bundle and the embedded Keploy MITM CA concatenated
// (in that order, with an intervening newline if necessary), and that
// keploy-ca.crt contains only the Keploy CA.
func TestSetupSharedVolume_MergesSystemAndKeploy(t *testing.T) {
	isolateCertEnv(t)
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
	isolateCertEnv(t)
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
	isolateCertEnv(t)
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

	// Restore the env vars this test mutates (and keploy's record of its own
	// SSL_CERT_FILE values) so follow-up tests in the same binary don't see
	// leaked state.
	isolateCertEnv(t)
	keys := []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"}
	for _, k := range keys {
		_ = os.Unsetenv(k)
	}

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

// A file that holds only keploy's own CA, or keploy's own merged bundle from an
// earlier run, can reach SSL_CERT_FILE from outside this process: a keploy
// process the agent inherited its environment from, or an environment shared
// with the app's. keploy's CAs in it must not be carried into the new bundle —
// apps would keep trusting earlier runs' MITM CAs, and a keploy-only file would
// leave them nothing else (keploy/k8s-proxy#375).
func TestLoadCABundle_SSLCertFile_LeavesOutKeploysOwnCAs(t *testing.T) {
	dir := t.TempDir()
	corp := makeSelfSignedPEM(t, "corp-root")
	missing := []string{filepath.Join(dir, "none.crt")}

	t.Run("an earlier merged bundle", func(t *testing.T) {
		merged := writePEMFile(t, dir, "ca.crt", corp, testCACertPEM())
		logger, logs := newObservedLogger(t)
		data, source := loadCABundle(logger, certEnv{file: merged}, missing, embeddedFallbackRoots)
		if !bytes.Equal(data, corp) || source != merged {
			t.Fatalf("expected the corporate root alone from %q; got %d certs from %q", merged, pemCertCount(data), source)
		}
		if logs.Len() != 0 {
			t.Fatalf("expected no Info+ logs; got %v", logs.All())
		}
	})

	t.Run("keploy's CA alone", func(t *testing.T) {
		keployOnly := writePEMFile(t, dir, "keploy-ca.crt", testCACertPEM())
		logger, logs := newObservedLogger(t)
		data, source := loadCABundle(logger, certEnv{file: keployOnly}, missing, embeddedFallbackRoots)
		if !bytes.Equal(data, embeddedFallbackRoots) || source != systemCABundleSourceEmbedded {
			t.Fatalf("expected the embedded roots; got %d bytes from %q", len(data), source)
		}
		if logs.Len() != 0 {
			t.Fatalf("keploy's own file is not an operator misconfiguration; expected no Info+ logs, got %v", logs.All())
		}
	})

	t.Run("in SSL_CERT_DIR", func(t *testing.T) {
		d := filepath.Join(dir, "certs")
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
		writePEMFile(t, d, "keploy-ca.crt", testCACertPEM())
		writePEMFile(t, d, "corp.pem", corp)
		logger, _ := newObservedLogger(t)
		data, _ := loadCABundle(logger, certEnv{dir: d}, missing, embeddedFallbackRoots)
		if bytes.Contains(data, testCACertPEM()) {
			t.Fatal("keploy's CA from SSL_CERT_DIR reached the bundle")
		}
		if !bytes.HasSuffix(data, corp) {
			t.Fatal("the corporate root from SSL_CERT_DIR is missing")
		}
	})
}

// A combined cert+key PEM is a common thing to point SSL_CERT_FILE at. Only its
// certificates may reach the bundle: ca.crt is world-readable and mounted into
// every app container.
func TestLoadCABundle_SSLCertFile_LeavesOutPrivateKeys(t *testing.T) {
	dir := t.TempDir()
	corp := makeSelfSignedPEM(t, "corp-root")
	key := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("secret key material")})
	combined := writePEMFile(t, dir, "combined.pem", key, corp)

	logger, _ := newObservedLogger(t)
	data, source := loadCABundle(logger, certEnv{file: combined}, nil, embeddedFallbackRoots)
	if source != combined {
		t.Fatalf("source = %q", source)
	}
	if bytes.Contains(data, []byte("PRIVATE KEY")) || bytes.Contains(data, []byte("secret key material")) {
		t.Fatal("the private key reached the trust bundle")
	}
	if !bytes.Equal(data, corp) {
		t.Fatalf("expected the certificate alone; got %q", data)
	}
}

// SSL_CERT_DIR=/etc/ssl/certs next to /etc/ssl/certs/ca-certificates.crt is a
// normal setup: every certificate in the directory is already in the bundle,
// so it adds nothing, and that is not a misconfiguration to warn about.
func TestLoadCABundle_SSLCertDir_OnlyRepeatingTheBundle_DoesNotWarn(t *testing.T) {
	dir := t.TempDir()
	root := makeSelfSignedPEM(t, "distro-root")
	bundle := writePEMFile(t, dir, "ca-certificates.crt", root)
	writePEMFile(t, dir, "distro-root.pem", root)

	core, logs := observer.New(zap.DebugLevel)
	data, source := loadCABundle(zap.New(core), certEnv{dir: dir}, []string{bundle}, embeddedFallbackRoots)
	if !bytes.Equal(data, root) || source != bundle {
		t.Fatalf("got %d certs from %q; want the bundle unchanged", pemCertCount(data), source)
	}
	if n := logs.FilterLevelExact(zap.WarnLevel).Len(); n != 0 {
		t.Fatalf("expected no Warn; got %v", logs.All())
	}
}

// End to end through the shared volume: a corporate CA an operator names with
// SSL_CERT_FILE ends up in the ca.crt the app containers read, next to keploy's
// CA, and in the Java truststore built from it.
func TestSetupSharedVolume_IncludesTheSSLCertFileBundle(t *testing.T) {
	isolateCertEnv(t)
	withSystemSearch(t, nil, nil) // the env file alone, whatever the host has
	corp := makeSelfSignedPEM(t, "corp-root")
	t.Setenv(envSSLCertFile, writePEMFile(t, t.TempDir(), "corp.pem", corp))
	t.Setenv(envSSLCertDir, "")
	orig := loadSystemCABundleFn
	loadSystemCABundleFn = loadSystemCABundle
	t.Cleanup(func() { loadSystemCABundleFn = orig })

	exportDir := t.TempDir()
	if err := setupSharedVolume(context.Background(), zap.NewNop(), exportDir, testCA); err != nil {
		t.Fatalf("setupSharedVolume: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(exportDir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, corp...), testCACertPEM()...)
	if !bytes.Equal(got, want) {
		t.Fatalf("ca.crt holds %d certs; want the corporate root then keploy's CA", pemCertCount(got))
	}
	ks := keystore.New()
	f, err := os.Open(filepath.Join(exportDir, "truststore.jks"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := ks.Load(f, []byte("changeit")); err != nil {
		t.Fatal(err)
	}
	if n := len(ks.Aliases()); n != 2 {
		t.Fatalf("truststore holds %d entries; want the corporate root and keploy's CA", n)
	}
}

// envPrimeChild marks the child process of
// TestSetEnvForPath_KeepsTheProcessesOwnRoots.
const envPrimeChild = "KEPLOY_TEST_PRIME_CHILD_CA"

// crypto/x509 reads SSL_CERT_FILE once, on a process's first use of the system
// pool — on Linux, and since Go 1.27 (x509sslcertoverrideplatform) on macOS and
// Windows too. keploy sets it, in its own process, to files meant for the app —
// the lone keploy CA in native mode, the merged bundle in docker/k8s. keploy's
// own TLS must keep the roots the process started with, not trust keploy's MITM
// CA (or only it). Run in a child process: the system pool is loaded once per
// process.
//
// FAILS BEFORE THE FIX: with no TLS use before SetEnvForPath, the child's
// system pool was keploy's file.
func TestSetEnvForPath_KeepsTheProcessesOwnRoots(t *testing.T) {
	if path := os.Getenv(envPrimeChild); path != "" {
		if err := SetEnvForPath(zap.NewNop(), path); err != nil {
			fmt.Println("SetEnvForPath:", err)
			os.Exit(2)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			fmt.Println("SystemCertPool:", err)
			os.Exit(2)
		}
		// The parent's CA (each process generates its own testCA).
		keployCA, err := os.ReadFile(path)
		if err != nil {
			fmt.Println("read:", err)
			os.Exit(2)
		}
		if own := x509.NewCertPool(); !own.AppendCertsFromPEM(keployCA) || !verifiesPEM(keployCA, own) {
			fmt.Println("control: keploy's CA does not verify against itself")
			os.Exit(2)
		}
		if verifiesPEM(keployCA, pool) {
			fmt.Println("the process's system pool trusts keploy's MITM CA")
			os.Exit(1)
		}
		fmt.Println(primeChildRan)
		os.Exit(0)
	}
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		t.Skip("crypto/x509 does not read SSL_CERT_FILE on " + runtime.GOOS)
	}
	path := writePEMFile(t, t.TempDir(), "keploy-ca.crt", testCACertPEM())
	// The child is bounded by a timeout, and its whole process group is killed
	// on a timeout and when the test ends, however it ends.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSetEnvForPath_KeepsTheProcessesOwnRoots$", "-test.count=1")
	cmd.Env = append(os.Environ(), envPrimeChild+"="+path, envSSLCertFile+"=", envSSLCertDir+"=")
	setOwnProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 10 * time.Second
	t.Cleanup(func() { _ = killProcessGroup(cmd) })
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte(primeChildRan)) {
		t.Fatalf("the child ran no check (did -test.run stop matching?):\n%s", out)
	}
}

// primeChildRan is what the child prints once its check passed.
const primeChildRan = "prime-child-checked"

// verifiesPEM reports whether the self-signed certificate in pemBytes chains
// to a root in pool.
func verifiesPEM(pemBytes []byte, pool *x509.CertPool) bool {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	_, err = cert.Verify(x509.VerifyOptions{Roots: pool})
	return err == nil
}

// The disk bundle is filtered like the env-sourced ones. In native mode on
// Linux keploy adds its own CA to the system bundle, so the client's Java
// truststore, built from that bundle, would otherwise carry this run's CA twice
// and a crashed run's stale one too. A bundle with nothing but keploy's CA is
// no bundle.
func TestLoadCABundle_DiskBundle_LeavesOutKeploysOwnCAs(t *testing.T) {
	dir := t.TempDir()
	root := makeSelfSignedPEM(t, "distro-root")
	keployOnly := writePEMFile(t, dir, "keploy-only.crt", testCACertPEM())
	withKeploy := writePEMFile(t, dir, "ca-certificates.crt", root, testCACertPEM())

	logger, _ := newObservedLogger(t)
	data, source := loadCABundle(logger, certEnv{}, []string{keployOnly, withKeploy}, embeddedFallbackRoots)
	if source != withKeploy {
		t.Fatalf("source = %q; the keploy-only file must be skipped", source)
	}
	if !bytes.Equal(data, root) {
		t.Fatalf("expected the distro root alone; got %d certs", pemCertCount(data))
	}
}

// withSystemSearch points the production loader's fixed bundle paths and
// default directories at test locations.
func withSystemSearch(t *testing.T, paths, dirs []string) {
	t.Helper()
	origPaths, origDirs := systemCABundleSearchPaths, systemCABundleSearchDirs
	systemCABundleSearchPaths, systemCABundleSearchDirs = paths, dirs
	t.Cleanup(func() { systemCABundleSearchPaths, systemCABundleSearchDirs = origPaths, origDirs })
}

// With SSL_CERT_DIR unset, crypto/x509 on Linux reads /etc/ssl/certs and
// /etc/pki/tls/certs too, so a CA dropped into one of them (no bundle rebuilt)
// is trusted by Go programs — and must reach the apps' bundle. A default
// directory that adds nothing (the usual case: it repeats the bundle, or the
// k8s sidecar's empty volume) is not reported.
func TestLoadSystemCABundle_ReadsTheDefaultCertDirs(t *testing.T) {
	isolateCertEnv(t)
	t.Setenv(envSSLCertFile, "")
	t.Setenv(envSSLCertDir, "")
	base := t.TempDir()
	root := makeSelfSignedPEM(t, "distro-root")
	corp := makeSelfSignedPEM(t, "corp-root")
	bundle := writePEMFile(t, base, "ca-certificates.crt", root)
	certs := filepath.Join(base, "certs")
	empty := filepath.Join(base, "empty")
	for _, d := range []string{certs, empty} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writePEMFile(t, certs, "distro-root.pem", root)
	writePEMFile(t, certs, "corp.pem", corp)

	t.Run("a CA dropped into a default directory", func(t *testing.T) {
		withSystemSearch(t, []string{bundle}, []string{certs, empty})
		core, logs := observer.New(zap.DebugLevel)
		data, source := loadSystemCABundle(zap.New(core))
		if !bytes.Equal(data, concat(root, corp)) || source != bundle+","+certs {
			t.Fatalf("got %d certs from %q; want the bundle and the dropped-in CA", pemCertCount(data), source)
		}
		if n := logs.Filter(func(e observer.LoggedEntry) bool { return e.Level >= zap.InfoLevel }).Len(); n != 0 {
			t.Fatalf("expected no Info+ logs; got %v", logs.All())
		}
	})

	t.Run("default directories adding nothing", func(t *testing.T) {
		withSystemSearch(t, []string{filepath.Join(base, "none.crt")}, []string{empty})
		core, logs := observer.New(zap.DebugLevel)
		data, source := loadSystemCABundle(zap.New(core))
		if !bytes.Equal(data, embeddedFallbackRoots) || source != systemCABundleSourceEmbedded {
			t.Fatalf("got %d bytes from %q; want the embedded roots", len(data), source)
		}
		if n := logs.Filter(func(e observer.LoggedEntry) bool { return e.Level >= zap.InfoLevel }).Len(); n != 0 {
			t.Fatalf("an empty default directory is no misconfiguration; got %v", logs.All())
		}
	})

	t.Run("SSL_CERT_DIR replaces the default directories", func(t *testing.T) {
		withSystemSearch(t, []string{bundle}, []string{certs})
		t.Setenv(envSSLCertDir, empty)
		data, _ := loadSystemCABundle(zap.NewNop())
		if !bytes.Equal(data, root) {
			t.Fatalf("got %d certs; the default directory must not be read when SSL_CERT_DIR is set", pemCertCount(data))
		}
	})
}

// crypto/x509 reads default certificate directories only on Linux.
func TestDefaultCertDirs(t *testing.T) {
	if got := defaultCertDirs("linux"); len(got) != 2 || got[0] != "/etc/ssl/certs" || got[1] != "/etc/pki/tls/certs" {
		t.Fatalf("linux: %v", got)
	}
	for _, goos := range []string{"darwin", "windows"} {
		if got := defaultCertDirs(goos); got != nil {
			t.Fatalf("%s: %v", goos, got)
		}
	}
}

// The bundle is built more than once in a process (the shared volume, the
// upstream roots, the native Java truststore): an unusable SSL_CERT_FILE or
// SSL_CERT_DIR is reported once, not on every build.
func TestLoadCABundle_WarnsOncePerValue(t *testing.T) {
	dir := t.TempDir()
	env := certEnv{file: filepath.Join(dir, "missing.pem"), dir: filepath.Join(dir, "missing-dir")}
	core, logs := observer.New(zap.DebugLevel)
	for i := 0; i < 3; i++ {
		loadCABundle(zap.New(core), env, nil, embeddedFallbackRoots)
	}
	if n := logs.FilterLevelExact(zap.WarnLevel).Len(); n != 2 {
		t.Fatalf("expected one Warn for SSL_CERT_FILE and one for SSL_CERT_DIR; got %d: %v", n, logs.All())
	}
}

// negativeSerialCertPEM returns a self-signed CA certificate whose serial
// number is negative: OpenSSL and Java accept it, Go's crypto/x509 rejects it
// (macOS's /etc/ssl/cert.pem ships such a root). The serial is rewritten in a
// generated certificate, so the signature no longer matches; nothing here
// verifies one.
func negativeSerialCertPEM(t *testing.T) []byte {
	t.Helper()
	block, _ := pem.Decode(makeSelfSignedPEM(t, "negative-serial-root"))
	var cert struct {
		TBS      asn1.RawValue
		SigAlg   asn1.RawValue
		SigValue asn1.BitString
	}
	if _, err := asn1.Unmarshal(block.Bytes, &cert); err != nil {
		t.Fatal(err)
	}
	var fields []asn1.RawValue
	for rest := cert.TBS.Bytes; len(rest) > 0; {
		var v asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &v); err != nil {
			t.Fatal(err)
		}
		fields = append(fields, v)
	}
	serial, err := asn1.Marshal(big.NewInt(-12345))
	if err != nil {
		t.Fatal(err)
	}
	fields[1] = asn1.RawValue{FullBytes: serial} // fields[0] is the [0] version
	var content []byte
	for _, f := range fields {
		content = append(content, f.FullBytes...)
	}
	tbs, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: content})
	if err != nil {
		t.Fatal(err)
	}
	cert.TBS = asn1.RawValue{FullBytes: tbs}
	der, err := asn1.Marshal(cert)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x509.ParseCertificate(der); err == nil {
		t.Fatal("fixture: crypto/x509 parsed the negative-serial certificate")
	}
	if !looksLikeCertificate(der) {
		t.Fatal("fixture: the negative-serial certificate lost its shape")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// A root Go's parser rejects but OpenSSL and Java accept (a negative serial)
// stays in the bundle the apps trust, from every source: the disk bundle (the
// native Java truststore on macOS is built from /etc/ssl/cert.pem, which ships
// one), SSL_CERT_FILE and SSL_CERT_DIR. Dropping it would silently break the
// apps' calls to whatever it signs.
func TestLoadCABundle_KeepsCertificatesGoCannotParse(t *testing.T) {
	dir := t.TempDir()
	odd := negativeSerialCertPEM(t)
	root := makeSelfSignedPEM(t, "ordinary-root")
	bundle := writePEMFile(t, dir, "bundle.pem", root, odd)
	missing := []string{filepath.Join(dir, "none.crt")}

	for _, tc := range []struct {
		name  string
		env   certEnv
		paths []string
	}{
		{"disk bundle", certEnv{}, []string{bundle}},
		{"SSL_CERT_FILE", certEnv{file: bundle}, missing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, logs := newObservedLogger(t)
			data, source := loadCABundle(logger, tc.env, tc.paths, embeddedFallbackRoots)
			if source != bundle || !bytes.Equal(data, concat(root, odd)) {
				t.Fatalf("got %d certs from %q; want both roots from %q", pemCertCount(data), source, bundle)
			}
			if logs.Len() != 0 {
				t.Fatalf("expected no Info+ logs; got %v", logs.All())
			}
		})
	}

	t.Run("SSL_CERT_DIR", func(t *testing.T) {
		d := filepath.Join(dir, "certs")
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
		writePEMFile(t, d, "odd.pem", odd)
		writePEMFile(t, d, "odd-again.pem", odd)
		logger, logs := newObservedLogger(t)
		data, _ := loadCABundle(logger, certEnv{dir: d}, missing, embeddedFallbackRoots)
		if !bytes.Equal(data, concat(embeddedFallbackRoots, odd)) {
			t.Fatal("expected the embedded roots plus the negative-serial root, once")
		}
		if logs.Len() != 0 {
			t.Fatalf("expected no Info+ logs; got %v", logs.All())
		}
	})

	t.Run("an SSL_CERT_FILE holding only it", func(t *testing.T) {
		only := writePEMFile(t, dir, "only-odd.pem", odd)
		logger, _ := newObservedLogger(t)
		data, source := loadCABundle(logger, certEnv{file: only}, missing, embeddedFallbackRoots)
		if source != only || !bytes.Equal(data, odd) {
			t.Fatalf("got %d bytes from %q; a bundle of one such root is still a bundle", len(data), source)
		}
	})
}

// keploy's loader must read every bundle file crypto/x509 reads on Linux:
// keploy's own upstream verification swaps Go's pool for the built-in roots
// when the loader finds no bundle file, so a file only Go knows would drop the
// host's trust store (and any CA added to it) from keploy's verification.
// The list is Go 1.27's crypto/x509/root_linux.go certFiles; update it with Go.
func TestSystemCABundleSearchPaths_CoverGosLinuxFiles(t *testing.T) {
	gos := []string{
		"/etc/ssl/certs/ca-certificates.crt",
		"/etc/pki/tls/certs/ca-bundle.crt",
		"/etc/ssl/ca-bundle.pem",
		"/etc/pki/tls/cacert.pem",
		"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
		"/etc/ssl/cert.pem",
	}
	pos := map[string]int{}
	for i, p := range systemCABundleSearchPaths {
		pos[p] = i
	}
	last := -1
	for _, p := range gos {
		i, ok := pos[p]
		if !ok {
			t.Fatalf("systemCABundleSearchPaths lacks %s, which crypto/x509 reads on Linux", p)
		}
		if i < last {
			t.Fatalf("systemCABundleSearchPaths has %s out of crypto/x509's order", p)
		}
		last = i
	}
}

// A trust-store file is read only up to maxCertFile bytes: the loader reads
// every file in SSL_CERT_DIR and the default certificate directories, and a
// large stray file there must not be read into memory whole. A file of exactly
// the cap is read; one byte more is refused, and its certificates are left out
// of the bundle while the directory's other files still count.
func TestReadRegularFile_CapsTheSize(t *testing.T) {
	isolateCertEnv(t)
	withSystemSearch(t, nil, nil)
	dir := t.TempDir()
	padded := func(name string, size int, cert []byte) string {
		t.Helper()
		data := append(append([]byte(nil), cert...), bytes.Repeat([]byte("\n"), size-len(cert))...)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	atCap := makeSelfSignedPEM(t, "at-the-cap-root")
	overCap := makeSelfSignedPEM(t, "over-the-cap-root")
	corp := makeSelfSignedPEM(t, "corp-root")
	atPath := padded("at-cap.pem", maxCertFile, atCap)
	overPath := padded("over-cap.pem", maxCertFile+1, overCap)
	writePEMFile(t, dir, "corp.pem", corp)

	if data, err := readRegularFile(atPath); err != nil || len(data) != maxCertFile {
		t.Fatalf("a file of exactly %d bytes: got %d bytes, err %v; want it read whole", maxCertFile, len(data), err)
	}
	if data, err := readRegularFile(overPath); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("a file of %d bytes: got %d bytes, err %v; want it refused", maxCertFile+1, len(data), err)
	}

	t.Setenv(envSSLCertFile, "")
	t.Setenv(envSSLCertDir, dir)
	data, _ := loadSystemCABundle(zap.NewNop())
	want := mergeCABundle(embeddedFallbackRoots, concat(atCap, corp))
	if !bytes.Equal(data, want) {
		t.Fatalf("the bundle holds %d certificates; want the built-in roots, the at-the-cap root and the corporate root (%d), and not the over-the-cap one",
			pemCertCount(data), pemCertCount(want))
	}
}

// In native mode the client starts the app with the trust variables keploy
// sets. Python (requests), OpenSSL and curl, and Cargo read theirs as the whole
// trust store, so each must name one bundle of the roots keploy's loader reads
// plus keploy's CA: the operator's SSL_CERT_FILE, the system bundle, or with
// neither the built-in roots. Node adds NODE_EXTRA_CA_CERTS to its own roots,
// so it names keploy's CA alone.
//
// FAILS BEFORE THE FIX: all four named the lone keploy CA, so Python, curl and
// Rust apps trusted no endpoint keploy does not intercept, nor the operator's
// corporate CA.
func TestSetupCaCertEnv_WholeStoreVariablesNameTheRootsPlusKeploysCA(t *testing.T) {
	isolateCertEnv(t)
	tmp := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, tmp)
	}
	base := t.TempDir()
	corp := makeSelfSignedPEM(t, "corp-root")
	distro := makeSelfSignedPEM(t, "distro-root")
	corpFile := writePEMFile(t, base, "corp.pem", corp)
	distroBundle := writePEMFile(t, base, "ca-certificates.crt", distro)

	for _, tc := range []struct {
		name     string
		certFile string
		paths    []string
		roots    []byte
	}{
		{"the operator's SSL_CERT_FILE", corpFile, []string{distroBundle}, corp},
		{"the system bundle", "", []string{distroBundle}, distro},
		{"no bundle file: the built-in roots", "", nil, embeddedFallbackRoots},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateCertEnv(t)
			withSystemSearch(t, tc.paths, nil)
			t.Setenv(envSSLCertFile, tc.certFile)
			t.Setenv(envSSLCertDir, "")
			t.Setenv(EnvJavaToolOptions, "")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			if err := SetupCaCertEnv(ctx, zap.NewNop(), testCACertPEM()); err != nil {
				t.Fatal(err)
			}
			// The run ends with the row: its lock file closed, its directory gone.
			dir := filepath.Dir(os.Getenv("NODE_EXTRA_CA_CERTS"))
			run := liveRunOf(t, dir)
			t.Cleanup(func() {
				cancel()
				waitRunGone(t, run)
			})
			want := mergeCABundle(tc.roots, testCACertPEM())
			for _, v := range []string{envSSLCertFile, "REQUESTS_CA_BUNDLE", "CARGO_HTTP_CAINFO"} {
				t.Run(v, func(t *testing.T) {
					got, err := os.ReadFile(os.Getenv(v))
					if err != nil {
						t.Fatalf("%s=%q: %v", v, os.Getenv(v), err)
					}
					if !bytes.Equal(got, want) {
						t.Fatalf("%s names %d certificates; want the roots then keploy's CA (%d)", v, pemCertCount(got), pemCertCount(want))
					}
				})
			}
			t.Run("NODE_EXTRA_CA_CERTS", func(t *testing.T) {
				got, err := os.ReadFile(os.Getenv("NODE_EXTRA_CA_CERTS"))
				if err != nil || !bytes.Equal(got, testCACertPEM()) {
					t.Fatalf("NODE_EXTRA_CA_CERTS must name keploy's CA alone; got %d certificates (%v)", pemCertCount(got), err)
				}
			})
			t.Run(EnvJavaToolOptions, func(t *testing.T) {
				// Java's truststore holds the same roots plus keploy's CA.
				var store string
				for _, f := range strings.Fields(os.Getenv(EnvJavaToolOptions)) {
					if v, ok := strings.CutPrefix(f, "-Djavax.net.ssl.trustStore="); ok {
						store = v
					}
				}
				if store == "" {
					t.Fatalf("%s names no truststore: %q", EnvJavaToolOptions, os.Getenv(EnvJavaToolOptions))
				}
				f, err := os.Open(store)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = f.Close() }()
				ks := keystore.New()
				if err := ks.Load(f, []byte("changeit")); err != nil {
					t.Fatal(err)
				}
				if n := len(ks.Aliases()); n != pemCertCount(want) {
					t.Fatalf("the truststore holds %d entries; want the roots then keploy's CA (%d)", n, pemCertCount(want))
				}
			})
		})
	}
}

// envNativeChild names, in the child process of
// TestSetupCAForApp_LoadsTheProcessesRootsBeforeTheNativeInstall, the bundle
// file the stubbed native install adds keploy's CA to.
const envNativeChild = "KEPLOY_TEST_NATIVE_CHILD_BUNDLE"

// On Linux keploy's native install adds its CA to the system bundle, a file
// crypto/x509 reads on the process's first use of the system pool. keploy's
// own TLS must keep the roots the process started with, so SetupCAForApp
// loads them before the install. The install is stubbed to append keploy's CA
// to the bundle SSL_CERT_FILE names, as the real one appends it to the
// distro bundle. Run in a child process: the system pool is loaded once per
// process.
func TestSetupCAForApp_LoadsTheProcessesRootsBeforeTheNativeInstall(t *testing.T) {
	if bundle := os.Getenv(envNativeChild); bundle != "" {
		var installed []byte
		setupNativeForAppFn = func(_ context.Context, _ *zap.Logger, ca *caState, _ caStore, _ int, _ string) error {
			installed = ca.certPEM
			f, err := os.OpenFile(bundle, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			_, err = f.Write(ca.certPEM)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			return err
		}
		if err := SetupCAForApp(context.Background(), zap.NewNop(), false, 0, ""); err != nil {
			fmt.Println("SetupCAForApp:", err)
			os.Exit(2)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			fmt.Println("SystemCertPool:", err)
			os.Exit(2)
		}
		data, err := os.ReadFile(bundle)
		if err != nil || len(installed) == 0 || !bytes.HasSuffix(data, installed) {
			fmt.Println("control: the stubbed install did not add keploy's CA to the bundle:", err)
			os.Exit(2)
		}
		if !verifiesPEM(data, pool) { // its first certificate: the distro root
			fmt.Println("control: the process's system pool lacks the bundle's own root")
			os.Exit(2)
		}
		if verifiesPEM(installed, pool) {
			fmt.Println("the process's system pool trusts keploy's MITM CA")
			os.Exit(1)
		}
		fmt.Println(primeChildRan)
		os.Exit(0)
	}
	if runtime.GOOS != "linux" {
		t.Skip("the native install adds keploy's CA to a bundle file crypto/x509 reads only on Linux")
	}
	dir := t.TempDir()
	bundle := writePEMFile(t, dir, "ca-certificates.crt", makeSelfSignedPEM(t, "distro-root"))
	noDir := filepath.Join(dir, "no-cert-dir")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSetupCAForApp_LoadsTheProcessesRootsBeforeTheNativeInstall$", "-test.count=1")
	cmd.Env = append(os.Environ(), envNativeChild+"="+bundle, envSSLCertFile+"="+bundle, envSSLCertDir+"="+noDir)
	setOwnProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 10 * time.Second
	t.Cleanup(func() { _ = killProcessGroup(cmd) })
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte(primeChildRan)) {
		t.Fatalf("the child ran no check (did -test.run stop matching?):\n%s", out)
	}
}

// setEnvVars notes SSL_CERT_FILE by its key, whatever other variables it sets
// and in whatever order the map yields them, and notes no other variable's
// value as keploy's SSL_CERT_FILE.
func TestSetEnvVars_NotesSSLCertFileByItsKey(t *testing.T) {
	isolateCertEnv(t)
	dir := t.TempDir()
	operatorFile := writePEMFile(t, dir, "corp.pem", makeSelfSignedPEM(t, "corp-root"))
	t.Setenv(envSSLCertFile, operatorFile)
	keploys := filepath.Join(dir, "keploy-bundle.crt")
	other := filepath.Join(dir, "keploy-ca.crt")

	if err := setEnvVars(zap.NewNop(), map[string]string{envSSLCertFile: keploys, "NODE_EXTRA_CA_CERTS": other}); err != nil {
		t.Fatal(err)
	}
	if got := operatorCertEnv().file; got != operatorFile {
		t.Fatalf("after keploy set SSL_CERT_FILE: %q, want the operator's %q", got, operatorFile)
	}
	t.Setenv(envSSLCertFile, other)
	if got := operatorCertEnv().file; got != other {
		t.Fatalf("SSL_CERT_FILE set to %q by someone else read as %q", other, got)
	}
}

// legacyCACertPEM is the public certificate of the retired static MITM CA
// (legacyCASHA256Hex), once committed at pkg/core/proxy/tls/asset/ca.crt.
const legacyCACertPEM = `-----BEGIN CERTIFICATE-----
MIIBczCCARigAwIBAgIUe1yXSfGefdyA09reeQpit2S7TXMwCgYIKoZIzj0EAwIw
FzEVMBMGA1UEAxMMTXkgQ3VzdG9tIENBMB4XDTIzMDQyMDExMDYwMFoXDTMzMDQx
NzExMDYwMFowFzEVMBMGA1UEAxMMTXkgQ3VzdG9tIENBMFkwEwYHKoZIzj0CAQYI
KoZIzj0DAQcDQgAE+QJh3KtWSGRe9NtzZGcJQBcn0ipsjzpo7YlLvbaSFeI62H1b
5EPUjnbDInG4K0nO1Dq/gPtnsVXZcJx06oHj9qNCMEAwDgYDVR0PAQH/BAQDAgEG
MA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFDejUAabmE7NFoKSOo5uHAHMumal
MAoGCCqGSM49BAMCA0kAMEYCIQDkGU6Oku9vLo7fLLquHWXR1rB/1YgWIBghnQXJ
fF5GawIhAK/0BaJCVm6rkRuYCdtahkN1N5DtTPr8AQoDV+MT0UmL
-----END CERTIFICATE-----
`

// The loader rebuilds every file-sourced bundle from the blocks crypto/x509
// would load as roots, less keploy's own CAs. It leaves out the retired static
// CA (its key was public, and its CN is not keploy's), a CERTIFICATE block
// holding no certificate, a CERTIFICATE block with headers and a TRUSTED
// CERTIFICATE block (OpenSSL's form, whose trust settings a plain
// CERTIFICATE would drop): crypto/x509 loads none of them.
func TestLoadCABundle_SSLCertFile_LeavesOutWhatGoDoesNotLoad(t *testing.T) {
	block, _ := pem.Decode([]byte(legacyCACertPEM))
	if fp, ok := legacyCAFingerprint(); block == nil || !ok || sha256.Sum256(block.Bytes) != fp {
		t.Fatal("fixture: legacyCACertPEM is not the retired CA legacyCAFingerprint names")
	}
	otherDER, _ := pem.Decode(makeSelfSignedPEM(t, "other-root"))
	dir := t.TempDir()
	corp := makeSelfSignedPEM(t, "corp-root")
	missing := []string{filepath.Join(dir, "none.crt")}
	for _, tc := range []struct {
		name string
		odd  []byte
	}{
		{"the retired static CA", []byte(legacyCACertPEM)},
		{"a CERTIFICATE block holding no certificate", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")})},
		{"a CERTIFICATE block with headers", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Headers: map[string]string{"Comment": "x"}, Bytes: otherDER.Bytes})},
		{"a TRUSTED CERTIFICATE block", pem.EncodeToMemory(&pem.Block{Type: "TRUSTED CERTIFICATE", Bytes: otherDER.Bytes})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := writePEMFile(t, t.TempDir(), "bundle.pem", corp, tc.odd)
			data, source := loadCABundle(zap.NewNop(), certEnv{file: file}, missing, embeddedFallbackRoots)
			if source != file || !bytes.Equal(data, corp) {
				t.Fatalf("expected the corporate root alone from %q; got %d certs from %q", file, pemCertCount(data), source)
			}
		})
	}
}

// With no bundle file and no embedded roots (the disk-only path), the source
// names only the directories the certificates came from.
func TestLoadCABundle_NoFallback_SourceNamesOnlyTheCertDir(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "certs")
	if err := os.MkdirAll(d, 0755); err != nil {
		t.Fatal(err)
	}
	corp := makeSelfSignedPEM(t, "corp-root")
	writePEMFile(t, d, "corp.pem", corp)
	data, source := loadCABundle(zap.NewNop(), certEnv{dir: d}, []string{filepath.Join(dir, "none.crt")}, nil)
	if source != d || !bytes.Equal(data, corp) {
		t.Fatalf("expected the corporate root from %q alone; got %d certs from %q", d, pemCertCount(data), source)
	}
}

// An SSL_CERT_DIR holding only keploy's own CA (inherited from a keploy
// process, or left by an earlier run) adds nothing, and is not an operator
// misconfiguration to warn about.
func TestLoadCABundle_SSLCertDir_OnlyKeploysCA_DoesNotWarn(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "certs")
	if err := os.MkdirAll(d, 0755); err != nil {
		t.Fatal(err)
	}
	writePEMFile(t, d, "keploy-ca.crt", testCACertPEM())
	logger, logs := newObservedLogger(t)
	data, source := loadCABundle(logger, certEnv{dir: d}, []string{filepath.Join(dir, "none.crt")}, embeddedFallbackRoots)
	if !bytes.Equal(data, embeddedFallbackRoots) || source != systemCABundleSourceEmbedded {
		t.Fatalf("expected the embedded roots alone; got %d bytes from %q", len(data), source)
	}
	if logs.Len() != 0 {
		t.Fatalf("expected no Info+ logs; got %v", logs.All())
	}
}
