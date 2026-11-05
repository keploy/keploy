package tls

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
	if err := installJavaCAForHome(context.Background(), zap.NewNop(), caPath, "", "keploy-mitm-x"); err != nil {
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
