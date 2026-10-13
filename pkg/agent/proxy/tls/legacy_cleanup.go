package tls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.uber.org/zap"
)

// legacyCASHA256Hex is the SHA-256 fingerprint of the retired static MITM CA
// (CN="My Custom CA", the committed asset/ca.crt whose private key was public).
// Cleanup matches on this fingerprint ONLY — never on a filename or Common Name
// — so it can never remove an unrelated "ca.crt" or a user's own CA that happens
// to share the generic name keploy used to write.
const legacyCASHA256Hex = "8264534c461d651bee1ac9de781e96751eb103a069a23a1afa37c856a23ff17e"

// legacyCAAlias is the fixed JDK keystore alias the old native install used.
// A modern run uses a per-run alias instead (see setupNativeForApp), so this
// alias belongs only to a legacy import and is removed once its stored cert's
// fingerprint confirms it is the retired CA.
const legacyCAAlias = "keployCA"

// legacyCAFingerprint returns the parsed legacy fingerprint, or ok=false if the
// constant is somehow malformed (a build-time invariant, checked defensively).
func legacyCAFingerprint() (fp [32]byte, ok bool) {
	b, err := hex.DecodeString(legacyCASHA256Hex)
	if err != nil || len(b) != 32 {
		return fp, false
	}
	copy(fp[:], b)
	return fp, true
}

// SweepLegacyCA removes every trace of the retired static MITM CA this host may
// carry from past keploy runs or from the documented "bake it into the image"
// recipe: the anchor files in the system store, the fixed JDK keystore alias,
// and the world-writable temp files the old client left behind. It is
// best-effort and idempotent — a host that was never touched, or already
// cleaned, is a no-op. With fresh=true the system trust bundle is rebuilt from
// scratch (update-ca-certificates --fresh on Debian) so hash-linked copies of a
// removed anchor cannot linger.
func SweepLegacyCA(ctx context.Context, logger *zap.Logger, fresh bool) {
	sweepLegacyCA(ctx, logger, fresh, false)
}

// SweepLegacyCAAggressive is the explicit `keploy ca clean` path. It does
// everything SweepLegacyCA does and also removes the leaked per-uid Java
// truststores. That removal is not fingerprint-gated, so it is kept out of the
// automatic per-run boot sweep (two concurrent runs share the per-uid truststore
// name, and the boot sweep of one could delete the other's live store).
func SweepLegacyCAAggressive(ctx context.Context, logger *zap.Logger, fresh bool) {
	sweepLegacyCA(ctx, logger, fresh, true)
}

func sweepLegacyCA(ctx context.Context, logger *zap.Logger, fresh, includeTruststores bool) {
	fp, ok := legacyCAFingerprint()
	if !ok {
		logger.Debug("legacy CA fingerprint constant is malformed; skipping cleanup")
		return
	}
	sweepCAByFingerprint(ctx, logger, fp, fresh, includeTruststores)
}

// sweepCAByFingerprint is sweepLegacyCA for a given fingerprint (the retired
// CA's in production; a test passes one it can make a certificate for).
//
// The anchors are removed, the store regenerated and the JDK keystore
// rewritten under the trust-store lock, like every other change keploy makes
// to the store: other keploy agents on this machine regenerate from the same
// anchors and rewrite the same keystore, and an anchor unlinked while one of
// them is reading it fails that agent's regeneration. Only a host that still
// has the retired CA takes the lock; the common case only reads.
func sweepCAByFingerprint(ctx context.Context, logger *zap.Logger, fp [32]byte, fresh, includeTruststores bool) {
	if anchors := findLegacyAnchors(fp); len(anchors) > 0 {
		removeLegacyAnchors(ctx, logger, anchors, fresh)
	}

	removeLegacyJavaAlias(ctx, logger, fp)
	removeLegacyTempFiles(logger, fp, includeTruststores)
}

// findLegacyAnchors lists, from each system CA directory, every file that is
// exactly one certificate whose fingerprint is the retired CA's. Requiring a
// single-certificate file means a shared bundle (ca-certificates.crt,
// ca-bundle.crt) is never touched — only the standalone anchor keploy wrote.
func findLegacyAnchors(fp [32]byte) []string {
	var found []string
	for _, dir := range caStorePath {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if p := filepath.Join(dir, e.Name()); fileIsExactlyCert(p, fp) {
				found = append(found, p)
			}
		}
	}
	return found
}

