package tls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
// (from the JDK JAVA_HOME names and from PATH's, each keystore found as an
// install finds it: resolveJavaStore), and the world-writable temp files the
// old client left behind. It is best-effort and idempotent — a host that was
// never touched, or already cleaned, is a no-op. With fresh=true the system
// trust bundle is rebuilt from scratch (update-ca-certificates --fresh on
// Debian) so hash-linked copies of a removed anchor cannot linger.
//
// It is the docker-mode start's sweep, in the agent's container; a native
// run's start sweeps the keystore it imports into (setupNativeForApp).
func SweepLegacyCA(ctx context.Context, logger *zap.Logger, fresh bool) {
	sweepLegacyCA(ctx, logger, fresh, false, javaStoresFor(ctx, logger, os.Getenv("JAVA_HOME"), ""))
}

// SweepLegacyCAAggressive is the explicit `keploy ca clean` path. It does
// everything SweepLegacyCA does, and also removes the leaked per-uid Java
// truststores. That removal is not fingerprint-gated, so it is kept out of the
// automatic per-run boot sweep (two concurrent runs share the per-uid
// truststore name, and the boot sweep of one could delete the other's live
// store).
func SweepLegacyCAAggressive(ctx context.Context, logger *zap.Logger, fresh bool) {
	sweepLegacyCA(ctx, logger, fresh, true, javaStoresFor(ctx, logger, os.Getenv("JAVA_HOME"), ""))
}

// sweepLegacyCA is SweepLegacyCA with the JDK keystores to look in given: a
// native run's own (setupNativeForApp); JAVA_HOME's alone on macOS; JAVA_HOME's
// and PATH's in docker mode and for `keploy ca clean`.
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
//
// The system bundle is rebuilt when this pass removed an anchor, and also when
// an earlier sweep's removal is still owed its rebuild (legacyRebuildMarker):
// a sweep or a `keploy ca clean` whose rebuild a stop, a failure or a crash
// cut short removed the anchor, and the next, finding no anchor, would
// otherwise never rebuild the bundle, which went on trusting a CA whose key is
// public. Both, the removal and the rebuild, are made under the trust-store
// lock (removeRetiredAnchorsAndRebuild), so agents starting together remove
// and rebuild once, and none clears a debt another has just taken on. A host
// with no anchor of the retired CA and nothing owed takes no lock.
//
// On Linux, the explicit `keploy ca clean` (includeTruststores) also rebuilds
// the bundle when nothing is owed: a bundle can hold the retired CA with no
// anchor left and no debt recorded (an older keploy's cleanup whose rebuild
// failed or was cut short left it so), and the automatic sweeps only say so
// (warnRetiredCAKeptInBundle), not rebuilding at every start.
func sweepCAByFingerprint(ctx context.Context, logger *zap.Logger, fp [32]byte, fresh, includeTruststores bool, javaStores []javaStore) {
	// Off Linux keploy rebuilds no system bundle, so `keploy ca clean` has
	// none to rebuild there.
	explicit := includeTruststores && runtime.GOOS == "linux"
	if explicit || legacyAnchorsPresent(fp) || legacyRebuildOwed() {
		gotLock := false
		err := withTrustStoreLock(ctx, logger, func(locked context.Context) error {
			gotLock = true
			return removeRetiredAnchorsAndRebuild(ctx, locked, logger, fp, fresh, explicit)
		})
		if err != nil {
			reportRetiredCACleanupFailure(ctx, logger, fp, err, gotLock, explicit)
		}
	} else if bundle := legacyBundleHolding(fp); bundle != "" {
		warnRetiredCAKeptInBundle(logger, bundle, false)
	}

	for _, js := range javaStores {
		removeLegacyJavaAlias(ctx, logger, fp, js)
	}
	removeLegacyTempFiles(logger, fp, includeTruststores)
}

