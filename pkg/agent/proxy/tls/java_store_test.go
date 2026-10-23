package tls

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// keytoolFails says which of a fake keytool's commands fail, and where one of
// its runs stops until the test lets it go on.
type keytoolFails struct {
	delete, importing bool
	// exportFailsOnCopies makes -exportcert fail on keploy's copies of a
	// keystore though the alias is there.
	exportFailsOnCopies bool
	// importDropsOthers makes -import write back only the entry it adds, as
	// a keytool that read a keystore cut short would.
	importDropsOthers bool
	// hold, when set, stops the run of hold.op on hold.alias (see keytoolHold).
	hold *keytoolHold
}

// keytoolHold stops a fake keytool run part way, as a real one is stopped by
// a kill or by the scheduler: the run of op ("-import", "-delete", or "-list"
// with alias "" for a list of the whole keystore, which stops before it reads
// it) on alias, the first times runs of it (1 unless set before the JDK is
// made). midWrite stops it once it has truncated the keystore it was given and
// before it writes it again (where a kill leaves a real keytool's keystore
// truncated); otherwise it stops once it has read the keystore and before it
// truncates it (where another keytool's write is lost under its own).
//
// A held run writes a line to the FIFO reached and waits for a line on the
// FIFO release (on fds 7 and 8: fd 3 is the trust-store lock a keytool run
// inherits); the test blocks on each, never on a clock. The test holds both
// FIFOs open for reading and writing until it ends, so a run never blocks
// opening one, and a run still held when the test ends (it failed, or timed
// out and died) reads the end of release and goes on: no fake keytool outlives
// its test.
type keytoolHold struct {
	op, alias        string
	midWrite         bool
	times            int
	reached, release string
	reachedF         *os.File
	releaseF         *os.File
	reachedLines     *bufio.Reader
}

// newKeytoolHold makes the two FIFOs of a hold of op on alias.
func newKeytoolHold(t *testing.T, op, alias string, midWrite bool) *keytoolHold {
	t.Helper()
	dir := t.TempDir()
	h := &keytoolHold{op: op, alias: alias, midWrite: midWrite, times: 1,
		reached: filepath.Join(dir, "reached"), release: filepath.Join(dir, "release")}
	for _, p := range []string{h.reached, h.release} {
		if err := syscallMkfifo(p); err != nil {
			t.Skipf("the fake keytool's hold needs a FIFO: %v", err)
		}
	}
	var err error
	if h.reachedF, err = os.OpenFile(h.reached, os.O_RDWR, 0); err != nil {
		t.Fatal(err)
	}
	if h.releaseF, err = os.OpenFile(h.release, os.O_RDWR, 0); err != nil {
		t.Fatal(err)
	}
	h.reachedLines = bufio.NewReader(h.reachedF)
	t.Cleanup(func() {
		_ = h.releaseF.Close()
		_ = h.reachedF.Close()
	})
	return h
}

// waitReached blocks until a held run has stopped, or fails the test if the
// operation that was to make that run ends first (done is closed, or sends its
// error).
func (h *keytoolHold) waitReached(t *testing.T, done <-chan error) {
	t.Helper()
	reached := make(chan string, 1)
	go func() {
		l, _ := h.reachedLines.ReadString('\n')
		reached <- l
	}()
	select {
	case <-reached:
	case err := <-done:
		// Let the read above return.
		_, _ = h.reachedF.WriteString("ended\n")
		<-reached
		t.Fatalf("the operation ended before its keytool run on %s reached the hold: %v", h.alias, err)
	}
}

// letGo lets a held run go on.
func (h *keytoolHold) letGo(t *testing.T) {
	t.Helper()
	if _, err := h.releaseF.WriteString("go\n"); err != nil {
		t.Fatal(err)
	}
}

