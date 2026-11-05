package tls

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"iter"
	"os"
	"runtime"

	"go.uber.org/zap"
	nssbundle "golang.org/x/crypto/x509roots/fallback/bundle"
)

// systemPoolIsFileBacked says crypto/x509's system pool is the certificates of
// the trust-store files — Linux, where keploy's loader reads the same files
// (systemCABundleSearchPaths holds every one of Go's; see
// TestSystemCABundleSearchPaths_CoverGosLinuxFiles), so keploy's upstream
// verification uses the loader's bundle in its place — rather than a platform
// verifier. A variable for tests.
var systemPoolIsFileBacked = runtime.GOOS == "linux"

// systemCertPoolFn is an indirection seam for tests, mirroring
// loadSystemCABundleFn above it in ca.go. Production code always uses
// crypto/x509's own SystemCertPool.
var systemCertPoolFn = x509.SystemCertPool

// LoadUpstreamRootCAs builds the trust-anchor pool keploy uses when it verifies
// the REAL upstream server on its own outbound dials (record.upstreamTls.verify).
//
// It is the mirror image of the rest of this file's CA machinery: everything
// else here makes the APPLICATION trust keploy's MITM CA; this makes KEPLOY
// trust the destination it is recording against.
//
// Contract:
//
//   - (nil, nil) means "use Go's default" — the caller leaves tls.Config.RootCAs
//     nil and crypto/tls falls back to the platform root pool (and, on
//     macOS/Windows, the platform verifier). It NEVER means "trust nothing";
//     an empty non-nil pool would fail every handshake, so this function only
//     ever returns a pool it has actually populated.
//   - A non-nil pool is the roots of the trust bundle keploy gives the apps
//     (always on Linux; elsewhere when the platform pool is unusable) or the
//     platform roots, plus every certificate in caCertPath.
//   - A non-nil error names caCertPath and means the operator's configuration is
//     wrong — the file could not be read, or held no PEM certificates. Callers
//     must surface it rather than quietly continuing, because a verifying dial
//     against roots that are missing the operator's CA fails the dest-side
//     handshake, and keploy's supervisor reacts to that by falling through to
//     raw passthrough — the app keeps working and the mock is silently dropped.
//
// caCertPath == "" is the common case: on macOS and Windows with a usable
// platform pool we return (nil, nil) and let crypto/tls do exactly what it
// would have done unconfigured. On Linux it costs one run of keploy's
// trust-bundle loader (systemOrEmbeddedRootPool), once per agent process.
func LoadUpstreamRootCAs(logger *zap.Logger, caCertPath string) (*x509.CertPool, error) {
	pool, source := systemOrEmbeddedRootPool(logger)

	if caCertPath == "" {
		switch source {
		case upstreamRootSourceSystem:
			// Nothing to add and the platform pool is healthy — hand back nil so
			// crypto/tls uses its own roots. On macOS/Windows that additionally
			// keeps the platform verifier in play, which an explicit pool would
			// bypass.
			return nil, nil
		case upstreamRootSourceEmpty:
			// We could not populate anything. Returning the empty pool would be
			// the "trust nothing" outcome this function promises never to
			// produce, so fall back to Go's default; systemOrEmbeddedRootPool
			// has already logged the remediation.
			return nil, nil
		case upstreamRootSourceEmbedded:
			// The platform pool IS the Mozilla list built into keploy, read
			// from a PEM file that cannot carry Mozilla's distrust-after
			// constraints. Returning nil would verify against that
			// unconstrained pool, so return the constrained one.
			logger.Debug("upstream TLS verification uses the Mozilla root list built into keploy, with its constraints",
				zap.String("reason", "the platform pool is that list read back from a PEM file, which drops its constraints"),
				zap.String("source", source))
			return pool, nil
		case upstreamRootSourceTrustBundle:
			// Linux: Go's pool is not what the apps trust (see
			// systemOrEmbeddedRootPool); returning nil would verify against it.
			logger.Debug("upstream TLS verification uses the roots of the trust bundle keploy gives the apps",
				zap.String("source", source))
			return pool, nil
		default:
			// The platform pool was unusable and we substituted keploy's own
			// bundle. Returning nil here would leave the caller verifying
			// against the same empty store we just rejected, so return the pool.
			logger.Info("upstream TLS verification will use keploy's own CA bundle",
				zap.String("reason", "the platform root pool was unavailable or empty"),
				zap.String("source", source))
			return pool, nil
		}
	}

	pemBytes, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read upstream TLS CA certificate %q: %w", caCertPath, err)
	}
	// AppendCertsFromPEM reports false only when it parsed ZERO certificates —
	// a truncated file, a DER blob saved with a .pem name, or a PEM that only
	// carries a private key. It reports TRUE when it parsed at least one, so a
	// corporate bundle with one malformed anchor among ten loads "successfully"
	// while quietly dropping that anchor, and the operator finds out as
	// dest-side handshake failures that fall through to raw passthrough and
	// drop mocks with no configuration error anywhere. Count the CERTIFICATE
	// blocks ourselves so the discrepancy is named.
	wanted, offered := countPEMCertificateBlocks(pemBytes)
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("upstream TLS CA certificate %q contains no usable PEM certificate blocks (%d bytes read, %d CERTIFICATE block(s) found); expected one or more parseable -----BEGIN CERTIFICATE----- blocks", caCertPath, len(pemBytes), wanted)
	}
	if wanted > offered {
		// Loud, but not fatal: the anchors that DID parse are real, and
		// refusing the whole bundle would take verification down over one bad
		// block. Naming the count is what turns an inexplicable handshake
		// failure into a one-line fix.
		logger.Warn("some certificates in the upstream TLS CA bundle could not be parsed and are NOT trusted",
			zap.String("ca_cert", caCertPath),
			zap.Int("certificate_blocks", wanted),
			zap.Int("loaded", offered),
			zap.String("next_step", "the skipped blocks are malformed or truncated; re-export the bundle (openssl x509 -in <file> -noout -text over each block shows which one fails) — until then any upstream signed by a skipped anchor will fail verification and its mock will be dropped"))
	}

	logger.Info("loaded extra trust anchors for upstream TLS verification",
		zap.String("ca_cert", caCertPath),
		zap.Int("certificates", offered),
		zap.String("base_roots", source))
	return pool, nil
}