// reportRetiredCACleanupFailure logs why the system-store half of the retired
// CA's cleanup failed (err), by what is left of it: anchors of the CA, a
// bundle that holds it, a rebuild owed. gotLock says the lock was taken (so
// each anchor that could not be removed has said so already); explicit, that
// `keploy ca clean` asked for it.
func reportRetiredCACleanupFailure(ctx context.Context, logger *zap.Logger, fp [32]byte, err error, gotLock, explicit bool) {
	anchors, bundle, owed := legacyAnchors(fp), legacyBundleHolding(fp), legacyRebuildOwed()
	left := len(anchors) > 0 || owed
	rebuild := "rebuild the system trust bundle as root: " + legacyRebuildCommands
	again := rebuild
	if explicit {
		again = "run sudo -E keploy ca clean again, or " + rebuild
	}
	switch {
	case ctx.Err() != nil && errors.Is(err, context.Cause(ctx)):
		msg := "the stop cut short the cleanup of the retired static MITM CA from the system trust store"
		if left {
			msg += "; the next keploy start finishes it"
		}
		logger.Debug(msg, zap.Error(err))
	case !gotLock && len(anchors) > 0:
		next := "remove " + strings.Join(anchors, " ") + " as root, then " + rebuild
		if explicit {
			next = "run sudo -E keploy ca clean again; if it cannot take the lock either, " + next
		}
		logger.Warn("could not take the trust-store lock to remove the retired static MITM CA from the system trust store; it stays trusted there",
			zap.Strings("anchors", anchors), zap.Error(err), zap.String("next_step", next))
	case bundle == "" && !owed:
		// No bundle keploy knows holds the CA, and no removal owes its
		// rebuild: nothing trusts it that a rebuild would change.
		logger.Info("the system trust bundle was not rebuilt, but no bundle keploy knows holds the retired static MITM CA, and no removal of it awaits a rebuild",
			zap.Strings("bundles", legacyCABundlePaths), zap.Error(err))
	default:
		what := "could not rebuild the system trust bundle without the retired static MITM CA"
		if !gotLock {
			what = "could not take the trust-store lock to rebuild the system trust bundle without the retired static MITM CA"
		}
		trusts := "; the system trust bundle may still trust that CA until it is rebuilt"
		if bundle != "" {
			trusts = "; " + bundle + " trusts that CA until it is rebuilt"
		}
		if owed {
			trusts += ", which the next keploy start tries again"
		}
		logger.Warn(what+trusts, zap.Error(err), zap.String("next_step", again))
	}
}

// removeRetiredAnchorsAndRebuild removes the retired CA's anchors
// (removeLegacyAnchors) and rebuilds the system bundle without it when this
// removed one or an earlier removal is still owed its rebuild, under the
// trust-store lock (locked); with explicit (`keploy ca clean`), always. A
// removal that failed, with nothing owed, owes nothing: no rebuild is made at
// every start for an anchor keploy cannot delete (its Warn says so). A stop
// (ctx) before the removal removes nothing (the anchors left make the next
// sweep do the work); one before the rebuild leaves it owed.
func removeRetiredAnchorsAndRebuild(ctx, locked context.Context, logger *zap.Logger, fp [32]byte, fresh, explicit bool) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("did not remove the retired static MITM CA's anchors: %w", context.Cause(ctx))
	}
	owed := legacyRebuildOwed()
	removed, failed := removeLegacyAnchors(logger, legacyAnchors(fp))
	if removed > 0 {
		logger.Info("removed retired static MITM CA anchors from the system trust store",
			zap.Int("count", removed))
	}
	if removed == 0 && !owed {
		clearLegacyRebuildOwed(logger) // taken on for a removal that did not happen
		if !explicit {
			return nil
		}
	}
	return rebuildWithoutRetiredCA(ctx, locked, logger, fp, fresh, failed > 0)
}

// legacyAnchorsPresent reports whether a system CA directory holds an anchor
// of the retired CA (fingerprint fp) for removeLegacyAnchors to remove.
func legacyAnchorsPresent(fp [32]byte) bool { return len(legacyAnchors(fp)) > 0 }

