package tls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Every edit keploy makes to a JDK keystore goes through editJavaKeystore: a
// native run's import of its CA (importJavaCA), its teardown's delete
// (removeJavaAlias), and the retired CA's removal (removeLegacyJavaAlias).
// keploy's rebuilds of the system trust store, whose Debian hook rewrites the
// JDK keystore, run under the same lock (updateCaStore, refreshCaStore).
//
// keytool never writes the keystore itself. keytool rewrites a keystore in
// place: it reads it whole, makes its one change, truncates the file and
// writes it again. So a keytool killed between the truncation and the end of
// the write (keploy ran it under the run's context, which kills it when the
// run is stopped, and the teardown's under a 30 s deadline) left the host
// JDK's truststore cut short, and every JVM on the host then failed TLS. And
// two keytools at once each write back their own reading of the keystore, so
// two keploy agents editing one keystore together lost one's edit: on a copy
// of a CI host's keystore, 20 trials of one agent's teardown beside another's
// start lost the import 4 times and the delete 13 times.
//
// So, under the machine-wide trust-store lock (lockTrustStore):
//
//  1. the keystore's real file (Debian's /usr/lib/jvm/<jdk>/lib/security/
//     cacerts is a link to /etc/ssl/certs/java/cacerts) is copied to a file of
//     keploy's own in the same directory;
//  2. keytool -list reads the copy, which tells whether there is anything to
//     do;
//  3. keytool makes the edit on the copy;
//  4. keytool -list reads the copy again, and it must hold exactly what the
//     keystore held with this edit's alias added or gone;
//  5. the copy is given the original's owner, mode and extended attributes
//     (its SELinux label), and renamed over the real file. The link, if any,
//     is left as it is.
//
// A rename is atomic: a JVM that reads the keystore meanwhile reads the old
// one or the new one, never part of either. An edit cut short anywhere before
// the rename (its keytool killed, a check that fails, the agent killed) leaves
// the keystore as it was: a deadline abandons only the copy, which is removed,
// and a copy a killed agent left behind is removed by the next edit, under the
// lock. A keystore that something other than keploy changed while keploy
// edited its copy (Debian's update-ca-certificates hook, an operator's
// keytool) is not replaced: the edit starts again from the keystore as it now
// is.

// keystoreEditAttempts is how many times an edit starts over because the
// keystore changed under it, before it gives up.
const keystoreEditAttempts = 3

// storeToolCeiling bounds one run of a tool keploy runs on the trust state (a
// keytool run, a trust-store rebuild), far above what one takes (a JVM's start
// and a keystore's read, a rebuild with its hooks: seconds, on a loaded host).
// It is not a budget for the work but the end of a tool that hangs: a hung JVM
// keeps its housekeeping threads running on their own timers, so to the probe
// of its progress (utils.ProcessProgress), to the CLI's, and to the processes
// queued for the lock it looks busy for ever. Past it a keytool that reads, or
// edits keploy's copy of a keystore, is killed; a rebuild, which writes the
// live store, is left running and waited for no longer (runStoreCommandWithin):
// holding the lock it inherited, it keeps the processes queued behind it
// waiting until it ends or they give up on it (lockTrustStore).
var storeToolCeiling = 5 * time.Minute // a variable for tests

// errKeystoreChanged is why an edit started over: the keystore changed while
// keploy edited its copy.
var errKeystoreChanged = errors.New("the keystore changed while keploy edited a copy of it")

// keystoreCopyPrefix is the start of the name of keploy's copy of the
// keystore whose real file is named base, in the same directory.
func keystoreCopyPrefix(base string) string { return "." + base + ".keploy-edit-" }

// keystoreEdit is one edit of a JDK keystore: the import of the certificate
// at certFile under alias, or, with certFile "", the deletion of alias.
type keystoreEdit struct {
	alias    string
	certFile string
	// onlyIf, when set, is asked of the keystore's copy, before the edit,
	// whether to make the edit at all (the retired CA's removal checks that
	// the entry is the retired CA's); an error fails the edit.
	onlyIf func(ctx, locked context.Context, keytool, copyPath string) (bool, error)
}

func (e keystoreEdit) importing() bool { return e.certFile != "" }