// fakeKeytool is a keytool that keeps a keystore in a file as the real one
// does in the ways keploy depends on: a run reads the whole keystore, makes its
// one change, and writes the whole keystore back in place (truncating the file
// it was given, then writing it); a keystore that was cut short (no "#end"
// line, as a real keystore without its integrity hash) is refused. Each entry
// is a line: the alias, lowercased as JKS does, a space, and the certificate's
// PEM body on one line. It answers -list (of the whole keystore, each entry
// as keytool prints it, or with -rfc as an "Alias name:" line; or of one
// -alias), -import,
// -exportcert -rfc and -delete, and logs every call to log. fail makes -delete
// fail though the alias is there, -import fail having written nothing (a
// keystore keploy may not write, say), or -exportcert fail on keploy's copies;
// fail.hold stops one run part way.
//
// The commands it runs are given by absolute path: the tests that use it leave
// on PATH only the fake JDKs and a fake trust-store update, so no real keytool,
// java or update-ca-certificates on the host is ever run.
func fakeKeytool(t *testing.T, log string, fail keytoolFails) string {
	t.Helper()
	tool := func(name string) string {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("the fake keytool needs %s: %v", name, err)
		}
		return "'" + p + "'"
	}
	cat, sed, tr, grep := tool("cat"), tool("sed"), tool("tr"), tool("grep")
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	holdOp, holdAlias, holdMid, holdTimes, reached, release := "", "", "0", "0", "", ""
	if h := fail.hold; h != nil {
		holdOp, holdAlias, holdMid, holdTimes, reached, release = h.op, strings.ToLower(h.alias), flag(h.midWrite), strconv.Itoa(h.times), h.reached, h.release
	}
	return `#!/bin/sh
while case $1 in -J*) true;; *) false;; esac; do shift; done
op=$1; shift
ks=; al=; f=; rfc=0
while [ $# -gt 0 ]; do
  case $1 in
    -keystore) ks=$2; shift;;
    -alias) al=$2; shift;;
    -file) f=$2; shift;;
    -rfc) rfc=1;;
  esac
  shift
done
al=$(printf '%s' "$al" | ` + tr + ` 'A-Z' 'a-z')
echo "$op keystore=$ks alias=$al" >> '` + log + `'
held=0
if [ "$op" = '` + holdOp + `' ] && [ "$al" = '` + holdAlias + `' ]; then
  n=$(` + cat + ` '` + reached + `.count' 2>/dev/null || echo 0)
  if [ "$n" -lt ` + holdTimes + ` ]; then
    held=1; echo $((n+1)) > '` + reached + `.count'
    exec 8< '` + release + `'
  fi
fi
load() {
  [ -f "$ks" ] || { echo "keytool error: java.io.FileNotFoundException: $ks (No such file or directory)"; exit 1; }
  all=$(` + cat + ` "$ks") || exit 1
  case $all in
    *'#end') ;;
    *) echo "keytool error: java.io.IOException: Keystore was tampered with, or password was incorrect"; exit 1;;
  esac
  entries=$(printf '%s\n' "$all" | ` + grep + ` -v '^#end$')
}
has() { printf '%s\n' "$entries" | ` + grep + ` -q "^$al "; }
hold() {
  [ $held = 1 ] || return 0
  [ ` + holdMid + ` = 1 ] && : > "$ks"
  exec 7<> '` + reached + `'
  echo reached >&7
  exec 7>&-
  read _ <&8
}
store() {
  hold
  { [ -n "$1" ] && printf '%s\n' "$1"; echo '#end'; } > "$ks"
}
case $op in
  -list)
    hold
    load
    if [ -z "$al" ]; then
      echo "Keystore type: JKS"
      echo
      printf '%s\n' "$entries" | while read -r a b; do
        [ -n "$a" ] || continue
        if [ $rfc = 1 ]; then printf 'Alias name: %s\nEntry type: trustedCertEntry\n\n' "$a"
        else printf '%s, Oct 23, 2026, trustedCertEntry,\n' "$a"; fi
      done
      exit 0
    fi
    has || { echo "keytool error: java.lang.Exception: Alias <$al> does not exist"; exit 1; };;
  -import)
    load
    [ ` + flag(fail.importing) + ` = 1 ] && { echo "keytool error: java.io.FileNotFoundException: $ks (Read-only file system)"; exit 1; }
    has && { echo "keytool error: java.lang.Exception: Certificate not imported, alias <$al> already exists"; exit 1; }
    body=$(` + sed + ` -e '1d' -e '$d' "$f" | ` + tr + ` -d '\n')
    [ ` + flag(fail.importDropsOthers) + ` = 1 ] && entries=
    if [ -n "$entries" ]; then store "$entries
$al $body"; else store "$al $body"; fi
    echo "Certificate was added to keystore";;
  -exportcert)
    load
    case $ks in *.keploy-edit-*) [ ` + flag(fail.exportFailsOnCopies) + ` = 1 ] && { echo "keytool error: java.lang.OutOfMemoryError"; exit 1; };; esac
    has || exit 1
    echo '-----BEGIN CERTIFICATE-----'
    printf '%s\n' "$entries" | ` + grep + ` "^$al " | ` + sed + ` 's/^[^ ]* //'
    echo '-----END CERTIFICATE-----';;
  -delete)
    load
    has || { echo "keytool error: java.lang.Exception: Alias <$al> does not exist"; exit 1; }
    [ ` + flag(fail.delete) + ` = 1 ] && { echo "keytool error: java.io.FileNotFoundException: $ks (Permission denied)"; exit 1; }
    store "$(printf '%s\n' "$entries" | ` + grep + ` -v "^$al ")";;
  *) exit 2;;
esac
`
}