// removeLegacyAnchors removes the given legacy anchors and regenerates the
// store without them, under the trust-store lock.
func removeLegacyAnchors(ctx context.Context, logger *zap.Logger, anchors []string, fresh bool) {
	work, unlock, err := lockTrustStore(ctx, logger)
	if err != nil {
		logger.Debug("could not lock the trust store to remove the retired static MITM CA", zap.Strings("anchors", anchors), zap.Error(err))
		return
	}
	defer unlock()
	removed := 0
	for _, p := range anchors {
		if err := os.Remove(p); err != nil {
			logger.Debug("could not remove a legacy CA anchor", zap.String("path", p), zap.Error(err))
			continue
		}
		logger.Debug("removed legacy CA anchor", zap.String("path", p))
		removed++
	}
	if removed == 0 {
		return
	}
	logger.Info("removed retired static MITM CA anchors from the system trust store", zap.Int("count", removed))
	if err := refreshCaStore(work, logger, fresh); err != nil {
		logger.Debug("could not refresh the system CA store after removing legacy anchors", zap.Error(err))
	}
}

// fileIsExactlyCert reports whether path decodes to exactly one CERTIFICATE PEM
// block whose SHA-256 matches fp. "Exactly one" is the guard against deleting a
// multi-cert system bundle.
func fileIsExactlyCert(path string, fp [32]byte) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var certDER []byte
	rest := data
	count := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return false
		}
		count++
		if count > 1 {
			return false
		}
		certDER = block.Bytes
	}
	if count != 1 || certDER == nil {
		return false
	}
	return sha256.Sum256(certDER) == fp
}

// removeLegacyJavaAlias deletes the fixed "keployCA" alias from a JDK keystore,
// but only after reading that entry back and confirming its certificate is the
// retired CA — so a user's own entry under the same alias is never clobbered.
func removeLegacyJavaAlias(ctx context.Context, logger *zap.Logger, fp [32]byte) {
	keytool, cacerts, ok := resolveKeytoolAndCacerts()
	if !ok {
		return
	}
	der, ok := exportJavaAliasCert(ctx, keytool, cacerts, legacyCAAlias)
	if !ok {
		return
	}
	if sha256.Sum256(der) != fp {
		logger.Debug("a 'keployCA' JDK alias exists but is not the retired CA; leaving it", zap.String("cacerts", cacerts))
		return
	}
	if systemTrustGenerates(cacerts) {
		// A p11-kit output (Fedora, RHEL): keytool would rewrite it in place
		// under every JVM that starts meanwhile, and the next regeneration —
		// this run's install does one — drops an entry keytool added anyway.
		return
	}
	// keytool rewrites the whole keystore file; another keploy agent importing
	// its run CA into the same cacerts at this moment would lose its entry, or
	// undo this removal.
	work, unlock, err := lockTrustStore(ctx, logger)
	if err != nil {
		logger.Debug("could not lock the trust store to delete the legacy JDK alias", zap.String("cacerts", cacerts), zap.Error(err))
		return
	}
	defer unlock()
	cmd := storeCommand(work, keytool, "-delete", "-alias", legacyCAAlias, "-keystore", cacerts, "-storepass", "changeit", "-noprompt")
	if out, err := runStoreCommand(cmd); err != nil {
		logger.Debug("could not delete the legacy JDK alias", zap.String("cacerts", cacerts), zap.ByteString("output", out), zap.Error(err))
		return
	}
	logger.Info("removed the retired MITM CA from the JDK truststore", zap.String("cacerts", cacerts))
}

// exportJavaAliasCert returns the DER of alias's certificate from cacerts, or
// ok=false when the alias is absent or keytool fails.
func exportJavaAliasCert(ctx context.Context, keytool, cacerts, alias string) (der []byte, ok bool) {
	cmd := exec.CommandContext(ctx, keytool, "-exportcert", "-rfc", "-alias", alias, "-keystore", cacerts, "-storepass", "changeit")
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	block, _ := pem.Decode(out)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, false
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, false
	}
	return block.Bytes, true
}

