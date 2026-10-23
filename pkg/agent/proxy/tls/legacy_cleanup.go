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
	"strings"

	"go.keploy.io/server/v3/utils"
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
// recipe: the anchor files in the system store, the fixed JDK keystore alias
// (from PATH's JDK, its keystore found as an install finds it:
// resolveJavaStore), and the world-writable temp files the old client left
// behind. It is best-effort and idempotent — a host that was never touched, or
// already cleaned, is a no-op. With fresh=true the system trust bundle is
// rebuilt from scratch (update-ca-certificates --fresh on Debian) so
// hash-linked copies of a removed anchor cannot linger.
func SweepLegacyCA(ctx context.Context, logger *zap.Logger, fresh bool) {
	sweepLegacyCA(ctx, logger, fresh, false, javaStoresFor(ctx, logger, ""))
}

// SweepLegacyCAAggressive is the explicit `keploy ca clean` path. It does
// everything SweepLegacyCA does, in the JDK JAVA_HOME names as well as PATH's,
// and also removes the leaked per-uid Java truststores. That removal is not
// fingerprint-gated, so it is kept out of the automatic per-run boot sweep (two
// concurrent runs share the per-uid truststore name, and the boot sweep of one
// could delete the other's live store).
func SweepLegacyCAAggressive(ctx context.Context, logger *zap.Logger, fresh bool) {
	sweepLegacyCA(ctx, logger, fresh, true, javaStoresFor(ctx, logger, os.Getenv("JAVA_HOME"), ""))
}

// sweepLegacyCA is SweepLegacyCA with the JDK keystores to look in given: a
// native run's own (setupNativeForApp) and PATH's.
func sweepLegacyCA(ctx context.Context, logger *zap.Logger, fresh, includeTruststores bool, javaStores []javaStore) {
	fp, ok := legacyCAFingerprint()
	if !ok {
		logger.Debug("legacy CA fingerprint constant is malformed; skipping cleanup")
		return
	}
	sweepCAByFingerprint(ctx, logger, fp, fresh, includeTruststores, javaStores)
}

// sweepCAByFingerprint is sweepLegacyCA for a given fingerprint (the retired
// CA's in production; a test passes one it can make a certificate for).
func sweepCAByFingerprint(ctx context.Context, logger *zap.Logger, fp [32]byte, fresh, includeTruststores bool, javaStores []javaStore) {
	removed := removeLegacyAnchors(logger, fp)
	if removed > 0 {
		logger.Info("removed retired static MITM CA anchors from the system trust store",
			zap.Int("count", removed))
		if err := refreshCaStore(ctx, fresh); err != nil {
			logger.Warn("removed the retired static MITM CA's anchors, but could not rebuild the system trust bundle without them; the bundle trusts that CA until its next rebuild",
				zap.Error(err), zap.String("next_step", "rebuild the system trust bundle as root: update-ca-certificates --fresh (Debian, Ubuntu), update-ca-trust extract (Fedora, RHEL)"))
		}
	}

	for _, js := range javaStores {
		removeLegacyJavaAlias(ctx, logger, fp, js)
	}
	removeLegacyTempFiles(logger, fp, includeTruststores)
}

// removeLegacyAnchors deletes, from each system CA directory, any file that is
// exactly one certificate whose fingerprint is the retired CA's. Requiring a
// single-certificate file means a shared bundle (ca-certificates.crt,
// ca-bundle.crt) is never touched — only the standalone anchor keploy wrote.
func removeLegacyAnchors(logger *zap.Logger, fp [32]byte) int {
	removed := 0
	for _, dir := range caStorePath {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			p := filepath.Join(dir, e.Name())
			if !fileIsExactlyCert(p, fp) {
				continue
			}
			if err := os.Remove(p); err != nil {
				logger.Warn("could not remove the retired static MITM CA from the system trust store; it stays trusted there",
					zap.String("path", p), zap.Error(err), zap.String("next_step", "remove "+p+" as root, then rebuild the system trust bundle"))
				continue
			}
			logger.Debug("removed legacy CA anchor", zap.String("path", p))
			removed++
		}
	}
	return removed
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

// removeLegacyJavaAlias deletes the fixed "keployCA" alias from the JDK
// keystore js, but only after reading that entry back and confirming its
// certificate is the retired CA — so a user's own entry under the same alias is
// never clobbered.
func removeLegacyJavaAlias(ctx context.Context, logger *zap.Logger, fp [32]byte, js javaStore) {
	der, ok := exportJavaAliasCert(ctx, js.keytool, js.cacerts, legacyCAAlias)
	if !ok {
		return
	}
	if sha256.Sum256(der) != fp {
		logger.Debug("a 'keployCA' JDK alias exists but is not the retired CA; leaving it", zap.String("cacerts", js.cacerts))
		return
	}
	cmd := exec.CommandContext(ctx, js.keytool, "-delete", "-alias", legacyCAAlias, "-keystore", js.cacerts, "-storepass", javaStorePass, "-noprompt")
	out, err := cmd.CombinedOutput()
	utils.BootStep("keytool -delete is over")
	if err != nil {
		logger.Warn("could not remove the retired static MITM CA from the JDK keystore; that JDK trusts it until the alias is deleted",
			zap.String("alias", legacyCAAlias), zap.String("cacerts", js.cacerts), zap.String("keytool", js.keytool),
			zap.ByteString("output", bytes.TrimSpace(out)), zap.Error(err),
			zap.String("next_step", fmt.Sprintf(`"%s" -delete -alias %s -keystore "%s" -storepass %s`, js.keytool, legacyCAAlias, js.cacerts, javaStorePass)))
		return
	}
	logger.Info("removed the retired MITM CA from the JDK truststore", zap.String("cacerts", js.cacerts))
}

// exportJavaAliasCert returns the DER of alias's certificate from cacerts, or
// ok=false when the alias is absent or keytool fails.
func exportJavaAliasCert(ctx context.Context, keytool, cacerts, alias string) (der []byte, ok bool) {
	cmd := exec.CommandContext(ctx, keytool, "-exportcert", "-rfc", "-alias", alias, "-keystore", cacerts, "-storepass", javaStorePass)
	out, err := cmd.Output()
	utils.BootStep("keytool -exportcert is over")
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

// refreshCaStore rebuilds the system trust bundle after anchors were removed.
// On Debian/Ubuntu update-ca-certificates --fresh also prunes stale hash-links
// to a deleted anchor (a plain run leaves dangling /etc/ssl/certs symlinks that
// Go ignores but that are confusing); other distros' tools regenerate wholesale.
func refreshCaStore(ctx context.Context, fresh bool) error {
	if commandExists("update-ca-certificates") {
		args := []string{}
		if fresh {
			args = append(args, "--fresh")
		}
		return exec.CommandContext(ctx, "update-ca-certificates", args...).Run()
	}
	// update-ca-trust / the rest take no --fresh; a bare extract regenerates.
	return updateCaStore(ctx)
}