// fakeJDK is one JDK on disk for these tests.
type fakeJDK struct {
	home    string // its java.home
	store   string // the file its lib/security/cacerts is (a link's target)
	keytool string // the log of every call of its keytool
}

// makeFakeJDKAt lays out a JDK at home: bin/java, which prints java.home as a
// JVM does (the JDK's real home, which the JVM derives from its own
// executable), bin/keytool (fakeKeytool, failing as fail says), and
// lib/security/cacerts, an empty keystore. With sharedStore set, cacerts is a
// link to it, as Debian's and Ubuntu's JDKs link theirs to
// /etc/ssl/certs/java/cacerts.
func makeFakeJDKAt(t *testing.T, home, sharedStore string, fail keytoolFails) fakeJDK {
	t.Helper()
	for _, d := range []string{filepath.Join(home, "bin"), filepath.Join(home, "lib", "security")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	j := fakeJDK{home: home, store: filepath.Join(home, "lib", "security", "cacerts"), keytool: home + ".keytool.log"}
	if sharedStore != "" {
		if err := os.MkdirAll(filepath.Dir(sharedStore), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(sharedStore, j.store); err != nil {
			t.Fatal(err)
		}
		j.store = sharedStore
	}
	writeFakeKeystore(t, j.store, nil)
	java := "#!/bin/sh\necho '    java.home = " + home + "' >&2\nexit 0\n"
	if err := os.WriteFile(filepath.Join(home, "bin", "java"), []byte(java), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "keytool"), []byte(fakeKeytool(t, j.keytool, fail)), 0o755); err != nil {
		t.Fatal(err)
	}
	return j
}

// writeFakeKeystore writes a fake keystore holding entries (alias to
// certificate PEM), in fakeKeytool's format.
func writeFakeKeystore(t *testing.T, store string, entries map[string][]byte) {
	t.Helper()
	var b strings.Builder
	aliases := make([]string, 0, len(entries))
	for a := range entries {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	for _, a := range aliases {
		block, _ := pem.Decode(entries[a])
		if block == nil {
			t.Fatalf("the certificate for %s is not PEM", a)
		}
		b.WriteString(strings.ToLower(a) + " " + base64.StdEncoding.EncodeToString(block.Bytes) + "\n")
	}
	b.WriteString("#end\n")
	if err := os.WriteFile(store, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// linkThroughAlternatives puts java and keytool in a usr/bin under root as
// links to an etc/alternatives entry that links to the JDK's own, as Debian's
// update-alternatives does, and returns that usr/bin.
func linkThroughAlternatives(t *testing.T, root string, j fakeJDK) string {
	t.Helper()
	bin, alt := filepath.Join(root, "usr", "bin"), filepath.Join(root, "etc", "alternatives")
	for _, d := range []string{bin, alt} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, exe := range []string{"java", "keytool"} {
		if err := os.Symlink(filepath.Join(j.home, "bin", exe), filepath.Join(alt, exe)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(alt, exe), filepath.Join(bin, exe)); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// aliasesIn lists the aliases a fake keystore holds.
func aliasesIn(t *testing.T, store string) []string {
	t.Helper()
	b, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] != "#end" {
			out = append(out, f[0])
		}
	}
	return out
}

// holds reports whether a fake keystore holds alias (JKS aliases are
// lowercase).
func holds(t *testing.T, store, alias string) bool {
	t.Helper()
	for _, a := range aliasesIn(t, store) {
		if a == strings.ToLower(alias) {
			return true
		}
	}
	return false
}

// nativeTrustStoreForTest points the system trust store at a fresh directory
// (and its bundle at a file there), its update at a command that succeeds, in
// bin, and the trust-store lock
// at a file of the test's own (trustStoreLockForTest), until the test ends. It
// also gives back, at the end, what a successful install changes for the rest
// of the process: the runtimes' CA variables (SetEnvForPath points them into
// this test's directories) and the CA's readiness.
func nativeTrustStoreForTest(t *testing.T) (bin string) {
	t.Helper()
	for _, v := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"} {
		t.Setenv(v, os.Getenv(v))
	}
	t.Cleanup(ResetCAReadyForTest)
	store, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "keploy-test-update-ca"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldPaths, oldCmds, oldBundles, oldMarker := caStorePath, caStoreUpdateCmd, legacyCABundlePaths, legacyRebuildMarker
	caStorePath, caStoreUpdateCmd = []string{store + "/"}, []string{"keploy-test-update-ca"}
	state := t.TempDir() // the bundle and the rebuild's marker: not in the anchor directory, and none unless a test writes one
	legacyCABundlePaths, legacyRebuildMarker = []string{filepath.Join(state, "ca-certificates.crt")}, filepath.Join(state, "keploy", "retired-ca-rebuild-owed")
	t.Cleanup(func() {
		caStorePath, caStoreUpdateCmd, legacyCABundlePaths, legacyRebuildMarker = oldPaths, oldCmds, oldBundles, oldMarker
		recordNativeInstall(nil)
	})
	trustStoreLockForTest(t)
	return bin
}

// A run's teardown removes its CA from the JDK keystore its install imported
// it into, however that keystore was found. The teardown used to find a
// keystore again, by rules of its own: JAVA_HOME, else the keystore beside
// PATH's keytool, its link not followed. On a Debian or Ubuntu host PATH's
// keytool is /usr/bin/keytool, a link through /etc/alternatives into the JDK,
// so the teardown looked for /usr/lib/security/cacerts, found none, and left
// every run's CA trusted by the host's JDK, one more each run (1190 on one CI
// host). Where the install had used the app's JDK or the agent had a JAVA_HOME
// of another JDK, it deleted from the wrong keystore.
func TestTeardownRemovesTheJDKAliasFromTheKeystoreTheInstallUsed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the trust-store anchors are Linux's; the fake JDK is /bin/sh scripts")
	}
	for _, tc := range []struct {
		name string
		// layout builds the JDKs under root and returns the PATH the agent
		// runs with (before the fake trust-store update's directory), the
		// JAVA_HOME it has, the --ca-java-home it was given, the JDK the
		// install must use and every other JDK, which must be left alone.
		layout func(t *testing.T, root string) (path, javaHomeEnv, override string, want fakeJDK, others []fakeJDK)
	}{{
		name: "PATH's keytool is an /etc/alternatives link (Debian, Ubuntu)",
		layout: func(t *testing.T, root string) (string, string, string, fakeJDK, []fakeJDK) {
			j := makeFakeJDKAt(t, filepath.Join(root, "usr", "lib", "jvm", "java-21-openjdk-amd64"),
				filepath.Join(root, "etc", "ssl", "certs", "java", "cacerts"), keytoolFails{})
			return linkThroughAlternatives(t, root, j), "", "", j, nil
		},
	}, {
		name: "the install used the JDK --ca-java-home names, not PATH's",
		layout: func(t *testing.T, root string) (string, string, string, fakeJDK, []fakeJDK) {
			onPath := makeFakeJDKAt(t, filepath.Join(root, "opt", "jdk-17"), "", keytoolFails{})
			app := makeFakeJDKAt(t, filepath.Join(root, "opt", "jdk-21"), "", keytoolFails{})
			return filepath.Join(onPath.home, "bin"), "", app.home, app, []fakeJDK{onPath}
		},
	}, {
		name: "the agent's JAVA_HOME names another JDK than PATH's",
		layout: func(t *testing.T, root string) (string, string, string, fakeJDK, []fakeJDK) {
			j := makeFakeJDKAt(t, filepath.Join(root, "usr", "lib", "jvm", "java-21-openjdk-amd64"),
				filepath.Join(root, "etc", "ssl", "certs", "java", "cacerts"), keytoolFails{})
			other := makeFakeJDKAt(t, filepath.Join(root, "opt", "jdk-17"), "", keytoolFails{})
			return linkThroughAlternatives(t, root, j), other.home, "", j, []fakeJDK{other}
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			update := nativeTrustStoreForTest(t)
			path, javaHomeEnv, override, want, others := tc.layout(t, t.TempDir())
			t.Setenv("PATH", path+string(os.PathListSeparator)+update)
			t.Setenv("JAVA_HOME", javaHomeEnv)

			ca, err := generateRunCA()
			if err != nil {
				t.Fatal(err)
			}
			alias := runAlias(ca)
			if err := setupNativeForApp(context.Background(), zap.NewNop(), ca, 0, override); err != nil {
				t.Fatalf("setupNativeForApp: %v", err)
			}
			if !holds(t, want.store, alias) {
				t.Fatalf("the install did not import %s into %s (it holds %v); the test's layout is wrong", alias, want.store, aliasesIn(t, want.store))
			}

			TeardownNativeCA(zap.NewNop())
			if holds(t, want.store, alias) {
				log, _ := os.ReadFile(want.keytool)
				t.Fatalf("the teardown left this run's CA %s in the keystore its install imported it into, %s\n--- that JDK's keytool calls ---\n%s",
					alias, want.store, log)
			}
			// The install's JDK is the only one keploy runs a keytool of: the
			// retired CA's sweep looks in it, the install imports into it and
			// the teardown deletes from it.
			for _, o := range others {
				if log, _ := os.ReadFile(o.keytool); len(log) != 0 {
					t.Errorf("keploy ran the keytool of a JDK it did not install into (%s):\n%s", o.home, log)
				}
			}
		})
	}
}

// The boot's sweep of the retired static CA ("keployCA", whose key was public)
// finds the JDK keystore the way an install does: through PATH's keytool link
// to the JDK on a Debian or Ubuntu host, it used to look for
// /usr/lib/security/cacerts, and left the retired CA trusted.
func TestLegacyJDKAliasSweepFindsTheKeystoreThroughAlternatives(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake JDK is /bin/sh scripts")
	}
	update := nativeTrustStoreForTest(t)
	root := t.TempDir()
	j := makeFakeJDKAt(t, filepath.Join(root, "usr", "lib", "jvm", "java-21-openjdk-amd64"),
		filepath.Join(root, "etc", "ssl", "certs", "java", "cacerts"), keytoolFails{})
	t.Setenv("PATH", linkThroughAlternatives(t, root, j)+string(os.PathListSeparator)+update)
	t.Setenv("JAVA_HOME", "")

	// The retired CA stands in as a CA the test can make: the sweep matches by
	// fingerprint only.
	retired, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	writeFakeKeystore(t, j.store, map[string][]byte{legacyCAAlias: retired.certPEM})

	// What SweepLegacyCA does, for this fingerprint.
	ctx := context.Background()
	sweepCAByFingerprint(ctx, zap.NewNop(), sha256.Sum256(retired.der), false, false, javaStoresFor(ctx, zap.NewNop(), ""))
	if holds(t, j.store, legacyCAAlias) {
		log, _ := os.ReadFile(j.keytool)
		t.Fatalf("the sweep left the retired CA %q in %s\n--- keytool calls ---\n%s", legacyCAAlias, j.store, log)
	}
}