// editJavaKeystore makes e on js's keystore, as described above. changed says
// the keystore was replaced by the edited copy; held says whether it holds
// e.alias now, as far as the edit saw (aliasUnknown when it never got to read
// it). With an error the keystore is as it was before the call.
//
// ctx bounds the wait for the lock (which also ends behind a holder that makes
// no progress: lockTrustStore) and the keytool runs (each also bounded by
// storeToolCeiling): an end of either abandons the edit, of which only the
// copy is lost; nothing cuts the keystore short. A caller that holds the lock
// already (holdsTrustStoreLock) does not wait for it again.
func editJavaKeystore(ctx context.Context, logger *zap.Logger, js javaStore, e keystoreEdit) (held aliasHeld, changed bool, err error) {
	held = aliasUnknown
	err = withTrustStoreLock(ctx, logger, func(locked context.Context) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("did not edit %s: %w", js.cacerts, context.Cause(ctx))
		}
		real, err := filepath.EvalSymlinks(js.cacerts)
		if err != nil {
			return fmt.Errorf("could not find the file the keystore %s is: %w", js.cacerts, err)
		}
		removeAbandonedKeystoreCopies(logger, real)
		for attempt := 1; ; attempt++ {
			held, changed, err = editKeystoreCopy(ctx, locked, js.keytool, real, e)
			if !errors.Is(err, errKeystoreChanged) || attempt == keystoreEditAttempts {
				return err
			}
			logger.Debug("the JDK keystore changed while keploy edited a copy of it; editing it again",
				zap.String("cacerts", real), zap.Int("attempt", attempt))
		}
	})
	return held, changed, err
}

// editKeystoreCopy is one attempt of editJavaKeystore on the keystore's real
// file real, under the lock locked holds. Each keytool run's end is a step
// the processes queued for the lock see (noteTrustStoreProgress). An attempt
// that fails for any reason after a keystore that changed while it was read
// (a copy cut short by an in-place writer, say) fails with errKeystoreChanged,
// so that the edit starts over.
func editKeystoreCopy(ctx, locked context.Context, keytool, real string, e keystoreEdit) (held aliasHeld, changed bool, err error) {
	orig, data, err := readKeystore(real)
	if err != nil {
		return aliasUnknown, false, err
	}
	defer func() {
		if err == nil || errors.Is(err, errKeystoreChanged) {
			return
		}
		if now, serr := os.Lstat(real); serr != nil || !sameFileState(orig, now) {
			held, err = aliasUnknown, fmt.Errorf("%w (%v)", errKeystoreChanged, err)
		}
	}()
	copyPath, err := writeKeystoreCopy(real, data)
	if err != nil {
		return aliasUnknown, false, err
	}
	defer func() {
		if copyPath != "" {
			_ = os.Remove(copyPath)
		}
	}()
	step := func() { noteTrustStoreProgress(locked) }

	alias := strings.ToLower(e.alias)
	before, err := listJavaAliases(ctx, locked, keytool, copyPath)
	step()
	if err != nil {
		return aliasUnknown, false, fmt.Errorf("keytool could not read a copy of %s: %w", real, err)
	}
	held = aliasAbsent
	if before[alias] {
		held = aliasPresent
	}
	if before[alias] == e.importing() {
		return held, false, nil // already as the edit would leave it
	}
	if e.onlyIf != nil {
		ok, err := e.onlyIf(ctx, locked, keytool, copyPath)
		step()
		if err != nil {
			return held, false, err
		}
		if !ok {
			return held, false, nil
		}
	}

	args := []string{"-delete", "-alias", e.alias, "-keystore", copyPath, "-storepass", javaStorePass, "-noprompt"}
	if e.importing() {
		args = []string{"-import", "-trustcacerts", "-keystore", copyPath, "-storepass", javaStorePass, "-noprompt", "-alias", e.alias, "-file", e.certFile}
	}
	// Given the lock: a keytool that outlives this process (killed mid-edit)
	// keeps it until it exits, so the next edit, which removes the copy a dead
	// edit left, never runs beside it.
	run, cancel := context.WithTimeout(ctx, storeToolCeiling)
	out, err := combinedOutputNoting(lockedCommand(run, locked, keytool, args...))
	cancel()
	utils.BootStep("keytool " + args[0] + " is over")
	step()
	if err != nil {
		return held, false, fmt.Errorf("keytool %s on a copy of %s: %w: %s", args[0], real, err, bytes.TrimSpace(out))
	}

	after, err := listJavaAliases(ctx, locked, keytool, copyPath)
	step()
	if err != nil {
		return held, false, fmt.Errorf("keytool could not read the edited copy of %s: %w", real, err)
	}
	want := map[string]bool{}
	for a := range before {
		want[a] = true
	}
	if e.importing() {
		want[alias] = true
	} else {
		delete(want, alias)
	}
	if missing, extra := aliasDiff(want, after); len(missing)+len(extra) != 0 {
		did := "removed"
		if e.importing() {
			did = "added"
		}
		return held, false, fmt.Errorf("the edited copy of %s is not the keystore with only %s %s: it lacks %v and has %v besides; left the keystore as it was",
			real, e.alias, did, missing, extra)
	}
	// To disk before its mode is the original's, which may forbid writing it.
	if err := syncFile(copyPath); err != nil {
		return held, false, fmt.Errorf("could not write the edited copy of %s to disk: %w", real, err)
	}
	if err := keepKeystoreAttributes(copyPath, real, orig); err != nil {
		return held, false, err
	}
	if now, err := os.Lstat(real); err != nil || !sameFileState(orig, now) {
		return aliasUnknown, false, errKeystoreChanged
	}
	if err := os.Rename(copyPath, real); err != nil {
		return held, false, fmt.Errorf("could not replace %s with its edited copy: %w", real, err)
	}
	copyPath = ""
	// The rename itself, to disk: until then a crash could bring the old
	// keystore back, which is no worse than the edit not having happened.
	_ = syncDir(filepath.Dir(real))
	if e.importing() {
		return aliasPresent, true, nil
	}
	return aliasAbsent, true, nil
}

