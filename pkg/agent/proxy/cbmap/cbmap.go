// Package cbmap publishes a "channel binding hash map" — pairs of
// (H(mitm_cert), H(real_upstream_cert)) — to a file on a shared volume
// that an LD_PRELOAD shim in the app container reads. The shim swaps
// the hash libpq computes during SCRAM-SHA-256-PLUS channel binding,
// turning a FATAL channel-binding mismatch into a successful auth even
// though keploy is doing TLS MITM.
//
// Why this exists: SCRAM channel binding ties the auth proof to the
// TLS server cert. Under keploy's MITM proxy, the app sees keploy's
// cert while postgres sees its own — the hashes diverge and the proof
// is rejected with "FATAL: SCRAM channel binding check failed". The
// shim resolves this by substituting the real upstream cert's hash
// inside the app process, before libpq builds the proof. This package
// is keploy's side of that handshake: it writes the table the shim
// consumes.
//
// Hash format per RFC 5929 §4.1 "tls-server-end-point": hash algorithm
// is derived from the cert's signature algorithm, with MD5 / SHA-1
// normalized to SHA-256, and SHA-384 / SHA-512 kept as-is. Anything
// else also normalizes to SHA-256.
//
// File format (one entry per line, two hex strings separated by space):
//
//	<H(mitm_cert)_hex>  <H(real_cert)_hex>
//
// Writes are atomic via tmpfile + rename so partial writes are never
// observed by the shim. Duplicate entries are skipped.
package cbmap

import (
	"crypto"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"go.uber.org/zap"
)

// DefaultPath is where the table is written when KEPLOY_CBMAP_PATH is
// not set. Matches the shared volume already used by SetupCA for the
// merged CA bundle (`/tmp/keploy-tls/`).
const DefaultPath = "/tmp/keploy-tls/cbmap.txt"

// Path returns the output file path, honoring KEPLOY_CBMAP_PATH.
func Path() string {
	if p := os.Getenv("KEPLOY_CBMAP_PATH"); p != "" {
		return p
	}
	return DefaultPath
}

// pair is one channel-binding mapping. Stored deduplicated in mem.
type pair struct {
	mitmHex string
	realHex string
}

var (
	mu      sync.Mutex
	entries = map[string]pair{} // key = mitmHex
)

// ---------------------------------------------------------------------------
// Deferred-publish API (RegisterMITM / RegisterReal / CleanupConnection)
// ---------------------------------------------------------------------------
//
// The proxy observes the two halves of a Postgres connection at different
// points in time:
//
//   - MITM cert: minted inside CertForClient when the app-side TLS handshake
//     runs. Side: pkg/agent/proxy/tls/ca.go.
//
//   - Real upstream cert: extracted from the upstream tls.Conn after the
//     keploy proxy completes its handshake to the real Postgres. Side:
//     dialPostgresSSLUpstream (raw InterceptPostgresSSLRequest flow) OR
//     the Postgres v3 parser's session_capture (TLSUpgrader flow).
//
// Their relative order is NOT stable — the raw-intercept flow dials upstream
// before the app's TLS finishes, while the parser-driven flow does the
// opposite. Rather than depending on either ordering, both call sites
// register their half against a stable connection identifier (the app's
// source port), and the second arrival triggers Publish.
//
// CleanupConnection removes orphan halves when a connection dies before
// both certs arrive (e.g. upstream dial failed, app disconnected).

type pendingPair struct {
	mitm    []byte
	real    []byte
	sigAlgo x509.SignatureAlgorithm
}

var (
	pendingMu sync.Mutex
	pending   = map[string]*pendingPair{} // key: connID
)

// RegisterMITM records the MITM leaf certificate keploy serves to the app
// for the connection identified by connID. If the real upstream cert has
// already been registered for this connID, the pair is published and the
// pending entry is removed. Otherwise the MITM half is stashed until
// RegisterReal arrives.
//
// Safe to call concurrently. Empty connID or empty mitmDER are silently
// dropped (the proxy may invoke this from edge-case paths where the
// cert isn't yet available).
func RegisterMITM(logger *zap.Logger, connID string, mitmDER []byte) {
	if connID == "" || len(mitmDER) == 0 {
		return
	}
	ready, mitm, real, sig := registerHalf(connID, mitmDER, nil, 0)
	if ready {
		if _, err := Publish(logger, mitm, real, sig); err != nil {
			logger.Debug("cbmap: deferred publish failed", zap.String("conn_id", connID), zap.Error(err))
		}
	}
}

// RegisterReal records the real upstream Postgres leaf certificate for the
// connection identified by connID. Symmetric to RegisterMITM.
func RegisterReal(logger *zap.Logger, connID string, realDER []byte, sigAlgo x509.SignatureAlgorithm) {
	if connID == "" || len(realDER) == 0 {
		return
	}
	ready, mitm, real, sig := registerHalf(connID, nil, realDER, sigAlgo)
	if ready {
		if _, err := Publish(logger, mitm, real, sig); err != nil {
			logger.Debug("cbmap: deferred publish failed", zap.String("conn_id", connID), zap.Error(err))
		}
	}
}

