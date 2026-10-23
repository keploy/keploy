package tls

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// keystoreForEditTest lays out a JDK whose keytool is fakeKeytool (failing and
// holding as fail says), its cacerts a link to a shared keystore as on Debian,
// holding entries, and returns the JDK and its javaStore. The trust-store lock
// is a file of the test's own (trustStoreLockForTest).
func keystoreForEditTest(t *testing.T, fail keytoolFails, entries map[string][]byte) (fakeJDK, javaStore) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the fake JDK is /bin/sh scripts, and the trust-store lock is Linux's")
	}
	trustStoreLockForTest(t)
	root := t.TempDir()
	j := makeFakeJDKAt(t, filepath.Join(root, "usr", "lib", "jvm", "java-21-openjdk-amd64"),
		filepath.Join(root, "etc", "ssl", "certs", "java", "cacerts"), fail)
	writeFakeKeystore(t, j.store, entries)
	return j, javaStore{keytool: filepath.Join(j.home, "bin", "keytool"), cacerts: filepath.Join(j.home, "lib", "security", "cacerts")}
}

// certFileForTest writes certPEM to a file of the test's own and returns its
// path, for an import's -file.
func certFileForTest(t *testing.T, certPEM []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(p, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// filesBeside lists the names in the directory of the keystore's real file.
func filesBeside(t *testing.T, store string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Dir(store))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// A keytool killed in the middle of writing a JDK's keystore must not leave
// that keystore cut short. keytool writes a keystore in place: it truncates
// the file and writes it again. keploy runs it under a context that kills it
// (SIGKILL) when the run is stopped during its start or the teardown's
// deadline passes, so a kill between the two left the host JDK's truststore
// truncated, and every JVM on the host then failed to verify TLS. Here the
// kill comes exactly there: the fake keytool has truncated the keystore it was
// given and waits; the run's context is then cancelled.
func TestAKeytoolKilledMidWriteLeavesTheKeystoreAsItWas(t *testing.T) {
	const alias = "keploy-mitm-00000000000000aa"
	hold := newKeytoolHold(t, "-import", alias, true)
	j, js := keystoreForEditTest(t, keytoolFails{hold: hold}, map[string][]byte{
		"debian:one.pem": testCACertPEM(), "debian:two.pem": testCACertPEM(),
	})
	before, err := os.ReadFile(j.store)
	if err != nil {
		t.Fatal(err)
	}
	caPath := certFileForTest(t, testCACertPEM())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- importJavaCA(ctx, zap.NewNop(), js, caPath, alias) }()
	hold.waitReached(t, done)
	cancel() // the stop: exec.CommandContext kills keytool half-way through its write
	if err := <-done; err == nil {
		t.Fatal("an import whose keytool was killed half-way reported success")
	}

	after, err := os.ReadFile(j.store)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("the JDK keystore is not what it was before the killed import:\n--- before ---\n%s--- after ---\n%s", before, after)
	}
	if names := filesBeside(t, j.store); len(names) != 1 || names[0] != "cacerts" {
		t.Fatalf("the killed import left files beside the keystore: %v", names)
	}
}

