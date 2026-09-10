package tls

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// stubSystemCertPool replaces the x509.SystemCertPool seam for the duration of
// a test. Not t.Parallel()-safe (package-level var), matching the other fn-var
// swapping tests in this package.
func stubSystemCertPool(t *testing.T, pool *x509.CertPool, err error) {
	t.Helper()
	orig := systemCertPoolFn
	systemCertPoolFn = func() (*x509.CertPool, error) { return pool, err }
	t.Cleanup(func() { systemCertPoolFn = orig })
}

// stubSystemCABundle replaces keploy's own disk-search + embedded-roots loader.
func stubSystemCABundle(t *testing.T, bundle []byte, source string) {
	t.Helper()
	orig := loadSystemCABundleFn
	loadSystemCABundleFn = func(_ *zap.Logger) ([]byte, string) { return bundle, source }
	t.Cleanup(func() { loadSystemCABundleFn = orig })
}

// poolWith returns a non-system pool seeded with the given PEM bundles, standing
// in for a healthy platform trust store.
func poolWith(t *testing.T, pems ...[]byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	for _, p := range pems {
		if !pool.AppendCertsFromPEM(p) {
			t.Fatalf("failed to seed test pool from PEM")
		}
	}
	return pool
}

// verifiesAgainst reports whether the self-signed CA in pemBytes chains to a
// root in pool. A self-signed CA is its own root, so this is a genuine
// end-to-end check that the pool works as a trust store — stronger than
// counting Subjects().
func verifiesAgainst(t *testing.T, pemBytes []byte, pool *x509.CertPool) bool {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("test PEM did not decode")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	_, err = cert.Verify(x509.VerifyOptions{Roots: pool})
	return err == nil
}

// A healthy platform pool and no extra CA is the default shape: the caller must
// get nil back so tls.Config.RootCAs stays nil and crypto/tls uses exactly the
// roots (and, on macOS/Windows, the platform verifier) it would have used
// unconfigured.
func TestLoadUpstreamRootCAs_NoCACert_ReturnsNilForGoDefault(t *testing.T) {
	systemCA := makeSelfSignedPEM(t, "system-root")
	stubSystemCertPool(t, poolWith(t, systemCA), nil)

	pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
	if err != nil {
		t.Fatalf("LoadUpstreamRootCAs: unexpected error: %v", err)
	}
	if pool != nil {
		t.Fatal("expected a nil pool so crypto/tls uses Go's own default roots, got a pool")
	}
}