// A teardown that cannot remove this run's CA from the JDK keystore says so
// at Warn, naming the alias and the keystore: the JDK then trusts the CA of a
// run that is over. Before, it was a Debug line, and on the hosts where the
// keystore was never found, not even that.
func TestTeardownWarnsWhenItCannotRemoveTheJDKAlias(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the trust-store anchors are Linux's; the fake JDK is /bin/sh scripts")
	}
	update := nativeTrustStoreForTest(t)
	root := t.TempDir()
	j := makeFakeJDKAt(t, filepath.Join(root, "usr", "lib", "jvm", "java-21-openjdk-amd64"),
		filepath.Join(root, "etc", "ssl", "certs", "java", "cacerts"), keytoolFails{delete: true})
	t.Setenv("PATH", linkThroughAlternatives(t, root, j)+string(os.PathListSeparator)+update)
	t.Setenv("JAVA_HOME", "")

	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	alias := runAlias(ca)
	if err := setupNativeForApp(context.Background(), zap.NewNop(), ca, 0, ""); err != nil {
		t.Fatalf("setupNativeForApp: %v", err)
	}
	core, logs := observer.New(zapcore.WarnLevel)
	TeardownNativeCA(zap.New(core))

	var found bool
	for _, e := range logs.All() {
		f := e.ContextMap()
		if e.Level == zapcore.WarnLevel && f["alias"] == alias && f["cacerts"] != nil && strings.Contains(f["cacerts"].(string), "cacerts") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no Warn naming alias %s and its keystore; Warn and above logged: %v", alias, logs.All())
	}
}

