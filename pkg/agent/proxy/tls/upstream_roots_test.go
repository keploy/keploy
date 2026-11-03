package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"iter"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	nssbundle "golang.org/x/crypto/x509roots/fallback/bundle"
)

// stubSystemCertPool replaces the x509.SystemCertPool seam for the duration of
// a test. Not t.Parallel()-safe (package-level var), matching the other fn-var
// swapping tests in this package.
//
// It also treats the stubbed pool as a platform pool (not file-backed), so a
// test's outcome does not depend on the host's trust store; tests of the
// file-backed rule call stubFileBackedPool after it.
func stubSystemCertPool(t *testing.T, pool *x509.CertPool, err error) {
	t.Helper()
	orig := systemCertPoolFn
	systemCertPoolFn = func() (*x509.CertPool, error) { return pool, err }
	t.Cleanup(func() { systemCertPoolFn = orig })
	stubFileBackedPool(t, false)
}

// stubFileBackedPool sets whether the system pool counts as file-backed.
func stubFileBackedPool(t *testing.T, fileBacked bool) {
	t.Helper()
	orig := systemPoolIsFileBacked
	systemPoolIsFileBacked = fileBacked
	t.Cleanup(func() { systemPoolIsFileBacked = orig })
}

// stubConstrainedNSSRoot makes the NSS root set the single CA given, with the
// constraint given (nil for none).
func stubConstrainedNSSRoot(t *testing.T, ca *x509.Certificate, constraint func([]*x509.Certificate) error) {
	t.Helper()
	old := nssRoots
	nssRoots = func() iter.Seq[nssbundle.Root] {
		return func(yield func(nssbundle.Root) bool) {
			yield(nssbundle.Root{Certificate: ca.Raw, Constraint: constraint})
		}
	}
	t.Cleanup(func() { nssRoots = old })
}

// errDistrusted is the constraint error the tests' NSS roots return.
var errDistrusted = errors.New("distrusted after date")

// requireDistrusted fails unless verr is the NSS constraint rejecting the
// chain — not merely any verification error (an unknown authority would pass
// a plain verr != nil check without the constraint ever being consulted).
func requireDistrusted(t *testing.T, verr error) {
	t.Helper()
	if verr == nil || !strings.Contains(verr.Error(), errDistrusted.Error()) {
		t.Fatalf("expected the NSS constraint to reject the chain; got %v", verr)
	}
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

// With the real compiled-in bundle as the fallback, the upstream pool is built
// from the NSS root set itself and holds every NSS root.
func TestLoadUpstreamRootCAs_RealEmbeddedRoots_UsesNSSRootPool(t *testing.T) {
	stubSystemCertPool(t, x509.NewCertPool(), nil)
	stubSystemCABundle(t, embeddedFallbackRoots, systemCABundleSourceEmbedded)

	pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
	if err != nil {
		t.Fatalf("LoadUpstreamRootCAs: unexpected error: %v", err)
	}
	want, n := nssRootPool(nssRoots())
	if n < 100 {
		t.Fatalf("NSS bundle has only %d roots; expected >=100", n)
	}
	if pool == nil || !pool.Equal(want) {
		t.Error("upstream pool does not hold exactly the NSS roots")
	}
}

// An NSS root's constraint (e.g. distrust-after, which a PEM file cannot carry)
// must be enforced on keploy's own upstream verification when the compiled-in
// bundle is the fallback: a chain to a root whose constraint rejects must fail.
func TestLoadUpstreamRootCAs_RealEmbeddedRoots_EnforcesNSSConstraints(t *testing.T) {
	ca, caKey := newTestCA(t, "nss-constrained-root")
	leaf := newTestLeaf(t, ca, caKey, "upstream.example")

	for _, tc := range []struct {
		name       string
		constraint func([]*x509.Certificate) error
		wantOK     bool
	}{
		{"unconstrained root verifies", nil, true},
		{"constraint rejects the chain", func([]*x509.Certificate) error { return errors.New("distrusted after date") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := nssRoots
			nssRoots = func() iter.Seq[nssbundle.Root] {
				return func(yield func(nssbundle.Root) bool) {
					yield(nssbundle.Root{Certificate: ca.Raw, Constraint: tc.constraint})
				}
			}
			t.Cleanup(func() { nssRoots = old })
			stubSystemCertPool(t, x509.NewCertPool(), nil)
			stubSystemCABundle(t, embeddedFallbackRoots, systemCABundleSourceEmbedded)

			pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
			if err != nil || pool == nil {
				t.Fatalf("LoadUpstreamRootCAs: pool=%v err=%v", pool, err)
			}
			_, verr := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "upstream.example"})
			if (verr == nil) != tc.wantOK {
				t.Fatalf("leaf.Verify error = %v, want ok=%v", verr, tc.wantOK)
			}
		})
	}
}