// readKeystore reads the keystore's real file whole, with what it was when
// read.
func readKeystore(real string) (os.FileInfo, []byte, error) {
	f, err := os.Open(real)
	if err != nil {
		return nil, nil, fmt.Errorf("could not open the keystore %s: %w", real, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("could not stat the keystore %s: %w", real, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("the keystore %s is not a regular file", real)
	}
	if n := hardLinks(fi); n > 1 {
		// Renaming a copy over one of its names would leave the others
		// with the keystore as it was: JVMs that read it by another name
		// would not see the edit.
		return nil, nil, fmt.Errorf("the keystore %s has %d hard links; keploy edits only a keystore it can replace whole", real, n)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, fmt.Errorf("could not read the keystore %s: %w", real, err)
	}
	return fi, data, nil
}

// writeKeystoreCopy writes data to a new file of keploy's own beside the
// keystore's real file, and returns its path.
func writeKeystoreCopy(real string, data []byte) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(real), keystoreCopyPrefix(filepath.Base(real))+"*")
	if err != nil {
		return "", fmt.Errorf("could not make a copy of the keystore %s beside it: %w", real, err)
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("could not write a copy of the keystore %s beside it: %w", real, err)
	}
	return f.Name(), nil
}

// removeAbandonedKeystoreCopies removes the copies of the keystore whose real
// file is real that edits cut short by their agent's death left beside it.
// Under a lock every keploy process takes, no other edit of it is under way,
// so every such copy is abandoned; where the lock keeps nothing out
// (trustStoreLockIsMachineWide) one could be another edit's, and none is
// removed.
func removeAbandonedKeystoreCopies(logger *zap.Logger, real string) {
	if !trustStoreLockIsMachineWide {
		return
	}
	dir, prefix := filepath.Dir(real), keystoreCopyPrefix(filepath.Base(real))
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, ent := range ents {
		if !strings.HasPrefix(ent.Name(), prefix) || !ent.Type().IsRegular() {
			continue
		}
		p := filepath.Join(dir, ent.Name())
		if err := os.Remove(p); err == nil {
			logger.Debug("removed the copy of a JDK keystore an edit cut short left behind", zap.String("path", p))
		}
	}
}

// sameFileState reports whether now is the file orig was, unchanged: the same
// file, of the same size, mode and modification time. An edit in place
// changes the modification time; a replacement, the file.
func sameFileState(orig, now os.FileInfo) bool {
	return os.SameFile(orig, now) && orig.Size() == now.Size() && orig.Mode() == now.Mode() && orig.ModTime().Equal(now.ModTime())
}

// syncFile writes path's data to disk.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// listJavaAliases returns the aliases keystore holds, lowercased, as keytool
// -list reads them: an error when keytool cannot read the keystore (a file
// cut short, a password that is not changeit), which is how a keystore keploy
// is about to write is checked. keytool runs in English so that its "Alias
// name:" lines read the same on every host. Under a locked context (one
// lockTrustStore returned), the run's progress is noted for the processes
// queued for the lock; pass ctx itself as locked otherwise.
func listJavaAliases(ctx, locked context.Context, keytool, keystore string) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, storeToolCeiling)
	defer cancel()
	cmd := lockedCommand(ctx, locked, keytool, "-J-Duser.language=en", "-J-Duser.country=US",
		"-list", "-rfc", "-keystore", keystore, "-storepass", javaStorePass)
	out, err := combinedOutputNoting(cmd)
	utils.BootStep("keytool -list is over")
	if err != nil {
		return nil, fmt.Errorf("keytool -list %s: %w: %s", keystore, err, bytes.TrimSpace(out))
	}
	aliases := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(l), "Alias name: "); ok {
			aliases[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	return aliases, nil
}

// aliasDiff returns the aliases want holds and got does not, and those got
// holds and want does not, sorted, at most ten of each (a keystore can hold
// over a thousand entries).
func aliasDiff(want, got map[string]bool) (missing, extra []string) {
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 10 {
		missing = append(missing[:10], fmt.Sprintf("and %d more", len(missing)-10))
	}
	if len(extra) > 10 {
		extra = append(extra[:10], fmt.Sprintf("and %d more", len(extra)-10))
	}
	return missing, extra
}