// legacyAnchors lists, in each system CA directory, every file that is exactly
// one certificate whose fingerprint is fp. Requiring a single-certificate file
// means a shared bundle (ca-certificates.crt, ca-bundle.crt) is never touched —
// only the standalone anchor keploy wrote.
func legacyAnchors(fp [32]byte) []string {
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

// removeLegacyAnchors deletes the retired CA's anchors (legacyAnchors), and
// returns how many are gone and how many it could not remove. One already
// gone (something that does not take keploy's lock removed it since it was
// listed) is gone: its rebuild is owed all the same. A rebuild is owed
// (oweLegacyRebuild) before the first is removed, so a sweep cut short after
// it still makes the rebuild at the next start; a debt that could not be
// recorded is a Warn once an anchor is gone.
func removeLegacyAnchors(logger *zap.Logger, anchors []string) (removed, failed int) {
	var recordErr error
	for i, p := range anchors {
		if i == 0 {
			recordErr = oweLegacyRebuild()
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("could not remove the retired static MITM CA from the system trust store; it stays trusted there",
				zap.String("path", p), zap.Error(err), zap.String("next_step", "remove "+p+" as root, then rebuild the system trust bundle"))
			failed++
			continue
		}
		logger.Debug("removed legacy CA anchor", zap.String("path", p))
		removed++
	}
	if removed > 0 && recordErr != nil {
		logger.Warn("could not record that the system trust bundle is owed a rebuild without the retired static MITM CA; if this start does not finish the rebuild, no later one makes it",
			zap.String("path", legacyRebuildMarker), zap.Error(recordErr),
			zap.String("next_step", "if this start's rebuild does not succeed, rebuild the system trust bundle as root: "+legacyRebuildCommands))
	}
	return removed, failed
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
// keystore js, but only once its certificate has been read back and found to
// be the retired CA — so a user's own entry under the same alias is never
// clobbered. The keystore itself is read first, needing no lock and no copy:
// almost none holds the retired CA. The delete is an edit like any other
// (editJavaKeystore), which reads the entry again from its copy.
func removeLegacyJavaAlias(ctx context.Context, logger *zap.Logger, fp [32]byte, js javaStore) {
	if held, _ := retiredCAHeld(ctx, ctx, logger, js.keytool, js.cacerts, fp); !held {
		return
	}
	_, changed, err := editJavaKeystore(ctx, logger, js, keystoreEdit{
		alias: legacyCAAlias,
		onlyIf: func(ctx, locked context.Context, keytool, copyPath string) (bool, error) {
			// The copy's list said it holds the alias: a failure to read the
			// entry is a failure, not an absence.
			return retiredCAHeld(ctx, locked, logger, keytool, copyPath, fp)
		},
	})
	switch {
	case err != nil && ctx.Err() != nil:
		logger.Debug("the stop cut short the removal of the retired static MITM CA from the JDK keystore; the next start removes it",
			zap.String("cacerts", js.cacerts), zap.Error(err))
	case err != nil:
		logger.Warn("could not remove the retired static MITM CA from the JDK keystore; that JDK trusts it until the alias is deleted",
			zap.String("alias", legacyCAAlias), zap.String("cacerts", js.cacerts), zap.String("keytool", js.keytool), zap.Error(err),
			zap.String("next_step", fmt.Sprintf(`"%s" -delete -alias %s -keystore "%s" -storepass %s`, js.keytool, legacyCAAlias, js.cacerts, javaStorePass)))
	case changed:
		logger.Info("removed the retired MITM CA from the JDK truststore", zap.String("cacerts", js.cacerts))
	default:
		logger.Debug("no retired MITM CA left to remove in keploy's copy of the JDK keystore: its keployCA entry was gone, or is not the retired CA", zap.String("cacerts", js.cacerts))
	}
}

// legacyCABundlePaths are the system trust bundles keploy's rebuild writes:
// update-ca-certificates' (Debian, Ubuntu, Alpine; openSUSE's) and
// update-ca-trust's (Fedora, RHEL, Amazon Linux; Arch's). They are read to say
// why the retired CA is still trusted, and, where no rebuild tool is on PATH,
// to tell whether a rebuild is still owed (a bundle that holds it was built by
// a tool). A bundle elsewhere is not seen. A variable so tests can point it at
// files of their own.
var legacyCABundlePaths = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/var/lib/ca-certificates/ca-bundle.pem",
	"/etc/ca-certificates/extracted/tls-ca-bundle.pem",
}

// legacyRebuildCommands are the rebuilds of the system trust bundle a Warn
// tells the user to run.
const legacyRebuildCommands = "update-ca-certificates --fresh (Debian, Ubuntu), update-ca-trust extract (Fedora, RHEL)"

// legacyRebuildMarker is the file that says a removal of the retired CA's
// anchors is still owed its rebuild of the system bundle: written before the
// first anchor is removed (removeLegacyAnchors), removed once a rebuild has
// succeeded. It outlives the process, and a reboot (nothing rebuilds the
// bundle at boot), so a removal whose rebuild never ran is rebuilt by the next
// sweep. A variable so tests can point it at a file of their own.
var legacyRebuildMarker = "/var/lib/keploy/retired-ca-rebuild-owed"

// legacyRebuildOwed reports whether a removal of the retired CA's anchors is
// still owed its rebuild.
func legacyRebuildOwed() bool {
	_, err := os.Stat(legacyRebuildMarker)
	return err == nil
}

// oweLegacyRebuild records that a rebuild is owed, before an anchor is
// removed (one owed already is recorded already). The sweep that removes it
// rebuilds whether or not the record could be written; without it, only a
// rebuild that this sweep does not finish (a stop, a failure, a crash) is not
// made at the next start.
func oweLegacyRebuild() error {
	if legacyRebuildOwed() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(legacyRebuildMarker), 0o755); err != nil {
		return err
	}
	return os.WriteFile(legacyRebuildMarker, []byte("the retired static MITM CA's anchors were removed; the system trust bundle is to be rebuilt\n"), 0o644)
}