// A teardown with nothing to remove from the JDK keystore (its import never
// happened: the keystore was not writable, the stop cut it short) logs no
// Warn: there is no CA left behind to warn of.
func TestTeardownDoesNotWarnWhenTheJDKAliasIsAlreadyGone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the trust-store anchors are Linux's; the fake JDK is /bin/sh scripts")
	}
	update := nativeTrustStoreForTest(t)
	root := t.TempDir()
	j := makeFakeJDKAt(t, filepath.Join(root, "usr", "lib", "jvm", "java-21-openjdk-amd64"),
		filepath.Join(root, "etc", "ssl", "certs", "java", "cacerts"), keytoolFails{})
	t.Setenv("PATH", linkThroughAlternatives(t, root, j)+string(os.PathListSeparator)+update)
	t.Setenv("JAVA_HOME", "")

	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := setupNativeForApp(context.Background(), zap.NewNop(), ca, 0, ""); err != nil {
		t.Fatalf("setupNativeForApp: %v", err)
	}
	// Someone (keploy ca clean, an operator) removed it already.
	writeFakeKeystore(t, j.store, nil)
	core, logs := observer.New(zapcore.WarnLevel)
	TeardownNativeCA(zap.New(core))
	if n := logs.Len(); n != 0 {
		t.Fatalf("the teardown warned with nothing left to remove: %v", logs.All())
	}
}