// A platform pool that holds exactly the compiled-in Mozilla list is that list
// read back from a PEM file (SSL_CERT_FILE naming the file an image without a
// bundle had the roots written to), which drops the NSS constraints. keploy's
// own upstream verification must use the constrained pool instead, and a
// platform pool that is anything else is still left to crypto/tls.
//
// FAILS BEFORE THE FIX: the platform pool was returned as is (nil to the
// caller), so a chain to a root whose NSS constraint rejects it verified.
func TestLoadUpstreamRootCAs_SystemPoolIsTheEmbeddedList_EnforcesNSSConstraints(t *testing.T) {
	ca, caKey := newTestCA(t, "nss-constrained-root")
	leaf := newTestLeaf(t, ca, caKey, "upstream.example")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})

	t.Run("control: an unconstrained root verifies", func(t *testing.T) {
		stubConstrainedNSSRoot(t, ca, nil)
		stubSystemCertPool(t, poolWith(t, caPEM), nil)
		pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
		if err != nil || pool == nil {
			t.Fatalf("expected the NSS pool; pool=%v err=%v", pool, err)
		}
		if _, verr := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "upstream.example"}); verr != nil {
			t.Fatalf("leaf.Verify: %v", verr)
		}
	})

	t.Run("the platform pool is the embedded list", func(t *testing.T) {
		stubConstrainedNSSRoot(t, ca, func([]*x509.Certificate) error { return errDistrusted })
		stubSystemCertPool(t, poolWith(t, caPEM), nil)
		pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
		if err != nil || pool == nil {
			t.Fatalf("expected the constrained pool; pool=%v err=%v", pool, err)
		}
		_, verr := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "upstream.example"})
		requireDistrusted(t, verr)
	})

	t.Run("an operator CA is added to the constrained pool", func(t *testing.T) {
		stubConstrainedNSSRoot(t, ca, func([]*x509.Certificate) error { return errDistrusted })
		stubSystemCertPool(t, poolWith(t, caPEM), nil)
		userCA := makeSelfSignedPEM(t, "private-corp-root")
		path := filepath.Join(t.TempDir(), "corp-ca.pem")
		if err := os.WriteFile(path, userCA, 0600); err != nil {
			t.Fatal(err)
		}
		pool, err := LoadUpstreamRootCAs(zap.NewNop(), path)
		if err != nil || pool == nil {
			t.Fatalf("pool=%v err=%v", pool, err)
		}
		if !verifiesAgainst(t, userCA, pool) {
			t.Error("the configured CA is not in the pool")
		}
		_, verr := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "upstream.example"})
		requireDistrusted(t, verr)
	})

	t.Run("a platform pool with more roots is left to crypto/tls", func(t *testing.T) {
		stubConstrainedNSSRoot(t, ca, func([]*x509.Certificate) error { return errDistrusted })
		stubSystemCertPool(t, poolWith(t, caPEM, makeSelfSignedPEM(t, "distro-root")), nil)
		pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
		if err != nil || pool != nil {
			t.Fatalf("expected nil (Go's default); pool=%v err=%v", pool, err)
		}
	})
}

// The real embedded list, read back from its PEM rendering (what an image with
// no bundle gets when SSL_CERT_FILE names a file it was written to), is
// recognised as that list.
func TestLoadUpstreamRootCAs_SystemPoolIsTheRealEmbeddedList_UsesNSSRootPool(t *testing.T) {
	stubSystemCertPool(t, poolWith(t, embeddedFallbackRoots), nil)

	pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
	if err != nil || pool == nil {
		t.Fatalf("expected the NSS root pool; pool=%v err=%v", pool, err)
	}
	want, _ := nssRootPool(nssRoots())
	if !pool.Equal(want) {
		t.Error("upstream pool does not hold exactly the NSS roots")
	}
}

// The last-resort error must not tell operators to install `ca-certificates`
// in keploy's image; the fix it names is a bundle of their own.
func TestLoadUpstreamRootCAs_NothingLoadable_NextStepNamesSSLCertFile(t *testing.T) {
	stubSystemCertPool(t, x509.NewCertPool(), nil)
	stubSystemCABundle(t, nil, "")

	logger, logs := newObservedLogger(t)
	if _, err := LoadUpstreamRootCAs(logger, ""); err != nil {
		t.Fatal(err)
	}
	errs := logs.FilterLevelExact(zap.ErrorLevel).All()
	if len(errs) != 1 {
		t.Fatalf("expected one ERROR; got %v", logs.All())
	}
	next, _ := errs[0].ContextMap()["next_step"].(string)
	if !strings.Contains(next, "SSL_CERT_FILE") || strings.Contains(next, "ca-certificates") {
		t.Fatalf("next_step = %q", next)
	}
}

func newTestCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func newTestLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dns},
		DNSNames:     []string{dns},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// With no platform pool, keploy's bundle can be the embedded roots followed by