// clearLegacyRebuildOwed records that no rebuild is owed. One that stays
// recorded (its record cannot be removed) makes every later start rebuild: a
// Warn says so. A record that cannot be removed because it cannot be there
// (its directory is not one, or is on a read-only mount where none was
// written) owes nothing.
func clearLegacyRebuildOwed(logger *zap.Logger) {
	if err := os.Remove(legacyRebuildMarker); err != nil && !errors.Is(err, fs.ErrNotExist) && legacyRebuildOwed() {
		logger.Warn("could not record that the system trust bundle owes no rebuild; every keploy start rebuilds it until this file is removed",
			zap.String("path", legacyRebuildMarker), zap.Error(err), zap.String("next_step", "remove "+legacyRebuildMarker+" as root"))
	}
}

// legacyBundleHolding returns the first of legacyCABundlePaths that holds a
// certificate whose fingerprint is fp ("" for none). Off Linux there is no
// such bundle for keploy to rebuild: "".
func legacyBundleHolding(fp [32]byte) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	for _, p := range legacyCABundlePaths {
		rest, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			if block.Type == "CERTIFICATE" && sha256.Sum256(block.Bytes) == fp {
				return p
			}
		}
	}
	return ""
}

// rebuildWithoutRetiredCA rebuilds the system trust bundle, under the lock
// (locked), so that it no longer holds the retired CA (fp), and then no longer
// owes the rebuild (legacyRebuildMarker). A stop (ctx) before the rebuild
// starts leaves it owed. With no rebuild tool on PATH, the rebuild stays owed
// only while a bundle holds the CA (legacyCABundlePaths): a tool built it,
// which another PATH may find. A bundle that holds the CA after the rebuild is a
// Warn that says why: the rebuild does not write it, or an anchor keploy does
// not manage keeps it (unless one keploy could not remove does, keptByFailed:
// that removal's Warn says so).
func rebuildWithoutRetiredCA(ctx, locked context.Context, logger *zap.Logger, fp [32]byte, fresh, keptByFailed bool) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("did not rebuild the system trust store: %w", context.Cause(ctx))
	}
	bundle := legacyBundleHolding(fp)
	var before os.FileInfo
	if bundle != "" {
		before, _ = os.Stat(bundle)
	}
	if err := refreshCaStore(locked, logger, fresh); err != nil {
		if errors.Is(err, errNoTrustStoreTool) && legacyBundleHolding(fp) == "" {
			// No rebuild tool on PATH, and no bundle that holds the CA for one
			// to rebuild: nothing is owed. (A bundle that holds it was built
			// by a tool this process cannot find, say on a PATH without
			// /usr/sbin: the rebuild stays owed.)
			clearLegacyRebuildOwed(logger)
		}
		return err
	}
	clearLegacyRebuildOwed(logger)
	still := legacyBundleHolding(fp)
	if still == "" || keptByFailed {
		return nil
	}
	if now, err := os.Stat(still); still == bundle && before != nil && err == nil && sameFileState(before, now) {
		logger.Warn("a system trust bundle holds the retired static MITM CA, and the trust store's rebuild does not write it",
			zap.String("bundle", still), zap.String("next_step", "remove the certificate CN=\"My Custom CA\" (sha256 "+legacyCASHA256Hex+") from "+still+" by hand"))
		return nil
	}
	warnRetiredCAKeptInBundle(logger, still, true)
	return nil
}