// Two JDK homes whose cacerts are one file (Debian links every JDK's to
// /etc/ssl/certs/java/cacerts) are one keystore: the retired CA's sweep edits
// it once.
func TestJavaStoresForEditsOneKeystoreOnce(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake JDK is /bin/sh scripts")
	}
	root := t.TempDir()
	shared := filepath.Join(root, "etc", "ssl", "certs", "java", "cacerts")
	a := makeFakeJDKAt(t, filepath.Join(root, "usr", "lib", "jvm", "java-17-openjdk-amd64"), shared, keytoolFails{})
	b := makeFakeJDKAt(t, filepath.Join(root, "usr", "lib", "jvm", "java-21-openjdk-amd64"), "", keytoolFails{})
	// b's cacerts links to the same file as a's.
	bStore := filepath.Join(b.home, "lib", "security", "cacerts")
	if err := os.Remove(bStore); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, bStore); err != nil {
		t.Fatal(err)
	}
	other := makeFakeJDKAt(t, filepath.Join(root, "opt", "jdk-11"), "", keytoolFails{})

	got := javaStoresFor(context.Background(), zap.NewNop(), a.home, b.home, a.home, other.home)
	if len(got) != 2 {
		t.Fatalf("javaStoresFor = %+v, want a's keystore and other's: b's is the same file as a's", got)
	}
	if got[0].keytool != filepath.Join(a.home, "bin", "keytool") || got[1].cacerts != filepath.Join(other.home, "lib", "security", "cacerts") {
		t.Fatalf("javaStoresFor = %+v, want a's then other's, each with its own JDK's keytool", got)
	}
}

// A keploy install whose import keytool refused (a read-only JDK, a keystore
// password that is not changeit) leaves the keystore as it was, so its
// teardown runs no keytool there and warns of nothing: there is nothing of
// this run's in it.
func TestTeardownLeavesAKeystoreWhoseImportWasRefused(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the trust-store anchors are Linux's; the fake JDK is /bin/sh scripts")
	}
	update := nativeTrustStoreForTest(t)
	root := t.TempDir()
	j := makeFakeJDKAt(t, filepath.Join(root, "usr", "lib", "jvm", "java-21-openjdk-amd64"),
		filepath.Join(root, "etc", "ssl", "certs", "java", "cacerts"), keytoolFails{importing: true})
	t.Setenv("PATH", linkThroughAlternatives(t, root, j)+string(os.PathListSeparator)+update)
	t.Setenv("JAVA_HOME", "")

	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := setupNativeForApp(context.Background(), zap.NewNop(), ca, 0, ""); err != nil {
		t.Fatalf("setupNativeForApp: a refused JDK import must not fail the install: %v", err)
	}
	before, _ := os.ReadFile(j.keytool)
	core, logs := observer.New(zapcore.WarnLevel)
	TeardownNativeCA(zap.New(core))
	after, _ := os.ReadFile(j.keytool)
	if len(after) != len(before) {
		t.Errorf("the teardown ran the JDK's keytool for an import keytool refused:\n%s", after[len(before):])
	}
	if logs.Len() != 0 {
		t.Errorf("the teardown warned of an import that never happened: %v", logs.All())
	}
}

