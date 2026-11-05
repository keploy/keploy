package tls

import (
	"context"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pavlo-v-chernykh/keystore-go/v4"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.uber.org/zap"
)

// A JVM takes seconds to start on a starved host, and the agent's start waits
// for every one the CA's native install runs. These rows hold the install to
// the JVMs its work needs.

// writeScript writes an executable /bin/sh script at path.
func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeJDK lays out a JDK under root whose java and keytool log each run to
// calls: java says javaHome as its java.home, keytool -list finds no alias.
// cacertsUnder is where lib/security/cacerts goes ("" for none), relative to
// root.
func fakeJDK(t *testing.T, root, cacertsUnder, javaHome, calls string) {
	t.Helper()
	writeScript(t, filepath.Join(root, "bin", "java"),
		"echo \"java $*\" >> "+calls+"\n"+
			"echo \"    java.home = "+javaHome+"\" >&2\n")
	writeScript(t, filepath.Join(root, "bin", "keytool"),
		"echo \"keytool $*\" >> "+calls+"\n"+
			"case \"$1\" in -list) exit 1;; esac\n")
	if cacertsUnder != "" {
		cacerts := filepath.Join(root, cacertsUnder, "lib", "security", "cacerts")
		if err := os.MkdirAll(filepath.Dir(cacerts), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cacerts, []byte("cacerts"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readCalls(t *testing.T, calls string) []string {
	t.Helper()
	b, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// The install wrote its anchor to each trust store directory and imported it
// into the JDK's keystore once per directory: the second pass ran keytool
// -list again (and, with no JDK named, the PATH JDK's lookup) only to find the
// alias the first had imported.
func TestNativeInstallImportsTheCAIntoTheJDKOnceForAllItsAnchors(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the trust-store anchors are Linux's")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	jdk := filepath.Join(dir, "jdk")
	fakeJDK(t, jdk, ".", jdk, calls)
	bin, home := filepath.Join(dir, "bin"), filepath.Join(dir, "home")
	writeScript(t, filepath.Join(bin, "keploy-test-update-ca"), "exit 0\n")
	storeA, storeB := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, d := range []string{storeA, storeB, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("JAVA_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("SUDO_USER", "")
	for _, k := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"} {
		t.Setenv(k, "")
	}
	oldPaths, oldCmds := caStorePath, caStoreUpdateCmd
	caStorePath, caStoreUpdateCmd = []string{storeA + "/", storeB + "/"}, []string{"keploy-test-update-ca"}
	t.Cleanup(func() {
		caStorePath, caStoreUpdateCmd = oldPaths, oldCmds
		recordNativeInstall(nil)
		ResetCAReadyForTest()
	})

	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := setupNativeForApp(context.Background(), zap.NewNop(), ca, nil, 0, jdk); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{storeA, storeB} {
		if _, err := os.Stat(filepath.Join(d, runCertFileName(ca))); err != nil {
			t.Fatalf("no anchor in %s: %v", d, err)
		}
	}
	got := readCalls(t, calls)
	if len(got) != 2 || !strings.HasPrefix(got[0], "keytool -list ") || !strings.HasPrefix(got[1], "keytool -import ") {
		t.Fatalf("the install's JVM runs: %q, want one keytool -list and one keytool -import", got)
	}
	if !strings.Contains(got[1], " -alias "+runAlias(ca)+" ") || !strings.Contains(got[1], " -keystore "+filepath.Join(jdk, "lib", "security", "cacerts")+" ") {
		t.Fatalf("keytool -import: %q, want this run's alias into the named JDK's cacerts", got[1])
	}
	if !strings.HasSuffix(got[1], " -file "+filepath.Join(storeB, runCertFileName(ca))) {
		t.Fatalf("keytool -import: %q, want it to read this run's last anchor written", got[1])
	}
	TeardownNativeCA(zap.NewNop())
}

// With no JDK named, the install looked up the PATH JDK's java.home by
// running it (java -XshowSettings:properties): a JVM before keytool's own.
// The launcher derives java.home from where its binary lies, so keploy reads
// it from the same place, and asks the JVM only of a java it cannot read.
func TestThePATHJDKIsReadFromItsLayoutAndAskedOnlyWhenItCannotBe(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake JDK's binaries are /bin/sh scripts")
	}
	for _, tc := range []struct {
		name string
		// lay builds the PATH directory's java under dir and returns the
		// java.home pathJavaHome must give, and whether a JVM is asked.
		lay func(t *testing.T, dir, calls string) (path, want string, asked bool)
	}{
		{"a JDK 9 or later behind the alternatives' symlinks", func(t *testing.T, dir, calls string) (string, string, bool) {
			jdk := filepath.Join(dir, "jvm", "jdk-21")
			fakeJDK(t, jdk, ".", "/not/read", calls)
			alt, usrBin := filepath.Join(dir, "alternatives"), filepath.Join(dir, "usr-bin")
			for _, d := range []string{alt, usrBin} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(jdk, "bin", "java"), filepath.Join(alt, "java")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(alt, "java"), filepath.Join(usrBin, "java")); err != nil {
				t.Fatal(err)
			}
			return usrBin, jdk, false
		}},
		{"a JDK 9 or later with a stray jre/ beside its lib/", func(t *testing.T, dir, calls string) (string, string, bool) {
			jdk := filepath.Join(dir, "jdk-17")
			fakeJDK(t, jdk, ".", "/not/read", calls)
			stray := filepath.Join(jdk, "jre", "lib", "security", "cacerts")
			if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stray, []byte("cacerts"), 0o644); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(jdk, "bin"), jdk, false
		}},
		{"a JDK 8, whose java.home is its jre", func(t *testing.T, dir, calls string) (string, string, bool) {
			jdk := filepath.Join(dir, "jdk8")
			fakeJDK(t, jdk, "jre", "/not/read", calls)
			return filepath.Join(jdk, "bin"), filepath.Join(jdk, "jre"), false
		}},
		{"a version manager's shim", func(t *testing.T, dir, calls string) (string, string, bool) {
			shims := filepath.Join(dir, "shims")
			writeScript(t, filepath.Join(shims, "java"),
				"echo \"java $*\" >> "+calls+"\n"+
					"echo \"    java.home = /opt/jdk-17\" >&2\n")
			return shims, "/opt/jdk-17", true
		}},
		{"a bin/java with no cacerts beside it", func(t *testing.T, dir, calls string) (string, string, bool) {
			root := filepath.Join(dir, "wrapper")
			fakeJDK(t, root, "", "/opt/jdk-11", calls)
			return filepath.Join(root, "bin"), "/opt/jdk-11", true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(dir, "calls")
			path, want, asked := tc.lay(t, dir, calls)
			t.Setenv("PATH", path)
			got, err := pathJavaHome(context.Background())
			if err != nil {
				t.Fatalf("pathJavaHome: %v", err)
			}
			if got != want {
				t.Fatalf("pathJavaHome = %q, want %q", got, want)
			}
			ran := readCalls(t, calls)
			if asked && (len(ran) != 1 || !strings.HasPrefix(ran[0], "java -XshowSettings:properties")) {
				t.Fatalf("JVM runs: %q, want the one that says its java.home", ran)
			}
			if !asked && len(ran) != 0 {
				t.Fatalf("JVM runs: %q, want none: the layout says java.home", ran)
			}
		})
	}
}

// End to end: with no JDK named, keytool -list and -import are the install's
// only JVM runs, and they are the PATH JDK's own, on its cacerts.
func TestJavaInstallWithNoJDKNamedRunsOnlyThePATHJDKsKeytool(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fake JDK's binaries are /bin/sh scripts")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	jdk := filepath.Join(dir, "jdk")
	fakeJDK(t, jdk, ".", "/not/read", calls)
	t.Setenv("PATH", filepath.Join(jdk, "bin"))
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, []byte("ca"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installJavaCAForHome(context.Background(), zap.NewNop(), caPath, "", "keploy-mitm-x"); err != nil {
		t.Fatal(err)
	}
	cacerts := filepath.Join(jdk, "lib", "security", "cacerts")
	got := readCalls(t, calls)
	if len(got) != 2 ||
		!strings.HasPrefix(got[0], "keytool -list -keystore "+cacerts+" ") ||
		!strings.HasPrefix(got[1], "keytool -import -trustcacerts -keystore "+cacerts+" ") {
		t.Fatalf("JVM runs: %q, want keytool -list and -import on %s and nothing else", got, cacerts)
	}
}

// The layout gives the java.home a real JDK's own JVM says. A host with no
// JDK under /usr/lib/jvm (or $JAVA_HOME) has none to compare, and skips.
func TestThePATHJDKsLayoutGivesTheJavaHomeItsJVMSays(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the JDK layouts read here are Linux's")
	}
	javas, _ := filepath.Glob("/usr/lib/jvm/*/bin/java")
	if jh := os.Getenv("JAVA_HOME"); jh != "" {
		javas = append(javas, filepath.Join(jh, "bin", "java"))
	}
	seen := map[string]bool{}
	for _, java := range javas {
		resolved, err := filepath.EvalSymlinks(java)
		if err != nil || seen[resolved] {
			continue
		}
		seen[resolved] = true
		t.Run(java, func(t *testing.T) {
			t.Setenv("PATH", filepath.Dir(java))
			fromLayout := javaHomeFromPATHLayout()
			fromJVM, err := util.GetJavaHome(context.Background())
			if err != nil {
				t.Skipf("%s does not run here: %v", java, err)
			}
			if fromLayout != fromJVM {
				t.Fatalf("java.home from the layout of %s = %q, its JVM says %q", java, fromLayout, fromJVM)
			}
		})
	}
	if len(seen) == 0 {
		t.Skip("no JDK on this host")
	}
}

// writeJKS stores a JKS keystore at path, under pass, trusting each
// certificate (DER) under its alias.
func writeJKS(t *testing.T, path, pass string, certs map[string][]byte) {
	t.Helper()
	ks := keystore.New()
	for alias, der := range certs {
		if err := ks.SetTrustedCertificateEntry(alias, keystore.TrustedCertificateEntry{
			CreationTime: time.Now(),
			Certificate:  keystore.Certificate{Type: "X509", Content: der},
		}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Store(f, []byte(pass)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// On Debian and Ubuntu the system store's update puts a new anchor into the
// keystore a packaged JDK's cacerts links to, under an alias of its own
// (ca-certificates-java's debian:<anchor>.pem; Adoptium's rebuild, under the
// certificate's name). keploy reads that keystore in Go to see whether it
// trusts the CA already: only a JKS whose integrity checks with the store's
// password, holding this very certificate, says yes.
func TestTheJDKKeystoreIsReadInGoForTheCA(t *testing.T) {
	dir := t.TempDir()
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	other, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	keyPEM := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ca.cert.Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	ks := func(name, pass string, certs map[string][]byte) string {
		p := filepath.Join(dir, name)
		writeJKS(t, p, pass, certs)
		return p
	}
	withCA := ks("with-ca", "changeit", map[string][]byte{"debian:keploy-mitm-x.pem": ca.cert.Raw, "debian:other.pem": other.cert.Raw})
	notPEM := filepath.Join(dir, "not-pem.crt")
	if err := os.WriteFile(notPEM, []byte("not a PEM file"), 0o644); err != nil {
		t.Fatal(err)
	}
	notJKS := filepath.Join(dir, "pkcs12")
	if err := os.WriteFile(notJKS, []byte{0x30, 0x82, 0x01, 0x00, 0x02, 0x01, 0x03}, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, cacerts, ca string
		want              bool
	}{
		{"the CA under another alias", withCA, caPath, true},
		{"only another certificate", ks("other-only", "changeit", map[string][]byte{"debian:other.pem": other.cert.Raw}), caPath, false},
		{"an empty keystore", ks("empty", "changeit", nil), caPath, false},
		{"the CA under another password", ks("other-pass", "secret", map[string][]byte{"keploy": ca.cert.Raw}), caPath, false},
		{"not a JKS", notJKS, caPath, false},
		{"no keystore", filepath.Join(dir, "none"), caPath, false},
		{"no CA file", withCA, filepath.Join(dir, "none.crt"), false},
		{"a CA file with no certificate", withCA, keyPEM, false},
		{"a CA file with no PEM", withCA, notPEM, false},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: jdkKeystoreTrusts panicked: %v", tc.name, r)
				}
			}()
			if got := jdkKeystoreTrusts(tc.cacerts, "changeit", tc.ca); got != tc.want {
				t.Errorf("%s: jdkKeystoreTrusts = %v, want %v", tc.name, got, tc.want)
			}
		}()
	}
}

// A stop that tears the install down while it runs takes its record: the
// install's note of its JDK import then finds none, and changes nothing.
func TestAJavaImportNotedAfterTheTeardownChangesNothing(t *testing.T) {
	recordNativeInstall(nil)
	t.Cleanup(func() { recordNativeInstall(nil) })
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("noteJavaImport with no install recorded panicked: %v", r)
			}
		}()
		noteJavaImport(javaInstall{})
	}()
	installedNativeMu.Lock()
	n := installedNative
	installedNativeMu.Unlock()
	if n != nil {
		t.Fatalf("noteJavaImport with no install recorded recorded %+v", n)
	}
}

// The install imports the CA into the JDK's keystore after the system
// store's update, and runs no keytool when that update put the CA there
// (here the update puts a keystore trusting it in the JDK's cacerts, as
// Debian's and Adoptium's hooks do). Before, the import ran first, with its
// keytool -list and -import, and the teardown's keytool -delete. At the
// teardown, a hook that rebuilds its keystore from the anchors (Adoptium's)
// has dropped the CA with the anchor, and no keytool runs; one that is not
// told of the anchor's removal (Debian's) leaves it, and the teardown
// deletes the hook's entry, which holds this run's CA. Where the update
// does not put it there, the install imports it, and the teardown deletes
// the alias: also when keytool found the alias there already, and when its
// import failed part way. With no JDK on PATH and none named, the install
// finds none and imports nothing: the teardown, which finds a JDK by
// JAVA_HOME, runs no keytool either.
func TestAJDKKeystoreTheStoreUpdateFilledRunsNoKeytoolAtTheInstall(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the trust-store anchors are Linux's")
	}
	for _, tc := range []struct {
		name    string
		hook    string   // "keeps": the update puts the CA there and leaves it (Debian's); "rebuilds": while its anchor is there (Adoptium's); "unreadable": then leaves a keystore Go cannot read; "": never
		keytool string   // the JDK's keytool's exit, by its first word; "" for fakeJDK's
		noJDK   bool     // no JDK named, nor a java on PATH, at the install
		want    []string // each keytool run, "<op> run" of this run's alias, "<op> hook" of the hook's
	}{
		{"the update put the CA there and rebuilds without it", "rebuilds", "", false, nil},
		{"the update put the CA there and leaves it", "keeps", "", false, []string{"-delete hook"}},
		{"the update put the CA there, then leaves a keystore Go cannot read", "unreadable", "", false, nil},
		{"the update left the keystore alone", "", "", false, []string{"-list run", "-import run", "-delete run"}},
		{"keytool finds the alias there already", "", "-list) exit 0;;", false, []string{"-list run", "-delete run"}},
		{"keytool's import fails", "", "-list|-import) exit 1;;", false, []string{"-list run", "-import run", "-delete run"}},
		{"no JDK at the install", "", "", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			jdk := filepath.Join(dir, "jdk")
			fakeJDK(t, jdk, ".", jdk, calls)
			if tc.keytool != "" {
				writeScript(t, filepath.Join(jdk, "bin", "keytool"), "echo \"keytool $*\" >> "+calls+"\ncase \"$1\" in "+tc.keytool+" esac\n")
			}
			bin, home := filepath.Join(dir, "bin"), filepath.Join(dir, "home")
			hooked, rebuilt := filepath.Join(dir, "hooked.jks"), filepath.Join(dir, "rebuilt.jks")
			store := filepath.Join(dir, "a")
			cacerts := filepath.Join(jdk, "lib", "security", "cacerts")
			update := "exit 0\n"
			switch tc.hook {
			case "keeps":
				update = "/bin/cp " + hooked + " " + cacerts + "\n"
			case "rebuilds":
				update = "if [ -e " + store + "/*.crt ]; then /bin/cp " + hooked + " " + cacerts + "; else /bin/cp " + rebuilt + " " + cacerts + "; fi\n"
			case "unreadable":
				update = "if [ -e " + store + "/*.crt ]; then /bin/cp " + hooked + " " + cacerts + "; else echo PKCS12 > " + cacerts + "; fi\n"
			}
			writeScript(t, filepath.Join(bin, "keploy-test-update-ca"), update)
			for _, d := range []string{store, home} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)
			t.Setenv("JAVA_HOME", "")
			t.Setenv("HOME", home)
			t.Setenv("SUDO_USER", "")
			for _, k := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"} {
				t.Setenv(k, "")
			}
			oldPaths, oldCmds, oldDebian := caStorePath, caStoreUpdateCmd, debianJavaKeystore
			caStorePath, caStoreUpdateCmd = []string{store + "/"}, []string{"keploy-test-update-ca"}
			debianJavaKeystore = filepath.Join(dir, "no-debian-keystore")
			t.Cleanup(func() {
				caStorePath, caStoreUpdateCmd, debianJavaKeystore = oldPaths, oldCmds, oldDebian
				recordNativeInstall(nil)
				ResetCAReadyForTest()
			})
			ca, err := generateRunCA()
			if err != nil {
				t.Fatal(err)
			}
			hookAlias := "debian:" + strings.TrimSuffix(runCertFileName(ca), ".crt") + ".pem"
			writeJKS(t, hooked, "changeit", map[string][]byte{hookAlias: ca.cert.Raw})
			writeJKS(t, rebuilt, "changeit", nil)

			named := jdk
			if tc.noJDK {
				named = ""
			}
			if err := setupNativeForApp(context.Background(), zap.NewNop(), ca, nil, 0, named); err != nil {
				t.Fatal(err)
			}
			// The teardown finds the JDK by JAVA_HOME.
			t.Setenv("JAVA_HOME", jdk)
			TeardownNativeCA(zap.NewNop())
			got := readCalls(t, calls)
			if len(got) != len(tc.want) {
				t.Fatalf("the install's and teardown's JVM runs: %q, want %q", got, tc.want)
			}
			for i, w := range tc.want {
				op, whose, _ := strings.Cut(w, " ")
				alias := runAlias(ca)
				if whose == "hook" {
					alias = hookAlias
				}
				if !strings.HasPrefix(got[i], "keytool "+op+" ") || !strings.Contains(got[i]+" ", " -alias "+alias+" ") || !strings.Contains(got[i]+" ", " -keystore "+cacerts+" ") {
					t.Fatalf("the install's and teardown's JVM runs: %q, want %q (run: %s, hook: %s) in %s", got, tc.want, runAlias(ca), hookAlias, cacerts)
				}
			}
		})
	}
}