// The operator's PEM must be ADDED to the platform roots, not substituted for
// them: a private CA for one upstream must not stop keploy verifying public
// upstreams on the same recording.
func TestLoadUpstreamRootCAs_ValidPEM_AppendsToSystemRoots(t *testing.T) {
	systemCA := makeSelfSignedPEM(t, "system-root")
	userCA := makeSelfSignedPEM(t, "private-corp-root")
	stubSystemCertPool(t, poolWith(t, systemCA), nil)

	path := filepath.Join(t.TempDir(), "corp-ca.pem")
	if err := os.WriteFile(path, userCA, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	pool, err := LoadUpstreamRootCAs(zap.NewNop(), path)
	if err != nil {
		t.Fatalf("LoadUpstreamRootCAs: unexpected error: %v", err)
	}
	if pool == nil {
		t.Fatal("expected a pool when caCert is set, got nil")
	}
	if !verifiesAgainst(t, userCA, pool) {
		t.Error("the configured CA does not verify against the returned pool")
	}
	if !verifiesAgainst(t, systemCA, pool) {
		t.Error("the system roots were replaced rather than extended")
	}
}

// A multi-cert PEM (the usual shape of a corporate chain file) must load every
// block, not just the first.
func TestLoadUpstreamRootCAs_ValidPEM_LoadsEveryBlock(t *testing.T) {
	stubSystemCertPool(t, poolWith(t, makeSelfSignedPEM(t, "system-root")), nil)

	first := makeSelfSignedPEM(t, "corp-root-1")
	second := makeSelfSignedPEM(t, "corp-root-2")
	path := filepath.Join(t.TempDir(), "chain.pem")
	if err := os.WriteFile(path, append(append([]byte{}, first...), second...), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	pool, err := LoadUpstreamRootCAs(zap.NewNop(), path)
	if err != nil {
		t.Fatalf("LoadUpstreamRootCAs: unexpected error: %v", err)
	}
	if !verifiesAgainst(t, first, pool) || !verifiesAgainst(t, second, pool) {
		t.Error("not every certificate in the bundle made it into the pool")
	}
}

// A typo'd path must be a loud, named error. Returning a usable-looking pool
// here would leave verification on with the operator's CA absent, which surfaces
// as silently dropped mocks rather than as a configuration error.
func TestLoadUpstreamRootCAs_MissingFile_ErrorsNamingTheFile(t *testing.T) {
	stubSystemCertPool(t, poolWith(t, makeSelfSignedPEM(t, "system-root")), nil)

	path := filepath.Join(t.TempDir(), "does-not-exist.pem")
	pool, err := LoadUpstreamRootCAs(zap.NewNop(), path)
	if err == nil {
		t.Fatal("expected an error for a nonexistent CA file, got nil")
	}
	if pool != nil {
		t.Error("expected a nil pool alongside the error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the offending file: %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error does not wrap the underlying os error: %v", err)
	}
}

// A file that parses as zero certificates (DER saved as .pem, a key-only file, a
// truncated download) is just as broken as a missing one — AppendCertsFromPEM
// reports false without an error of its own, so this is the case a naive
// implementation swallows.
func TestLoadUpstreamRootCAs_NoPEMBlocks_ErrorsNamingTheFile(t *testing.T) {
	stubSystemCertPool(t, poolWith(t, makeSelfSignedPEM(t, "system-root")), nil)

	path := filepath.Join(t.TempDir(), "not-a-bundle.pem")
	if err := os.WriteFile(path, []byte("this file contains no PEM blocks at all\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	pool, err := LoadUpstreamRootCAs(zap.NewNop(), path)
	if err == nil {
		t.Fatal("expected an error for a PEM-less CA file, got nil")
	}
	if pool != nil {
		t.Error("expected a nil pool alongside the error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the offending file: %v", err)
	}
}

// x509.SystemCertPool on Unix does NOT fail on an image with no trust store — it
// returns an empty pool and a nil error. Handing that to tls.Config.RootCAs
// would reject every upstream, so emptiness must route to keploy's own bundle
// loader (disk search, then the go:embed'd Mozilla roots).
func TestLoadUpstreamRootCAs_EmptySystemPool_FallsBackToKeployBundle(t *testing.T) {
	stubSystemCertPool(t, x509.NewCertPool(), nil)
	embedded := makeSelfSignedPEM(t, "embedded-mozilla-stand-in")
	stubSystemCABundle(t, embedded, systemCABundleSourceEmbedded)

	pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
	if err != nil {
		t.Fatalf("LoadUpstreamRootCAs: unexpected error: %v", err)
	}
	if pool == nil {
		t.Fatal("expected the embedded fallback pool, got nil (which would verify against the same empty store)")
	}
	if !verifiesAgainst(t, embedded, pool) {
		t.Error("the embedded fallback roots are not in the returned pool")
	}
}

// Same fallback when SystemCertPool reports a hard error rather than an empty
// pool (the Windows/js shape).
func TestLoadUpstreamRootCAs_SystemPoolError_FallsBackToKeployBundle(t *testing.T) {
	stubSystemCertPool(t, nil, errors.New("system root pool is not available"))
	embedded := makeSelfSignedPEM(t, "embedded-mozilla-stand-in")
	stubSystemCABundle(t, embedded, systemCABundleSourceEmbedded)

	pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
	if err != nil {
		t.Fatalf("LoadUpstreamRootCAs: unexpected error: %v", err)
	}
	if pool == nil || !verifiesAgainst(t, embedded, pool) {
		t.Fatal("expected the embedded fallback roots when SystemCertPool errors")
	}
}

// The one thing this function must never do is hand back a non-nil EMPTY pool:
// that is "trust nothing", and it fails every handshake. With nothing loadable
// anywhere the answer is nil — Go's default — plus a logged error.
func TestLoadUpstreamRootCAs_NothingLoadable_NeverReturnsEmptyPool(t *testing.T) {
	stubSystemCertPool(t, x509.NewCertPool(), nil)
	stubSystemCABundle(t, nil, "")

	logger, logs := newObservedLogger(t)
	pool, err := LoadUpstreamRootCAs(logger, "")
	if err != nil {
		t.Fatalf("LoadUpstreamRootCAs: unexpected error: %v", err)
	}
	if pool != nil {
		t.Fatal("expected nil (Go's default); a non-nil pool here would be empty, i.e. \"trust nothing\"")
	}
	if logs.FilterLevelExact(zap.ErrorLevel).Len() == 0 {
		t.Error("expected an ERROR log naming the missing trust anchors")
	}
}

// Even with no base roots at all, an explicitly configured CA is a complete
// answer on its own — the operator named the only anchor they need.
func TestLoadUpstreamRootCAs_NothingLoadable_StillHonoursConfiguredCA(t *testing.T) {
	stubSystemCertPool(t, x509.NewCertPool(), nil)
	stubSystemCABundle(t, nil, "")

	userCA := makeSelfSignedPEM(t, "the-only-root")
	path := filepath.Join(t.TempDir(), "only-ca.pem")
	if err := os.WriteFile(path, userCA, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	pool, err := LoadUpstreamRootCAs(zap.NewNop(), path)
	if err != nil {
		t.Fatalf("LoadUpstreamRootCAs: unexpected error: %v", err)
	}
	if pool == nil || !verifiesAgainst(t, userCA, pool) {
		t.Fatal("expected a pool containing the configured CA")
	}
}

// TestLoadUpstreamRootCAs_PartiallyCorruptBundle is the regression test for a
// bundle that loses trust anchors silently.
//
// x509.CertPool.AppendCertsFromPEM returns true when it parsed AT LEAST ONE
// certificate, so a corporate bundle with one truncated anchor among several
// loads "successfully" while quietly dropping it. The operator then sees
// dest-side handshake failures that fall through to raw passthrough and drop
// mocks, with no configuration error anywhere to explain it — exactly the
// outcome this loader's contract promises to prevent.
//
// FAILS BEFORE THE FIX: the loader only inspected AppendCertsFromPEM's boolean,
// so the discrepancy was invisible and no warning was emitted.
func TestLoadUpstreamRootCAs_PartiallyCorruptBundle(t *testing.T) {
	stubSystemCertPool(t, poolWith(t, makeSelfSignedPEM(t, "system-root")), nil)

	good1 := makeSelfSignedPEM(t, "corp-root-1")
	good2 := makeSelfSignedPEM(t, "corp-root-2")
	// A CERTIFICATE block whose DER is garbage: x509 rejects it, so
	// AppendCertsFromPEM skips it and still reports true for the two good ones.
	corrupt := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")})

	bundle := append(append(append([]byte{}, good1...), corrupt...), good2...)
	path := filepath.Join(t.TempDir(), "corp-bundle.pem")
	if err := os.WriteFile(path, bundle, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	core, logs := observer.New(zapcore.DebugLevel)
	pool, err := LoadUpstreamRootCAs(zap.New(core), path)
	if err != nil {
		t.Fatalf("a bundle with two valid anchors must still load: %v", err)
	}
	if pool == nil {
		t.Fatal("expected a non-nil pool when a caCert was supplied")
	}
	// The anchors that DID parse must be usable — refusing the whole bundle
	// over one bad block would take verification down entirely.
	if !verifiesAgainst(t, good1, pool) || !verifiesAgainst(t, good2, pool) {
		t.Fatal("the valid anchors in a partially corrupt bundle were not loaded")
	}
	entries := logs.FilterMessageSnippet("could not be parsed").All()
	if len(entries) == 0 {
		t.Fatalf("the dropped anchor was silent; the operator has no way to connect the missing trust to a bad PEM block: %v", logs.All())
	}
	fields := entries[0].ContextMap()
	if got := fields["certificate_blocks"]; got != int64(3) {
		t.Errorf("certificate_blocks = %v, want 3", got)
	}
	if got := fields["loaded"]; got != int64(2) {
		t.Errorf("loaded = %v, want 2", got)
	}
}

// TestLoadUpstreamRootCAs_IntactBundleDoesNotWarn is the other half: a healthy
// bundle must not cry wolf, or the warning above becomes noise operators learn
// to ignore.
func TestLoadUpstreamRootCAs_IntactBundleDoesNotWarn(t *testing.T) {
	stubSystemCertPool(t, poolWith(t, makeSelfSignedPEM(t, "system-root")), nil)

	bundle := append(append([]byte{}, makeSelfSignedPEM(t, "corp-root-1")...), makeSelfSignedPEM(t, "corp-root-2")...)
	path := filepath.Join(t.TempDir(), "corp-bundle.pem")
	if err := os.WriteFile(path, bundle, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	core, logs := observer.New(zapcore.DebugLevel)
	if _, err := LoadUpstreamRootCAs(zap.New(core), path); err != nil {
		t.Fatalf("LoadUpstreamRootCAs: %v", err)
	}
	if n := logs.FilterMessageSnippet("could not be parsed").Len(); n != 0 {
		t.Fatalf("an intact bundle produced %d dropped-anchor warnings", n)
	}
}

// TestCountPEMCertificateBlocks pins the counter against the exact filter
// AppendCertsFromPEM applies, so the two numbers stay comparable: only
// header-free CERTIFICATE blocks are candidates, and a block that fails to
// parse counts as wanted-but-not-loaded.
func TestCountPEMCertificateBlocks(t *testing.T) {
	good := makeSelfSignedPEM(t, "root")
	corrupt := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")})
	key := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("junk")})
	withHeaders := pem.EncodeToMemory(&pem.Block{
		Type:    "CERTIFICATE",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED"},
		Bytes:   []byte("junk"),
	})

	cases := []struct {
		name           string
		in             []byte
		wanted, parsed int
	}{
		{"empty", nil, 0, 0},
		{"one good", good, 1, 1},
		{"one corrupt", corrupt, 1, 0},
		{"good + corrupt + good", concat(good, corrupt, good), 3, 2},
		{"a private key is not a candidate", concat(good, key), 1, 1},
		{"a headered block is skipped like AppendCertsFromPEM skips it", concat(good, withHeaders), 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wanted, parsed := countPEMCertificateBlocks(tc.in)
			if wanted != tc.wanted || parsed != tc.parsed {
				t.Fatalf("countPEMCertificateBlocks = (%d, %d), want (%d, %d)", wanted, parsed, tc.wanted, tc.parsed)
			}
		})
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