// The keystore is the JDK's own and the keytool too: an import edits
// $javaHome/lib/security/cacerts with $javaHome/bin/keytool, whatever keytool
// PATH has.
func TestImportJavaCAUsesTheJDKsOwnKeytool(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake JDK is /bin/sh scripts")
	}
	trustStoreLockForTest(t)
	root := t.TempDir()
	j := makeFakeJDKAt(t, filepath.Join(root, "opt", "jdk-21"), "", keytoolFails{})
	onPath := makeFakeJDKAt(t, filepath.Join(root, "usr", "lib", "jvm", "default"), "", keytoolFails{})
	t.Setenv("PATH", filepath.Join(onPath.home, "bin"))

	ctx := context.Background()
	js, ok, err := resolveJavaStore(ctx, zap.NewNop(), j.home)
	if err != nil || !ok {
		t.Fatalf("resolveJavaStore(%s) = %+v, %v, %v", j.home, js, ok, err)
	}
	if want := (javaStore{keytool: filepath.Join(j.home, "bin", "keytool"), cacerts: j.store}); js != want {
		t.Fatalf("resolveJavaStore = %+v, want %+v", js, want)
	}
	caPath := filepath.Join(root, "ca.crt")
	if err := os.WriteFile(caPath, testCACertPEM(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := importJavaCA(ctx, zap.NewNop(), js, caPath, "keploy-mitm-0000000000000001"); err != nil {
		t.Fatalf("importJavaCA: %v", err)
	}
	if !holds(t, j.store, "keploy-mitm-0000000000000001") {
		t.Fatalf("the import did not land in %s", j.store)
	}
	if log, _ := os.ReadFile(onPath.keytool); len(log) != 0 {
		t.Fatalf("PATH's keytool ran for a JDK that has its own:\n%s", log)
	}
}

// resolveJavaStore's rules, row by row: where a JDK's keystore is, which
// keytool edits it, and when there is none to edit.
func TestResolveJavaStore(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake JDK is /bin/sh scripts")
	}
	write := func(t *testing.T, p string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("a JDK 8 root keeps its keystore under jre", func(t *testing.T) {
		home := t.TempDir()
		write(t, filepath.Join(home, "bin", "keytool"), 0o755)
		write(t, filepath.Join(home, "jre", "lib", "security", "cacerts"), 0o644)
		js, ok, err := resolveJavaStore(context.Background(), zap.NewNop(), home)
		if err != nil || !ok || js.cacerts != filepath.Join(home, "jre", "lib", "security", "cacerts") {
			t.Fatalf("resolveJavaStore = %+v, %v, %v; want the jre keystore", js, ok, err)
		}
	})
	t.Run("a home with no keystore has none to edit", func(t *testing.T) {
		// keytool -import would create a keystore there that no JVM reads.
		home := t.TempDir()
		write(t, filepath.Join(home, "bin", "keytool"), 0o755)
		if err := os.MkdirAll(filepath.Join(home, "lib", "security"), 0o755); err != nil {
			t.Fatal(err)
		}
		if js, ok, err := resolveJavaStore(context.Background(), zap.NewNop(), home); err == nil || ok {
			t.Fatalf("resolveJavaStore = %+v, %v, %v; want an error", js, ok, err)
		}
	})
	t.Run("a JRE-only home is edited with PATH's keytool, by its path", func(t *testing.T) {
		home, bin := t.TempDir(), t.TempDir()
		write(t, filepath.Join(home, "lib", "security", "cacerts"), 0o644)
		write(t, filepath.Join(bin, "keytool"), 0o755)
		t.Setenv("PATH", bin)
		js, ok, err := resolveJavaStore(context.Background(), zap.NewNop(), home)
		if err != nil || !ok || js.keytool != filepath.Join(bin, "keytool") {
			t.Fatalf("resolveJavaStore = %+v, %v, %v; want PATH's keytool %s", js, ok, err, filepath.Join(bin, "keytool"))
		}
	})
	t.Run("no keytool anywhere is an error", func(t *testing.T) {
		home := t.TempDir()
		write(t, filepath.Join(home, "lib", "security", "cacerts"), 0o644)
		t.Setenv("PATH", "/nonexistent-path-for-test")
		if js, ok, err := resolveJavaStore(context.Background(), zap.NewNop(), home); err == nil || ok {
			t.Fatalf("resolveJavaStore = %+v, %v, %v; want an error", js, ok, err)
		}
	})
	t.Run("no java on PATH is no JDK, and no error", func(t *testing.T) {
		t.Setenv("PATH", "/nonexistent-path-for-test")
		if js, ok, err := resolveJavaStore(context.Background(), zap.NewNop(), ""); err != nil || ok {
			t.Fatalf("resolveJavaStore(\"\") = %+v, %v, %v; want no JDK, no error", js, ok, err)
		}
	})
}

// An import the stop cuts short (its context cancelled) leaves the keystore as
// it was: the import is made on a copy, which replaces the keystore only once
// it is complete (editJavaKeystore). So the run's record holds no JDK keystore,
// and its teardown runs no keytool there.
func TestAnImportTheStopCutShortLeavesNothingToRemove(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the trust-store anchors are Linux's; the fake JDK is /bin/sh scripts")
	}
	update := nativeTrustStoreForTest(t)
	j := makeFakeJDKAt(t, filepath.Join(t.TempDir(), "opt", "jdk-21"), "", keytoolFails{})
	writeFakeKeystore(t, j.store, map[string][]byte{"debian:one.pem": testCACertPEM()})
	before, err := os.ReadFile(j.store)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", update)

	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	// The stop came before the import could run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := setupNativeForApp(ctx, zap.NewNop(), ca, 0, j.home); err == nil {
		t.Fatal("an install whose context was cancelled succeeded")
	}
	if after, _ := os.ReadFile(j.store); !bytes.Equal(before, after) {
		t.Fatalf("a cut-short import changed the keystore:\n%s", after)
	}
	installedNativeMu.Lock()
	var got javaStore
	if installedNative != nil {
		got = installedNative.java
	}
	installedNativeMu.Unlock()
	if got != (javaStore{}) {
		t.Fatalf("after a cut-short import the run's record holds JDK keystore %+v; it holds nothing of this run's", got)
	}
	logBefore, _ := os.ReadFile(j.keytool)
	TeardownNativeCA(zap.NewNop())
	if logAfter, _ := os.ReadFile(j.keytool); len(logAfter) != len(logBefore) {
		t.Fatalf("the teardown ran keytool for an import that changed nothing:\n%s", logAfter[len(logBefore):])
	}
}

