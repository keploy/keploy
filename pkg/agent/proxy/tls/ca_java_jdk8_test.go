package tls

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// The import went to <home>/lib/security/cacerts whatever JDK it was given. A
// JDK 8 root (the JAVA_HOME an app's environment names) keeps its keystore
// in jre/lib/security and has no lib/security, so keytool failed on every
// JDK 8 run and the agent logged the failure as an error. The keystore is
// now looked up in lib/security, then in jre/lib/security; with neither, no
// keytool runs and the install says why.
func TestInstallJavaCAForHomeUsesTheKeystoreTheJDKHas(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake JDK's tools are /bin/sh scripts")
	}
	for _, tc := range []struct {
		name string
		// lay builds the JDK at home and returns its keystore, "" for none.
		lay func(t *testing.T, home, calls string) string
	}{
		{"a JDK 9 or later", func(t *testing.T, home, calls string) string {
			fakeJDK(t, home, ".", "/not/read", calls)
			return filepath.Join(home, "lib", "security", "cacerts")
		}},
		{"a JDK 8 root, as an app's JAVA_HOME names it", func(t *testing.T, home, calls string) string {
			fakeJDK(t, home, "jre", "/not/read", calls)
			return filepath.Join(home, "jre", "lib", "security", "cacerts")
		}},
		{"a JDK 9 or later with a stray jre/ beside its lib/", func(t *testing.T, home, calls string) string {
			fakeJDK(t, home, ".", "/not/read", calls)
			stray := filepath.Join(home, "jre", "lib", "security", "cacerts")
			if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stray, []byte("cacerts"), 0o644); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(home, "lib", "security", "cacerts")
		}},
		{"a JDK with no keystore at all", func(t *testing.T, home, calls string) string {
			fakeJDK(t, home, "", "/not/read", calls)
			return ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			home, calls := filepath.Join(dir, "jdk"), filepath.Join(dir, "calls")
			want := tc.lay(t, home, calls)
			caPath := filepath.Join(dir, "ca.crt")
			if err := os.WriteFile(caPath, []byte("ca"), 0o644); err != nil {
				t.Fatal(err)
			}
			jdk, err := installJavaCAForHome(context.Background(), zap.NewNop(), caPath, home, "keploy-mitm-run")
			got := readCalls(t, calls)
			if want == "" {
				if err == nil || !strings.Contains(err.Error(), "jre/lib/security") {
					t.Errorf("err = %v, want one that says where it looked for the keystore", err)
				}
				if jdk != (javaInstall{}) {
					t.Errorf("install = %+v, want none: there is no keystore to delete from at the teardown", jdk)
				}
				if len(got) != 0 {
					t.Errorf("keytool ran with no keystore to write to: %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("installJavaCAForHome: %v", err)
			}
			wantInstall := javaInstall{keytool: filepath.Join(home, "bin", "keytool"), keystore: want, imported: true}
			if jdk != wantInstall {
				t.Errorf("install = %+v, want %+v", jdk, wantInstall)
			}
			if len(got) != 2 || !strings.HasPrefix(got[0], "keytool -list ") || !strings.HasPrefix(got[1], "keytool -import ") {
				t.Fatalf("keytool runs = %q, want its -list then its -import", got)
			}
			for _, c := range got {
				if !strings.Contains(c, " -keystore "+want+" ") {
					t.Errorf("keytool run %q is not on the keystore %s", c, want)
				}
			}
		})
	}
}