// resolveKeytoolAndCacerts finds a keytool and the cacerts it manages, from
// JAVA_HOME first then PATH. ok=false when no JDK is reachable.
func resolveKeytoolAndCacerts() (keytool, cacerts string, ok bool) {
	exe := "keytool"
	if runtime.GOOS == "windows" {
		exe = "keytool.exe"
	}
	if jh := os.Getenv("JAVA_HOME"); jh != "" {
		kt := filepath.Join(jh, "bin", exe)
		cc := filepath.Join(jh, "lib", "security", "cacerts")
		if fileExists(kt) && fileExists(cc) {
			return kt, cc, true
		}
	}
	kt, err := exec.LookPath(exe)
	if err != nil {
		return "", "", false
	}
	// Derive JAVA_HOME from <home>/bin/keytool -> <home>/lib/security/cacerts.
	home := filepath.Dir(filepath.Dir(kt))
	cc := filepath.Join(home, "lib", "security", "cacerts")
	if !fileExists(cc) {
		return "", "", false
	}
	return kt, cc, true
}

// removeLegacyTempFiles deletes the world-writable temp files the old client
// wrote and never cleaned: the per-process CA copies ($TMPDIR/ca.crt*) that
// contain the retired cert, and the deterministic per-uid Java truststore.
func removeLegacyTempFiles(logger *zap.Logger, fp [32]byte, includeTruststores bool) {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(tmp, name)
		switch {
		case strings.HasPrefix(name, "ca.crt"):
			// Fingerprint-gated: only the old client's temp copy of the retired
			// CA, never an unrelated file that happens to start with "ca.crt".
			if fileIsExactlyCert(p, fp) {
				if err := os.Remove(p); err == nil {
					logger.Debug("removed a leaked legacy CA temp file", zap.String("path", p))
				}
			}
		case includeTruststores && strings.HasPrefix(name, "keploy-java-truststore-"):
			// Not fingerprint-gated (it is a JKS, not a PEM), so only done on the
			// explicit `keploy ca clean` — never on the automatic boot sweep,
			// where it could race a concurrent run's live per-uid truststore.
			if err := os.Remove(p); err == nil {
				logger.Debug("removed a leaked keploy Java truststore", zap.String("path", p))
			}
		}
	}
}

// refreshCaStore rebuilds the system trust outputs from the anchors now in the
// store, with the first of caStoreUpdateCmd this host has. The caller holds the
// trust-store lock, and ctx is the one lockTrustStore returned: the distro
// tools are not safe to run two at a time.
//
// Where that tool is update-ca-trust and the store is the p11-kit one of
// Fedora and RHEL, keploy does the wrapper's work itself, without the gap the
// wrapper leaves (swapP11KitTrust).
//
// With fresh, Debian's update-ca-certificates is given --fresh, which also
// prunes stale hash-links to a deleted anchor (a plain run leaves dangling
// /etc/ssl/certs symlinks that Go ignores but that are confusing). The other
// distros' tools take no such flag; a bare run regenerates wholesale.
func refreshCaStore(ctx context.Context, logger *zap.Logger, fresh bool) error {
	tool := storeTool()
	if tool == "" {
		return fmt.Errorf("no valid CA store tools command found")
	}
	if filepath.Base(tool) == "update-ca-trust" {
		if handled, err := swapP11KitTrust(ctx, logger, tool); handled {
			return err
		}
	}
	var args []string
	if fresh && filepath.Base(tool) == "update-ca-certificates" {
		args = append(args, "--fresh")
	}
	if out, err := runStoreCommand(storeCommand(ctx, tool, args...)); err != nil {
		return fmt.Errorf("%s failed: %w: %s", tool, err, bytes.TrimSpace(out))
	}
	return nil
}

// storeTool is the path of the first of caStoreUpdateCmd this machine has: the
// tool its trust store is regenerated with. "" when it has none.
func storeTool() string {
	for _, tool := range caStoreUpdateCmd {
		if p, err := exec.LookPath(tool); err == nil {
			return p
		}
	}
	return ""
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