// countPEMCertificateBlocks reports how many CERTIFICATE blocks the bundle
// CONTAINS and how many of them x509 can actually parse.
//
// It mirrors what AppendCertsFromPEM does internally — same pem.Decode loop,
// same "CERTIFICATE type with no headers" filter, same ParseCertificate — so
// the two counts are directly comparable. Anything AppendCertsFromPEM skips
// silently (a non-CERTIFICATE block, a block carrying headers) is not counted
// as wanted either; only blocks that ASK to be trust anchors and fail to parse
// open a gap between the two numbers.
func countPEMCertificateBlocks(pemBytes []byte) (wanted, parseable int) {
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return wanted, parseable
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			continue
		}
		wanted++
		if _, err := x509.ParseCertificate(block.Bytes); err == nil {
			parseable++
		}
	}
}

// Sources reported by systemOrEmbeddedRootPool, and echoed in the logs above so
// an operator debugging a verification failure can tell which trust store the
// agent actually used.
const (
	upstreamRootSourceSystem = "system"
	// upstreamRootSourceEmbedded is a platform pool holding exactly the
	// compiled-in Mozilla NSS roots, replaced by the same roots with their
	// constraints.
	upstreamRootSourceEmbedded = "embedded_mozilla_roots"
	// upstreamRootSourceTrustBundle is, on Linux, the pool of the trust
	// bundle keploy's loader builds for the apps (trustBundlePool).
	upstreamRootSourceTrustBundle = "trust_bundle"
	// upstreamRootSourceKeployBundle is the same bundle standing in for a
	// platform pool that was unavailable or empty (macOS, Windows).
	upstreamRootSourceKeployBundle = "keploy_bundle"
	upstreamRootSourceEmpty        = "empty"
)

