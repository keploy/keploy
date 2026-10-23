package tls

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// A native run imports its CA into a JDK's keystore (setupNativeForApp) and
// deletes it again at the run's end (TeardownNativeCA). The two must edit the
// same keystore, and they used to find it by different rules: the install took
// the JDK's java.home (the app's JDK, or PATH java's as that JVM reports it),
// the teardown JAVA_HOME or the directory above PATH's keytool, its link not
// followed. On Debian and Ubuntu PATH's keytool is /usr/bin/keytool, a link
// through /etc/alternatives into /usr/lib/jvm/<jdk>/bin, so the teardown looked
// for /usr/lib/security/cacerts, found nothing, said nothing, and the host's
// JDK went on trusting every run's CA: one more trusted root per run. Where the
// install had used the app's JDK, or the agent had a JAVA_HOME of another, it
// deleted from the wrong keystore.
//
// So the keystore is decided in one place (resolveJavaStore), the install
// records the one it used (nativeInstall.java), and the teardown deletes from
// exactly that one. The sweep of the retired static CA finds its keystores
// with the same resolver. Every edit of a keystore, whoever makes it, is made
// on a copy under the host-wide trust-store lock (editJavaKeystore).

// javaStorePass is the password of every JDK's cacerts as shipped.
const javaStorePass = "changeit"

// javaStore is one JDK keystore keploy edits: the cacerts file, and the
// keytool it edits it with.
type javaStore struct {
	keytool string
	cacerts string
}

// resolveJavaStore decides the JDK keystore of javaHome. Every edit keploy
// makes to a JDK's trust resolves its keystore here, so no two of them can
// find different ones.
//
// javaHome "" is the JDK PATH's java runs: its java.home as that JVM itself
// reports it (util.GetJavaHome), the JDK's real home whatever links lead to the
// java on PATH. ok is false, with a nil error, where there is no JDK (no java
// on PATH).
//
// The keystore is the cacerts that JDK's JVMs read, $javaHome/lib/security/
// cacerts, or $javaHome/jre/lib/security/cacerts for a JDK 8 root (whose
// java.home is its jre); keytool and the JVM alike follow a link there, as
// Debian's to /etc/ssl/certs/java/cacerts. A home with neither is an error: a
// keytool -import would create a keystore no JVM reads. The keytool is that
// JDK's own $javaHome/bin/keytool; a home without one (a JRE-only layout) has
// PATH's keytool, by the path it is found at now, so a later edit of the same
// keystore runs the same keytool.
func resolveJavaStore(ctx context.Context, logger *zap.Logger, javaHome string) (js javaStore, ok bool, err error) {
	if javaHome == "" {
		if !util.IsJavaInstalled() {
			logger.Debug("Java is not installed on the system")
			return javaStore{}, false, nil
		}
		logger.Debug("checking java path from default java home")
		if javaHome, err = util.GetJavaHome(ctx); err != nil {
			return javaStore{}, false, fmt.Errorf("java is on PATH, but its java.home could not be read: %w", err)
		}
	}
	for _, rel := range [][]string{{"lib", "security", "cacerts"}, {"jre", "lib", "security", "cacerts"}} {
		p := filepath.Join(append([]string{javaHome}, rel...)...)
		if fi, statErr := os.Stat(p); statErr == nil && fi.Mode().IsRegular() {
			js.cacerts = p
			break
		}
	}
	if js.cacerts == "" {
		return javaStore{}, false, fmt.Errorf("the JDK at %s has no lib/security/cacerts (nor jre/lib/security/cacerts)", javaHome)
	}
	exe := "keytool"
	if runtime.GOOS == "windows" {
		exe = "keytool.exe"
	}
	js.keytool = filepath.Join(javaHome, "bin", exe)
	if _, statErr := os.Stat(js.keytool); statErr != nil {
		onPath, lookErr := exec.LookPath(exe)
		if lookErr != nil {
			return javaStore{}, false, fmt.Errorf("the JDK at %s has no bin/%s, and PATH has none: %w", javaHome, exe, lookErr)
		}
		logger.Debug("resolved keytool binary not found in javaHome; falling back to PATH keytool",
			zap.String("expected", js.keytool), zap.String("keytool", onPath), zap.Error(statErr))
		js.keytool = onPath
	}
	return js, true, nil
}

// javaStoresFor resolves the keystore of each of homes ("" for PATH's JDK),
// leaving out a home with no JDK, one that cannot be resolved, and a keystore
// already listed (dedupJavaStores).
func javaStoresFor(ctx context.Context, logger *zap.Logger, homes ...string) []javaStore {
	var stores []javaStore
	seen := map[string]bool{}
	for _, h := range homes {
		if seen[h] {
			continue
		}
		seen[h] = true
		js, ok, err := resolveJavaStore(ctx, logger, h)
		if err != nil {
			logger.Debug("could not find a JDK keystore", zap.String("java_home", h), zap.Error(err))
			continue
		}
		if ok {
			stores = append(stores, js)
		}
	}
	return dedupJavaStores(stores)
}

