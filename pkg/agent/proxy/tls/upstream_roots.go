package tls

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"go.uber.org/zap"
)

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
//   - A non-nil pool is the system roots (or, on an image with no trust store,
//     keploy's embedded Mozilla NSS roots) plus every certificate in caCertPath.
//   - A non-nil error names caCertPath and means the operator's configuration is
//     wrong — the file could not be read, or held no PEM certificates. Callers
//     must surface it rather than quietly continuing, because a verifying dial
//     against roots that are missing the operator's CA fails the dest-side
//     handshake, and keploy's supervisor reacts to that by falling through to
//     raw passthrough — the app keeps working and the mock is silently dropped.
//
// caCertPath == "" is the common case and costs one SystemCertPool probe: if the
// host has a usable trust store we return (nil, nil) and let crypto/tls do
// exactly what it would have done unconfigured.
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
	// upstreamRootSourceKeployBundle covers both halves of loadSystemCABundle's
	// own fallback chain (a disk bundle found by systemCABundleSearchPaths, or
	// the go:embed'd Mozilla roots); loadSystemCABundle logs which one it took.
	upstreamRootSourceKeployBundle = "keploy_bundle"
	upstreamRootSourceEmpty        = "empty"
)

// systemOrEmbeddedRootPool returns a populated root pool plus the source label.
//
// x509.SystemCertPool on Unix does not fail when the image has no trust store —
// it returns a non-nil, EMPTY pool with a nil error. That is precisely the
// distroless/scratch shape keploy already carries embeddedFallbackRoots for, and
// handing an empty pool to tls.Config.RootCAs would reject every upstream, so
// emptiness is treated exactly like an error here.
//
// Emptiness is probed with len(Subjects()). Subjects is deprecated because it
// omits the system roots for the platform-verifier-backed pools on macOS and
// Windows — there it under-reports and routes us into the keploy bundle below.
// That is a benign degradation (keploy's own disk search finds /etc/ssl/cert.pem
// on macOS, and the embedded Mozilla roots otherwise: a valid trust store either
// way, just without the platform verifier), and the agent's proxy is a
// Linux-only path in practice, where the pool is file-backed and the count is
// accurate.
func systemOrEmbeddedRootPool(logger *zap.Logger) (*x509.CertPool, string) {
	//nolint:staticcheck // SA1019: Subjects is deprecated only because it under-reports platform-verifier pools; we need a count, not the contents, and an under-report here degrades safely into the keploy bundle below.
	if pool, err := systemCertPoolFn(); err == nil && pool != nil && len(pool.Subjects()) > 0 {
		return pool, upstreamRootSourceSystem
	}

	// Reuse the loader the shared-volume path already uses: it walks
	// systemCABundleSearchPaths and falls back to the go:embed'd Mozilla NSS
	// roots, logging the severity of a missing disk bundle for us.
	bundle, _ := loadSystemCABundleFn(logger)
	pool := x509.NewCertPool()
	if len(bundle) > 0 && pool.AppendCertsFromPEM(bundle) {
		return pool, upstreamRootSourceKeployBundle
	}

	// Only reachable if the embedded bundle itself is missing or unparseable,
	// which go:embed makes a build-time impossibility in production builds.
	// Report it as empty so LoadUpstreamRootCAs does not pass an empty pool off
	// as a trust store.
	logger.Error("no usable trust anchors for upstream TLS verification",
		zap.String("next_step", "install `ca-certificates` in the keploy agent image, or set record.upstreamTls.caCert to a PEM bundle readable by the agent"))
	return pool, upstreamRootSourceEmpty
}