// systemOrEmbeddedRootPool returns a populated root pool plus the source label.
//
// On Linux (systemPoolIsFileBacked) it is the pool of the trust bundle
// keploy's loader builds for the apps (trustBundlePool), never Go's own pool.
// Both are the certificates of the same trust-store files, but the loader
// departs from crypto/x509 where Go would leave no public roots
// (loadSystemCABundle lists how) and drops keploy's own CAs. Go's pool can
// then hold far less than the apps trust: with SSL_CERT_FILE naming a missing
// file and SSL_CERT_DIR a corporate CA on a Debian host, Go reads the
// corporate CA alone, the apps get the Debian bundle plus it, and verifying
// against Go's pool would reject every public upstream the apps trust (each
// connection then falls through to passthrough and its mock is dropped).
// With one pool, keploy verifies upstreams against exactly what the apps
// trust.
//
// Elsewhere it is the platform pool. x509.SystemCertPool does not fail when
// there is no trust store: it returns a non-nil, EMPTY pool with a nil error,
// and handing an empty pool to tls.Config.RootCAs would reject every upstream,
// so emptiness is treated exactly like an error, and keploy's trust bundle
// stands in.
//
// Emptiness is probed with len(Subjects()). Subjects is deprecated because it
// omits the system roots for the platform-verifier-backed pools on macOS and
// Windows — there it under-reports and routes us into the keploy bundle below.
// That is a benign degradation (keploy's own disk search finds /etc/ssl/cert.pem
// on macOS, and the embedded Mozilla roots otherwise: a valid trust store either
// way, just without the platform verifier).
//
// A platform pool that holds exactly the compiled-in Mozilla roots is the same
// list read back from a PEM file (SSL_CERT_FILE naming a file the roots were
// written to, which Go reads on macOS and Windows too since Go 1.27). PEM
// drops Mozilla's distrust-after constraints, so that pool is swapped for the
// constrained one, as the trust bundle's embedded roots are.
func systemOrEmbeddedRootPool(logger *zap.Logger) (*x509.CertPool, string) {
	if systemPoolIsFileBacked {
		return trustBundlePool(logger, upstreamRootSourceTrustBundle)
	}
	//nolint:staticcheck // SA1019: Subjects is deprecated only because it under-reports platform-verifier pools; we need a count, not the contents, and an under-report here degrades safely into the keploy bundle below.
	if pool, err := systemCertPoolFn(); err == nil && pool != nil && len(pool.Subjects()) > 0 {
		if nss, n := nssRootPool(nssRoots()); n > 0 && pool.Equal(nss) {
			return nss, upstreamRootSourceEmbedded
		}
		return pool, upstreamRootSourceSystem
	}
	return trustBundlePool(logger, upstreamRootSourceKeployBundle)
}

// trustBundlePool returns the pool of the trust bundle keploy's loader builds
// for the apps (it honours SSL_CERT_FILE / SSL_CERT_DIR, walks
// systemCABundleSearchPaths and falls back to the compiled-in Mozilla NSS
// roots), labelled source. Wherever that bundle holds the NSS roots — first,
// as the loader puts the embedded ones, or as the whole bundle — they keep
// their NSS constraints.
func trustBundlePool(logger *zap.Logger, source string) (*x509.CertPool, string) {
	bundle, _ := loadSystemCABundleFn(logger)
	if embedded, ok := embeddedRootsPool(bundle); ok {
		return embedded, source
	}
	pool := x509.NewCertPool()
	if pool.AppendCertsFromPEM(bundle) {
		if nss, _ := nssRootPool(nssRoots()); pool.Equal(nss) {
			return nss, source
		}
		return pool, source
	}

	// Only reachable if the compiled-in bundle itself is empty or unparseable,
	// which cannot happen in a production build.
	// Report it as empty so LoadUpstreamRootCAs does not pass an empty pool off
	// as a trust store.
	logger.Error("no usable trust anchors for upstream TLS verification",
		zap.String("next_step", "mount a PEM CA bundle into the keploy agent and set SSL_CERT_FILE to it, or set record.upstreamTls.caCert to a PEM bundle readable by the agent"))
	return pool, upstreamRootSourceEmpty
}

// embeddedRootsPool returns the pool for a bundle of keploy's loader that is
// the embedded roots, perhaps followed by certificates SSL_CERT_DIR added: the
// roots built from the NSS bundle itself rather than its PEM rendering, so a
// root's "distrust after" constraint (which X.509 cannot carry) is enforced
// for keploy's own upstream verification, plus the rest as they are. A
// certificate that repeats an NSS root does not displace it: the pool keeps
// the first copy. ok is false for any other bundle.
func embeddedRootsPool(bundle []byte) (*x509.CertPool, bool) {
	if !bytes.HasPrefix(bundle, embeddedFallbackRoots) {
		return nil, false
	}
	pool, n := nssRootPool(nssRoots())
	if n == 0 {
		return nil, false
	}
	pool.AppendCertsFromPEM(bundle[len(embeddedFallbackRoots):])
	return pool, true
}

// nssRoots yields the compiled-in Mozilla NSS root set; a variable so tests can
// inject roots with constraints.
var nssRoots = nssbundle.Roots

// nssRootPool turns NSS roots into a pool, attaching each root's NSS constraint
// (e.g. distrust-after) via AddCertWithConstraint. It returns the number of
// roots added.
func nssRootPool(roots iter.Seq[nssbundle.Root]) (*x509.CertPool, int) {
	pool := x509.NewCertPool()
	n := 0
	for root := range roots {
		cert, err := x509.ParseCertificate(root.Certificate)
		if err != nil {
			continue
		}
		if root.Constraint != nil {
			pool.AddCertWithConstraint(cert, root.Constraint)
		} else {
			pool.AddCert(cert)
		}
		n++
	}
	return pool, n
}