// resolveKeytoolAndCacerts (the JDK the retired alias's removal and the
// teardown's Debian keystore pass use when the install found none) looked
// only in <home>/lib/security, so a JDK 8 named by JAVA_HOME or found on PATH
// was no JDK to it. It also took the directory above a PATH keytool's own
// bin/ for its JDK, so a keytool that links into its JDK, as the
// alternatives' /usr/bin/keytool does, found no keystore.
func TestResolveKeytoolAndCacertsFindsAJDK8Keystore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake JDK's tools are /bin/sh scripts")
	}
	for _, tc := range []struct {
		name string
		// lay builds what JAVA_HOME and PATH name and returns them and the
		// keytool and keystore wanted ("" for none).
		lay func(t *testing.T, dir string) (javaHome, path, keytool, cacerts string)
	}{
		{"a JDK 8 that JAVA_HOME names", func(t *testing.T, dir string) (string, string, string, string) {
			jdk := filepath.Join(dir, "jdk8")
			fakeJDK(t, jdk, "jre", "/not/read", filepath.Join(dir, "calls"))
			return jdk, filepath.Join(dir, "empty"), filepath.Join(jdk, "bin", "keytool"), filepath.Join(jdk, "jre", "lib", "security", "cacerts")
		}},
		{"a JDK 8 on PATH", func(t *testing.T, dir string) (string, string, string, string) {
			jdk := filepath.Join(dir, "jdk8")
			fakeJDK(t, jdk, "jre", "/not/read", filepath.Join(dir, "calls"))
			return "", filepath.Join(jdk, "bin"), filepath.Join(jdk, "bin", "keytool"), filepath.Join(jdk, "jre", "lib", "security", "cacerts")
		}},
		{"a JDK 9 or later on PATH", func(t *testing.T, dir string) (string, string, string, string) {
			jdk := filepath.Join(dir, "jdk-21")
			fakeJDK(t, jdk, ".", "/not/read", filepath.Join(dir, "calls"))
			return "", filepath.Join(jdk, "bin"), filepath.Join(jdk, "bin", "keytool"), filepath.Join(jdk, "lib", "security", "cacerts")
		}},
		{"JAVA_HOME names a JDK with no keystore, PATH none", func(t *testing.T, dir string) (string, string, string, string) {
			jdk := filepath.Join(dir, "jdk")
			fakeJDK(t, jdk, "", "/not/read", filepath.Join(dir, "calls"))
			return jdk, filepath.Join(dir, "empty"), "", ""
		}},
		{"JAVA_HOME names a keystore with no keytool, PATH none", func(t *testing.T, dir string) (string, string, string, string) {
			jre := filepath.Join(dir, "jre")
			cacerts := filepath.Join(jre, "lib", "security", "cacerts")
			if err := os.MkdirAll(filepath.Dir(cacerts), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cacerts, []byte("cacerts"), 0o644); err != nil {
				t.Fatal(err)
			}
			return jre, filepath.Join(dir, "empty"), "", ""
		}},
		{"a keytool on PATH that links, through another link, into a JDK", func(t *testing.T, dir string) (string, string, string, string) {
			jdk := filepath.Join(dir, "jdk-21")
			fakeJDK(t, jdk, ".", "/not/read", filepath.Join(dir, "calls"))
			bin, alt := filepath.Join(dir, "usr", "bin"), filepath.Join(dir, "alternatives")
			for _, d := range []string{bin, alt} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(jdk, "bin", "keytool"), filepath.Join(alt, "keytool")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(alt, "keytool"), filepath.Join(bin, "keytool")); err != nil {
				t.Fatal(err)
			}
			return "", bin, filepath.Join(jdk, "bin", "keytool"), filepath.Join(jdk, "lib", "security", "cacerts")
		}},
		{"a keytool on PATH with no keystore above it", func(t *testing.T, dir string) (string, string, string, string) {
			jdk := filepath.Join(dir, "wrapper")
			fakeJDK(t, jdk, "", "/not/read", filepath.Join(dir, "calls"))
			return "", filepath.Join(jdk, "bin"), "", ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The keytool found is named by its real path, so the wanted ones
			// are too (macOS's temp dir is under a link).
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			javaHome, path, wantKT, wantCC := tc.lay(t, dir)
			t.Setenv("JAVA_HOME", javaHome)
			t.Setenv("PATH", path)
			kt, cc, ok := resolveKeytoolAndCacerts()
			if ok != (wantKT != "") || kt != wantKT || cc != wantCC {
				t.Errorf("resolveKeytoolAndCacerts() = (%q, %q, %v), want (%q, %q, %v)", kt, cc, ok, wantKT, wantCC, wantKT != "")
			}
		})
	}
}