// certificates SSL_CERT_DIR added. The embedded part keeps its NSS constraints
// — even when an added certificate repeats an NSS root, which must not replace
// the constrained copy — and the added certificates are trusted too.
//
// FAILS BEFORE THE FIX: only a bundle exactly equal to the embedded roots got
// the constrained pool; this one was read as plain PEM, so the repeated root
// verified the chain unconstrained.
func TestLoadUpstreamRootCAs_EmbeddedRootsPlusCertDir_KeepsConstraints(t *testing.T) {
	ca, caKey := newTestCA(t, "nss-constrained-root")
	leaf := newTestLeaf(t, ca, caKey, "upstream.example")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
	corp := makeSelfSignedPEM(t, "corp-root")
	bundle := concat(embeddedFallbackRoots, caPEM, corp)

	for _, tc := range []struct {
		name       string
		constraint func([]*x509.Certificate) error
	}{
		{"control: an unconstrained root verifies", nil},
		{"the constraint is enforced", func([]*x509.Certificate) error { return errDistrusted }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubConstrainedNSSRoot(t, ca, tc.constraint)
			stubSystemCertPool(t, x509.NewCertPool(), nil)
			stubSystemCABundle(t, bundle, systemCABundleSourceEmbedded+",/corp/certs")

			pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
			if err != nil || pool == nil {
				t.Fatalf("pool=%v err=%v", pool, err)
			}
			if !verifiesAgainst(t, corp, pool) {
				t.Error("the SSL_CERT_DIR certificate is not in the pool")
			}
			_, verr := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "upstream.example"})
			if tc.constraint == nil {
				if verr != nil {
					t.Fatalf("leaf.Verify: %v", verr)
				}
				return
			}
			requireDistrusted(t, verr)
		})
	}
}

// On Linux, Go's pool is the trust-store files' certificates. An agent with
// no CA bundle file and SSL_CERT_DIR naming a corporate CA — what keploy's own
// hint suggests — has a Go pool of that CA alone. keploy's upstream
// verification must use what the apps' bundle uses: the embedded roots (with
// their constraints) plus the corporate CA. A platform-verifier pool (macOS,
// Windows) is left alone.
//
// FAILS BEFORE THE FIX: the corporate-only pool was handed back as "system",
// so keploy rejected every public upstream while the app trusted them.
func TestLoadUpstreamRootCAs_FileBackedPoolWithoutABundleFile_UsesTheEmbeddedRoots(t *testing.T) {
	ca, caKey := newTestCA(t, "public-root-stand-in")
	leaf := newTestLeaf(t, ca, caKey, "upstream.example")
	corp := makeSelfSignedPEM(t, "corp-root")

	for _, tc := range []struct {
		name       string
		fileBacked bool
		wantPool   bool
	}{
		{"file-backed (Linux)", true, true},
		{"platform verifier (macOS, Windows)", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubConstrainedNSSRoot(t, ca, nil)
			stubSystemCertPool(t, poolWith(t, corp), nil)
			stubFileBackedPool(t, tc.fileBacked)
			stubSystemCABundle(t, concat(embeddedFallbackRoots, corp), systemCABundleSourceEmbedded+",/corp/certs")

			pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantPool {
				if pool != nil {
					t.Fatal("a platform-verifier pool must be left to crypto/tls (nil)")
				}
				return
			}
			if pool == nil {
				t.Fatal("expected the embedded roots plus the corporate CA; got nil, i.e. the corporate CA alone")
			}
			if _, verr := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "upstream.example"}); verr != nil {
				t.Fatalf("a public upstream does not verify: %v", verr)
			}
			if !verifiesAgainst(t, corp, pool) {
				t.Error("the corporate CA is not in the pool")
			}
		})
	}

	t.Run("file-backed: the built-in roots keep their constraints", func(t *testing.T) {
		stubConstrainedNSSRoot(t, ca, func([]*x509.Certificate) error { return errDistrusted })
		stubSystemCertPool(t, poolWith(t, corp), nil)
		stubFileBackedPool(t, true)
		stubSystemCABundle(t, concat(embeddedFallbackRoots, corp), systemCABundleSourceEmbedded+",/corp/certs")
		pool, err := LoadUpstreamRootCAs(zap.NewNop(), "")
		if err != nil || pool == nil {
			t.Fatalf("pool=%v err=%v", pool, err)
		}
		_, verr := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "upstream.example"})
		requireDistrusted(t, verr)
	})

	t.Run("an agent with a CA bundle file keeps Go's pool", func(t *testing.T) {
		stubSystemCertPool(t, poolWith(t, corp), nil)
		stubFileBackedPool(t, true)
		stubSystemCABundle(t, corp, "/etc/ssl/certs/ca-certificates.crt")
		if pool, err := LoadUpstreamRootCAs(zap.NewNop(), ""); err != nil || pool != nil {
			t.Fatalf("expected nil (Go's default); pool=%v err=%v", pool, err)
		}
	})
}