// Two keploy agents on one host editing one JDK keystore at the same time
// lose neither's edit. keytool reads the whole keystore, makes its change and
// writes the whole keystore back: run beside another keytool, one of them
// writes back a keystore without the other's change. Measured with the real
// keytool on a copy of a CI host's keystore, 20 trials of one agent's teardown
// (-delete of its alias) beside another's start (-import of its own) lost the
// import 4 times and the delete 13 times. Here agent A's import has read the
// keystore and is about to write it when agent B's teardown deletes B's
// alias: B must wait for A (or else it ran beside it), and both edits must be
// in the keystore at the end.
func TestTwoAgentsEditingOneKeystoreLoseNeitherEdit(t *testing.T) {
	const live, dead = "keploy-mitm-00000000000000aa", "keploy-mitm-00000000000000bb"
	hold := newKeytoolHold(t, "-import", live, false)
	j, js := keystoreForEditTest(t, keytoolFails{hold: hold}, map[string][]byte{
		dead: testCACertPEM(), "debian:one.pem": testCACertPEM(),
	})
	caPath := certFileForTest(t, testCACertPEM())

	aDone := make(chan error, 1)
	go func() { aDone <- importJavaCA(context.Background(), zap.NewNop(), js, caPath, live) }()
	hold.waitReached(t, aDone)

	// B, logging to a core that says when B has to queue for the lock.
	waiting := make(chan struct{})
	var once sync.Once
	core, _ := observer.New(zapcore.DebugLevel)
	logB := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if e.Message == msgTrustStoreBusy {
			once.Do(func() { close(waiting) })
		}
		return nil
	}))
	bDone := make(chan struct{})
	go func() {
		removeJavaAlias(context.Background(), logB, js, dead)
		close(bDone)
	}()
	ranBeside := false
	select {
	case <-bDone:
		ranBeside = true // B did not wait for A's write
	case <-waiting:
	}
	hold.letGo(t)
	if err := <-aDone; err != nil {
		t.Fatalf("agent A's import: %v", err)
	}
	<-bDone

	got := aliasesIn(t, j.store)
	if !holds(t, j.store, live) || holds(t, j.store, dead) || !holds(t, j.store, "debian:one.pem") {
		t.Fatalf("after A's import of %s and B's delete of %s the keystore holds %v (B ran beside A: %v); want %s and debian:one.pem, without %s",
			live, dead, got, ranBeside, live, dead)
	}
	if ranBeside {
		t.Fatalf("B's delete ran while A's import was half-way, with nothing to keep them apart (the keystore holds %v)", got)
	}
}

// An edit leaves the keystore's real file as its readers knew it but for the
// edit: the same mode and owner, the same extended attributes, and a link to
// it (Debian's cacerts) still the link it was. A file keploy creates is 0600
// and labelled by its directory; renamed in as it is, no JVM run as another
// user could read the host's truststore.
func TestAKeystoreEditKeepsTheKeystoresModeOwnerAttributesAndLink(t *testing.T) {
	j, js := keystoreForEditTest(t, keytoolFails{}, map[string][]byte{"debian:one.pem": testCACertPEM()})
	if err := os.Chmod(j.store, 0o644); err != nil {
		t.Fatal(err)
	}
	xattrSet := setTestXattr(t, j.store, "user.keploy-test", "kept")
	label := relabelForTest(t, j.store, "cert_t")
	link := js.cacerts
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(j.store)
	if err != nil {
		t.Fatal(err)
	}

	if err := importJavaCA(context.Background(), zap.NewNop(), js, certFileForTest(t, testCACertPEM()), "keploy-mitm-00000000000000aa"); err != nil {
		t.Fatalf("importJavaCA: %v", err)
	}
	if !holds(t, j.store, "keploy-mitm-00000000000000aa") || !holds(t, j.store, "debian:one.pem") {
		t.Fatalf("the import did not leave the keystore with both entries: %v", aliasesIn(t, j.store))
	}
	after, err := os.Stat(j.store)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode() != before.Mode() {
		t.Errorf("the edited keystore's mode is %v, was %v", after.Mode(), before.Mode())
	}
	if !sameOwner(before, after) {
		t.Errorf("the edited keystore's owner changed")
	}
	if os.SameFile(before, after) {
		t.Errorf("the keystore was written in place, not replaced by its edited copy")
	}
	if got, err := os.Readlink(link); err != nil || got != target {
		t.Errorf("the link %s to the keystore is now %q (%v), was %q", link, got, err, target)
	}
	if xattrSet {
		if got := testXattr(t, j.store, "user.keploy-test"); got != "kept" {
			t.Errorf("the edited keystore's user.keploy-test is %q, was \"kept\"", got)
		}
	}
	if label != "" {
		if got := testXattr(t, j.store, "security.selinux"); got != label {
			t.Errorf("the edited keystore is labelled %q, was %q: a confined service reading the JDK's truststore by its label could not any more", got, label)
		}
	}
	if names := filesBeside(t, j.store); len(names) != 1 {
		t.Errorf("the edit left files beside the keystore: %v", names)
	}
}