// dedupJavaStores keeps the first of the keystores that are one file (by its
// real path: Debian's JDKs all link theirs to /etc/ssl/certs/java/cacerts), so
// no keystore is edited twice.
func dedupJavaStores(stores []javaStore) []javaStore {
	var out []javaStore
	seen := map[string]bool{}
	for _, js := range stores {
		file := js.cacerts
		if real, err := filepath.EvalSymlinks(file); err == nil {
			file = real
		}
		if seen[file] {
			continue
		}
		seen[file] = true
		out = append(out, js)
	}
	return out
}

// importJavaCA imports the certificate at caPath into js under alias, unless
// js already holds alias. nil says js holds alias now. An error says js is as
// it was: the import is made on a copy of the keystore, which replaces it only
// once the import is complete and checked (editJavaKeystore).
func importJavaCA(ctx context.Context, logger *zap.Logger, js javaStore, caPath, alias string) error {
	logger.Debug("importing the keploy CA into the JDK keystore",
		zap.String("keytool", js.keytool), zap.String("caCertsPath", js.cacerts), zap.String("caPath", caPath), zap.String("alias", alias))
	_, changed, err := editJavaKeystore(ctx, logger, js, keystoreEdit{alias: alias, certFile: caPath})
	if err != nil {
		return fmt.Errorf("did not import the keploy CA into %s: %w", js.cacerts, err)
	}
	if !changed {
		logger.Debug("Java detected and CA already exists", zap.String("path", js.cacerts))
		return nil
	}
	logger.Debug("Java detected and successfully imported CA", zap.String("path", js.cacerts))
	return nil
}

// removeJavaAlias deletes this run's alias from js, the keystore its install
// imported it into. A failure is a Warn naming the alias and the keystore: that
// JDK goes on trusting the CA of a run that is over. A keystore that does not
// hold the alias (its import failed, or someone removed it already) is not one.
func removeJavaAlias(ctx context.Context, logger *zap.Logger, js javaStore, alias string) {
	held, changed, err := editJavaKeystore(ctx, logger, js, keystoreEdit{alias: alias})
	switch {
	case err == nil && changed:
		logger.Debug("removed this run's CA from the JDK keystore", zap.String("alias", alias), zap.String("cacerts", js.cacerts))
		return
	case err == nil:
		logger.Debug("this run's CA is not in the JDK keystore; nothing to remove", zap.String("alias", alias), zap.String("cacerts", js.cacerts))
		return
	}
	if held == aliasUnknown {
		// The edit never read the keystore (the lock, or the copy, failed):
		// read it as it is. A keystore is only ever replaced whole, so a read
		// needs no lock.
		held = javaAliasHeld(ctx, js, alias)
	}
	msg := "could not remove this run's CA from the JDK keystore; that JDK trusts it until the alias is deleted"
	switch held {
	case aliasAbsent:
		logger.Debug("this run's CA is not in the JDK keystore; nothing to remove", zap.String("alias", alias), zap.String("cacerts", js.cacerts), zap.NamedError("edit_error", err))
		return
	case aliasUnknown:
		msg = "could not remove this run's CA from the JDK keystore, nor read the keystore to see whether it holds it; if it does, that JDK trusts it until the alias is deleted"
	}
	logger.Warn(msg,
		zap.String("alias", alias), zap.String("cacerts", js.cacerts), zap.String("keytool", js.keytool), zap.Error(err),
		zap.String("next_step", fmt.Sprintf(`"%s" -delete -alias %s -keystore "%s" -storepass %s`, js.keytool, alias, js.cacerts, javaStorePass)))
}

// aliasHeld is what keytool says of an alias in a keystore.
type aliasHeld int

const (
	aliasPresent aliasHeld = iota // the keystore holds it
	aliasAbsent                   // keytool reads the keystore, and it does not
	aliasUnknown                  // keytool cannot read the keystore
)

// javaAliasHeld lists js with keytool (listJavaAliases) and looks for alias
// among its entries: one keytool run, whether the keystore holds it or not (as
// on a host whose trust-store update rebuilds the JDK keystore from the system
// store, Fedora's or Temurin's, and so dropped keploy's import already).
func javaAliasHeld(ctx context.Context, js javaStore, alias string) aliasHeld {
	aliases, err := listJavaAliases(ctx, ctx, js.keytool, js.cacerts)
	if err != nil {
		return aliasUnknown
	}
	if aliases[strings.ToLower(alias)] {
		return aliasPresent
	}
	return aliasAbsent
}