// javaAliasHeld and removeJavaAlias against a real keytool, on a keystore of
// the test's own (never a JDK's): the exit codes and the -list output the fake
// keytool imitates are the real one's.
func TestJavaAliasProbeAgainstRealKeytool(t *testing.T) {
	keytool, err := exec.LookPath("keytool")
	if err != nil {
		t.Skip("no keytool on PATH")
	}
	trustStoreLockForTest(t)
	dir := t.TempDir()
	js := javaStore{keytool: keytool, cacerts: filepath.Join(dir, "cacerts")}
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, testCACertPEM(), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const alias = "keploy-mitm-0123456789abcdef"
	if out, err := exec.CommandContext(ctx, keytool, "-importcert", "-noprompt", "-trustcacerts", "-storetype", "JKS",
		"-alias", alias, "-file", caPath, "-keystore", js.cacerts, "-storepass", javaStorePass).CombinedOutput(); err != nil {
		t.Fatalf("keytool -importcert: %v\n%s", err, out)
	}
	if got := javaAliasHeld(ctx, js, alias); got != aliasPresent {
		t.Fatalf("javaAliasHeld of an imported alias = %v, want present", got)
	}
	if got := javaAliasHeld(ctx, js, "keploy-mitm-ffffffffffffffff"); got != aliasAbsent {
		t.Fatalf("javaAliasHeld of an alias never imported = %v, want absent", got)
	}
	if got := javaAliasHeld(ctx, javaStore{keytool: keytool, cacerts: filepath.Join(dir, "none")}, alias); got != aliasUnknown {
		t.Fatalf("javaAliasHeld of a keystore that does not exist = %v, want unknown", got)
	}

	core, logs := observer.New(zapcore.WarnLevel)
	removeJavaAlias(ctx, zap.New(core), js, alias)
	if got := javaAliasHeld(ctx, js, alias); got != aliasAbsent {
		t.Fatalf("after removeJavaAlias the alias is %v, want absent", got)
	}
	removeJavaAlias(ctx, zap.New(core), js, alias) // already gone: nothing to warn of
	if logs.Len() != 0 {
		t.Fatalf("removeJavaAlias warned: %v", logs.All())
	}
}