// A copy an edit left beside the keystore when its agent was killed is
// removed by the next edit, which holds the lock every keploy edit takes, so
// no other edit is under way; a file of any other name is left alone.
func TestAnEditRemovesTheCopyADeadEditLeftBehind(t *testing.T) {
	j, js := keystoreForEditTest(t, keytoolFails{}, nil)
	dir := filepath.Dir(j.store)
	dead := filepath.Join(dir, keystoreCopyPrefix(filepath.Base(j.store))+"123456")
	other := filepath.Join(dir, "cacerts.dpkg-old")
	for _, p := range []string{dead, other} {
		if err := os.WriteFile(p, []byte("half a keystore"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := importJavaCA(context.Background(), zap.NewNop(), js, certFileForTest(t, testCACertPEM()), "keploy-mitm-00000000000000aa"); err != nil {
		t.Fatalf("importJavaCA: %v", err)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("the copy a dead edit left, %s, is still there (stat: %v)", dead, err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("the edit removed %s, which is not keploy's: %v", other, err)
	}
}

// An edit asked for after the stop does nothing: the lock, free, is still
// taken (its context bounds only the wait), but no keytool runs, no copy is
// made and nothing beside the keystore is touched, not even a dead edit's
// copy, which the next edit removes.
func TestAnEditAskedAfterTheStopTouchesNothing(t *testing.T) {
	j, js := keystoreForEditTest(t, keytoolFails{}, map[string][]byte{"debian:one.pem": testCACertPEM()})
	dead := filepath.Join(filepath.Dir(j.store), keystoreCopyPrefix(filepath.Base(j.store))+"123456")
	if err := os.WriteFile(dead, []byte("half a keystore"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(j.store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = importJavaCA(ctx, zap.NewNop(), js, certFileForTest(t, testCACertPEM()), "keploy-mitm-00000000000000aa")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("importJavaCA after the stop = %v; want it to fail with the stop", err)
	}
	if after, err := os.ReadFile(j.store); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("an edit asked after the stop changed the keystore (read: %v)", err)
	}
	if calls, err := os.ReadFile(j.keytool); err == nil && len(bytes.TrimSpace(calls)) != 0 {
		t.Fatalf("an edit asked after the stop ran keytool:\n%s", calls)
	}
	if _, err := os.Stat(dead); err != nil {
		t.Fatalf("an edit asked after the stop touched the keystore's directory: the dead edit's copy is gone (%v)", err)
	}
}

// A keystore that something other than keploy changes while keploy edits its
// copy (Debian's update-ca-certificates hook rewrites /etc/ssl/certs/java/
// cacerts in place; an operator's keytool) is not replaced by that copy, which
// would undo the change: the edit starts again from the keystore as it is.
func TestAnEditDoesNotUndoAChangeMadeUnderIt(t *testing.T) {
	const alias = "keploy-mitm-00000000000000aa"
	hold := newKeytoolHold(t, "-import", alias, false)
	hold.times = 2 // the edit's first attempt, and the one it starts over with
	j, js := keystoreForEditTest(t, keytoolFails{hold: hold}, map[string][]byte{"debian:one.pem": testCACertPEM()})
	caPath := certFileForTest(t, testCACertPEM())
	done := make(chan error, 1)
	go func() { done <- importJavaCA(context.Background(), zap.NewNop(), js, caPath, alias) }()
	hold.waitReached(t, done)
	// The distro's hook adds an entry, in place, while keploy's copy is edited.
	writeFakeKeystore(t, j.store, map[string][]byte{"debian:one.pem": testCACertPEM(), "debian:two.pem": testCACertPEM()})
	hold.letGo(t)
	hold.waitReached(t, done) // the edit's second attempt, from the keystore as it now is
	hold.letGo(t)
	if err := <-done; err != nil {
		t.Fatalf("importJavaCA: %v", err)
	}
	for _, a := range []string{"debian:one.pem", "debian:two.pem", alias} {
		if !holds(t, j.store, a) {
			t.Fatalf("after the import the keystore holds %v; want debian:two.pem, added under the edit, kept", aliasesIn(t, j.store))
		}
	}
}

// An edited copy that does not hold exactly what the keystore held with the
// edit made (a keytool that read the copy cut short, say, and wrote back only
// its own entry) is not put in place: the keystore is left as it was.
func TestAnEditWhoseCopyFailsItsCheckLeavesTheKeystore(t *testing.T) {
	j, js := keystoreForEditTest(t, keytoolFails{importDropsOthers: true}, map[string][]byte{
		"debian:one.pem": testCACertPEM(), "debian:two.pem": testCACertPEM(),
	})
	before, err := os.ReadFile(j.store)
	if err != nil {
		t.Fatal(err)
	}
	err = importJavaCA(context.Background(), zap.NewNop(), js, certFileForTest(t, testCACertPEM()), "keploy-mitm-00000000000000aa")
	if err == nil || !strings.Contains(err.Error(), "debian:one.pem") {
		t.Fatalf("importJavaCA = %v; want the check of the copy to fail, naming what it lacks", err)
	}
	if after, _ := os.ReadFile(j.store); !bytes.Equal(before, after) {
		t.Fatalf("a copy that failed its check replaced the keystore:\n%s", after)
	}
	if names := filesBeside(t, j.store); len(names) != 1 {
		t.Errorf("the failed edit left files beside the keystore: %v", names)
	}
}

// A keystore file with other hard links is not edited: renaming the edited
// copy over one name would leave the others with the keystore as it was, so a
// JVM that reads it by another name would not see the edit.
func TestAHardLinkedKeystoreIsNotSplit(t *testing.T) {
	j, js := keystoreForEditTest(t, keytoolFails{}, map[string][]byte{"debian:one.pem": testCACertPEM()})
	other := filepath.Join(t.TempDir(), "cacerts")
	if err := os.Link(j.store, other); err != nil {
		t.Skipf("no hard link here: %v", err)
	}
	before, err := os.ReadFile(j.store)
	if err != nil {
		t.Fatal(err)
	}
	err = importJavaCA(context.Background(), zap.NewNop(), js, certFileForTest(t, testCACertPEM()), "keploy-mitm-00000000000000aa")
	if err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("importJavaCA = %v; want it refused for the keystore's hard links", err)
	}
	for _, p := range []string{j.store, other} {
		if after, _ := os.ReadFile(p); !bytes.Equal(before, after) {
			t.Fatalf("a refused edit changed %s:\n%s", p, after)
		}
	}
}

// A rebuild of the system trust store and an edit of the JDK keystore do not
// run together. On Debian and Ubuntu update-ca-certificates' hook
// (ca-certificates-java) starts a JVM that reads /etc/ssl/certs/java/cacerts,
// the very keystore keploy edits, and writes it back in place with its own
// change: an edit renamed in meanwhile was lost under that write. Here the
// rebuild is a fake hook that has read the keystore and waits; an agent's
// import must wait for it, and both changes must be in the keystore at the
// end. Both rebuilds keploy runs: the install's (updateCaStore, the first of
// caStoreUpdateCmd) and the teardown's and sweep's (refreshCaStore, which
// runs update-ca-certificates by name where it exists).
func TestATrustStoreRebuildAndAKeystoreEditDoNotRunTogether(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		rebuild    func() error
	}{
		{"the install's update", "keploy-test-update-ca", func() error { return updateCaStore(context.Background(), zap.NewNop()) }},
		{"the teardown's refresh", "update-ca-certificates", func() error { return refreshCaStore(context.Background(), zap.NewNop(), false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			update := nativeTrustStoreForTest(t)
			const alias = "keploy-mitm-00000000000000aa"
			j, js := keystoreForEditTest(t, keytoolFails{}, map[string][]byte{"debian:one.pem": testCACertPEM()})
			hook := newKeytoolHold(t, "", "", false)
			block, _ := pem.Decode(testCACertPEM())
			script := `#!/bin/sh
all=$(cat '` + j.store + `')
exec 8< '` + hook.release + `'
exec 7<> '` + hook.reached + `'; echo reached >&7; exec 7>&-
read _ <&8
{ printf '%s\n' "$all" | grep -v '^#end$'; echo 'debian:hook.pem ` + base64.StdEncoding.EncodeToString(block.Bytes) + `'; echo '#end'; } > '` + j.store + `'
`
			fake := filepath.Join(update, tc.tool)
			if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", update+string(os.PathListSeparator)+os.Getenv("PATH"))
			if got, err := exec.LookPath(tc.tool); err != nil || got != fake {
				t.Fatalf("PATH finds %s at %q (%v), not the fake %s: the test would run the host's", tc.tool, got, err, fake)
			}

			rebuilt := make(chan error, 1)
			go func() { rebuilt <- tc.rebuild() }()
			hook.waitReached(t, rebuilt)

			waiting := make(chan struct{})
			var once sync.Once
			core, _ := observer.New(zapcore.DebugLevel)
			logger := zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
				if e.Message == msgTrustStoreBusy {
					once.Do(func() { close(waiting) })
				}
				return nil
			}))
			caPath := certFileForTest(t, testCACertPEM())
			imported := make(chan error, 1)
			go func() { imported <- importJavaCA(context.Background(), logger, js, caPath, alias) }()
			ranBeside := false
			select {
			case <-waiting:
			case err := <-imported:
				ranBeside = true
				imported <- err
			}
			hook.letGo(t)
			if err := <-rebuilt; err != nil {
				t.Fatalf("the rebuild: %v", err)
			}
			if err := <-imported; err != nil {
				t.Fatalf("importJavaCA: %v", err)
			}
			if !holds(t, j.store, alias) || !holds(t, j.store, "debian:hook.pem") || ranBeside {
				t.Fatalf("after a rebuild whose hook rewrote the keystore and an import beside it, the keystore holds %v (the import ran beside the rebuild: %v); want the hook's entry and %s",
					aliasesIn(t, j.store), ranBeside, alias)
			}
		})
	}
}

// A sweep that finds the retired CA in the keystore and then cannot read the
// entry from its copy says so at Warn: the CA whose key is public stays
// trusted, and a failed read is not an absence.
func TestRetiredCASweepWarnsWhenItCannotReadTheEntryOnItsCopy(t *testing.T) {
	update := legacyTestHost(t)
	root := t.TempDir()
	j := retiredJDK(t, filepath.Join(root, "opt", "jdk-21"), "", keytoolFails{exportFailsOnCopies: true})
	t.Setenv("PATH", filepath.Join(j.home, "bin")+string(os.PathListSeparator)+update)
	t.Setenv("JAVA_HOME", "")

	core, logs := observer.New(zapcore.WarnLevel)
	SweepLegacyCA(context.Background(), zap.New(core), false)
	if !holds(t, j.store, legacyCAAlias) {
		t.Fatal("the sweep removed the entry it could not read from its copy")
	}
	if !warnedWith(logs, "alias", legacyCAAlias) {
		t.Fatalf("no Warn naming %s; Warn and above logged: %v", legacyCAAlias, logs.All())
	}
}

// A copy taken while something rewrote the keystore in place can be cut
// short, and keytool then cannot read it. The edit sees that the keystore
// changed since it read it and starts over, rather than failing (and, at a
// teardown, leaving the run's CA trusted).
func TestAnEditStartsOverFromACopyTornByAWriter(t *testing.T) {
	const alias = "keploy-mitm-00000000000000aa"
	hold := newKeytoolHold(t, "-list", "", false) // the edit's first read of its copy
	j, js := keystoreForEditTest(t, keytoolFails{hold: hold}, map[string][]byte{"debian:one.pem": testCACertPEM()})
	caPath := certFileForTest(t, testCACertPEM())
	done := make(chan error, 1)
	go func() { done <- importJavaCA(context.Background(), zap.NewNop(), js, caPath, alias) }()
	hold.waitReached(t, done)

	// The writer: it rewrites the keystore, and the copy keploy took of it is
	// cut short.
	writeFakeKeystore(t, j.store, map[string][]byte{"debian:one.pem": testCACertPEM(), "debian:two.pem": testCACertPEM()})
	copies, _ := filepath.Glob(filepath.Join(filepath.Dir(j.store), keystoreCopyPrefix("cacerts")+"*"))
	if len(copies) != 1 {
		hold.letGo(t)
		<-done
		t.Fatalf("keploy's copy of the keystore is not beside it: %v", copies)
	}
	if err := os.WriteFile(copies[0], []byte("debian:one.pem MII"), 0o600); err != nil {
		t.Fatal(err)
	}
	hold.letGo(t)
	if err := <-done; err != nil {
		t.Fatalf("importJavaCA from a torn copy did not start over: %v", err)
	}
	for _, a := range []string{"debian:one.pem", "debian:two.pem", alias} {
		if !holds(t, j.store, a) {
			t.Fatalf("after the import the keystore holds %v", aliasesIn(t, j.store))
		}
	}
}