// warnRetiredCAKeptInBundle warns of a bundle the rebuild writes that holds
// the retired CA though keploy has no anchor of it left to remove. After a
// rebuild (rebuilt), an anchor of it that keploy does not manage (in a
// subdirectory of an anchor directory, or listed elsewhere for the rebuild)
// keeps it. Without one, the bundle may also not have been rebuilt since an
// earlier removal (an older keploy's, whose rebuild failed): `keploy ca clean`
// rebuilds it.
func warnRetiredCAKeptInBundle(logger *zap.Logger, bundle string, rebuilt bool) {
	find := "find the anchor holding the certificate CN=\"My Custom CA\" (sha256 " + legacyCASHA256Hex + ") among the trust store's sources (searching their subdirectories too), remove it, and rebuild the bundle"
	if rebuilt {
		logger.Warn("the system trust bundle holds the retired static MITM CA after its rebuild, and keploy has no anchor of it to remove: an anchor that keploy does not manage keeps it there",
			zap.String("bundle", bundle), zap.String("next_step", find))
		return
	}
	logger.Warn("the system trust bundle holds the retired static MITM CA, and keploy has no anchor of it to remove: the bundle was not rebuilt since an earlier removal, or an anchor that keploy does not manage keeps it there",
		zap.String("bundle", bundle), zap.String("next_step", "run sudo -E keploy ca clean, which rebuilds the bundle; if it still holds the CA after that, "+find))
}

// retiredCAHeld reports whether the keystore's legacyCAAlias entry is the
// retired CA (fingerprint fp). An error says the entry could not be read: the
// keystore holds no such alias, or keytool failed to read it. Under a locked
// context (a copy's check), keytool's progress is noted for the processes
// queued for the lock; pass ctx itself as locked otherwise.
func retiredCAHeld(ctx, locked context.Context, logger *zap.Logger, keytool, keystore string, fp [32]byte) (bool, error) {
	der, ok := exportJavaAliasCert(ctx, locked, keytool, keystore, legacyCAAlias)
	if !ok {
		return false, fmt.Errorf("keytool could not read the %s entry of %s", legacyCAAlias, keystore)
	}
	if sha256.Sum256(der) != fp {
		logger.Debug("a 'keployCA' JDK alias exists but is not the retired CA; leaving it", zap.String("cacerts", keystore))
		return false, nil
	}
	return true, nil
}

// exportJavaAliasCert returns the DER of alias's certificate from cacerts, or
// ok=false when the alias is absent or keytool fails. Its progress is noted
// under a locked context, as listJavaAliases's.
func exportJavaAliasCert(ctx, locked context.Context, keytool, cacerts, alias string) (der []byte, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, storeToolCeiling)
	defer cancel()
	out, err := combinedOutputNoting(lockedCommand(ctx, locked, keytool, "-exportcert", "-rfc", "-alias", alias, "-keystore", cacerts, "-storepass", javaStorePass))
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
// Like updateCaStore it runs under the trust-store lock, and to its end once
// the lock is held: its Debian hook rewrites the JDK keystore.
func refreshCaStore(ctx context.Context, logger *zap.Logger, fresh bool) error {
	if !commandExists("update-ca-certificates") {
		// update-ca-trust / the rest take no --fresh; a bare extract regenerates.
		return updateCaStore(ctx, logger)
	}
	return withTrustStoreLock(ctx, logger, func(locked context.Context) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("did not rebuild the system trust store: %w", context.Cause(ctx))
		}
		args := []string{}
		if fresh {
			args = append(args, "--fresh")
		}
		out, err := runStoreCommandWithin(storeCommand(locked, "update-ca-certificates", args...), storeToolCeiling)
		if err != nil {
			return fmt.Errorf("update-ca-certificates failed: %w: %s", err, bytes.TrimSpace(out))
		}
		return nil
	})
}