// registerHalf merges one or the other side into the pending map under lock,
// and reports whether both halves are now present (in which case the caller
// runs Publish OUTSIDE the lock — Publish writes a file and we don't want
// to hold the pending map across I/O). Exactly one of mitmDER / realDER is
// non-nil per call.
func registerHalf(connID string, mitmDER, realDER []byte, sigAlgo x509.SignatureAlgorithm) (ready bool, mitm []byte, real []byte, sig x509.SignatureAlgorithm) {
	pendingMu.Lock()
	defer pendingMu.Unlock()

	p, ok := pending[connID]
	if !ok {
		p = &pendingPair{}
		pending[connID] = p
	}
	if mitmDER != nil {
		p.mitm = mitmDER
	}
	if realDER != nil {
		p.real = realDER
		p.sigAlgo = sigAlgo
	}
	if p.mitm == nil || p.real == nil {
		return false, nil, nil, 0
	}
	mitm, real, sig = p.mitm, p.real, p.sigAlgo
	delete(pending, connID)
	return true, mitm, real, sig
}

// CleanupConnection drops any pending half registered for connID without
// publishing. Call when a connection terminates so half-arrived state
// doesn't accumulate.
func CleanupConnection(connID string) {
	if connID == "" {
		return
	}
	pendingMu.Lock()
	delete(pending, connID)
	pendingMu.Unlock()
}

// pendingSnapshot is a test helper that returns the count and keys of
// the current pending map. Not part of the public API surface; lives
// here rather than in the test file so it can read the package globals.
func pendingSnapshot() (int, []string) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	keys := make([]string, 0, len(pending))
	for k := range pending {
		keys = append(keys, k)
	}
	return len(pending), keys
}

// Publish records the mapping H(mitm_cert) -> H(real_cert) and
// rewrites the shared file atomically. Safe to call concurrently and
// repeatedly with the same inputs — duplicate mappings are no-ops.
//
// Returns the path written to (or would have written to on error).
func Publish(logger *zap.Logger, mitmDER, realDER []byte, sigAlgo x509.SignatureAlgorithm) (string, error) {
	if len(mitmDER) == 0 || len(realDER) == 0 {
		return Path(), fmt.Errorf("cbmap: empty DER provided")
	}

	h := hashForCBSig(sigAlgo)
	h.Write(mitmDER)
	mitmHash := h.Sum(nil)
	h.Reset()
	h.Write(realDER)
	realHash := h.Sum(nil)

	mitmHex := hex.EncodeToString(mitmHash)
	realHex := hex.EncodeToString(realHash)

	mu.Lock()
	defer mu.Unlock()

	if existing, ok := entries[mitmHex]; ok && existing.realHex == realHex {
		// Already published — nothing to do.
		return Path(), nil
	}
	entries[mitmHex] = pair{mitmHex: mitmHex, realHex: realHex}

	path := Path()
	if err := writeAtomic(path, entries); err != nil {
		logger.Debug("cbmap: write failed",
			zap.String("path", path), zap.Error(err))
		return path, err
	}
	logger.Debug("cbmap: published",
		zap.String("path", path),
		zap.String("mitm_hash", mitmHex[:16]+"..."),
		zap.String("real_hash", realHex[:16]+"..."))
	return path, nil
}

// hashForCBSig returns the channel-binding hash function per RFC 5929
// §4.1, given a cert's signature algorithm.
//
// Rules:
//   - MD5* / SHA1* signatures   → SHA-256
//   - SHA-384* signatures       → SHA-384
//   - SHA-512* signatures       → SHA-512
//   - Anything else (incl. Ed25519, RSA-PSS without an algorithm
//     identifier, SHA-256) → SHA-256
//
// The function matches libpq's pgtls_get_peer_certificate_hash exactly,
// so the hash on both sides of the MITM stays byte-comparable.
func hashForCBSig(alg x509.SignatureAlgorithm) hash.Hash {
	switch alg {
	case x509.SHA384WithRSA, x509.ECDSAWithSHA384, x509.SHA384WithRSAPSS:
		return sha512.New384()
	case x509.SHA512WithRSA, x509.ECDSAWithSHA512, x509.SHA512WithRSAPSS:
		return sha512.New()
	default:
		// Includes MD5/SHA-1 (normalize to SHA-256 per RFC), SHA-256
		// variants (already SHA-256), Ed25519 (RFC 8410 leaves this
		// implementation-defined; we follow libpq's SHA-256 default).
		_ = crypto.SHA256
		return sha256.New()
	}
}

// writeAtomic serializes the table to a tmpfile in the same directory
// and renames it into place. The rename is atomic on POSIX so readers
// either see the old table or the new one — never a half-written file.
func writeAtomic(path string, m map[string]pair) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// Sort by mitmHex for stable ordering across writes — keeps the file
	// diff-friendly and makes any test that snapshots it deterministic.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	tmp, err := os.CreateTemp(dir, "cbmap-*.tmp")
	if err != nil {
		return fmt.Errorf("create tmp in %s: %w", dir, err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name()) // no-op if rename succeeded
	}()

	for _, k := range keys {
		if _, err := fmt.Fprintf(tmp, "%s %s\n", k, m[k].realHex); err != nil {
			return fmt.Errorf("write tmp: %w", err)
		}
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	// File mode 0644 so the app's container can read it. The shared
	// volume is mounted by both keploy-agent and the app, so this is
	// the minimum permission needed.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("chmod tmp: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp.Name(), path, err)
	}
	return nil
}
