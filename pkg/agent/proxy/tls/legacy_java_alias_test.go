package tls

import (
	"context"
	"crypto/sha256"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The retired CA's JDK keystore alias is read in Go, and is the agent's to
// remove once its proxy listens: the start runs no JVM for it.

// legacyFakeJDK lays out a JDK at JAVA_HOME whose keytool logs each run to
// the returned calls file, and whose keytool -exportcert prints exportPEM
// (exits 1 when it is empty). cacerts is the keystore's content.
func legacyFakeJDK(t *testing.T, cacerts []byte, exportPEM []byte) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	jdk := filepath.Join(dir, "jdk")
	export := "exit 1"
	if len(exportPEM) > 0 {
		pemPath := filepath.Join(dir, "export.pem")
		if err := os.WriteFile(pemPath, exportPEM, 0o644); err != nil {
			t.Fatal(err)
		}
		export = "/bin/cat " + pemPath
	}
	writeScript(t, filepath.Join(jdk, "bin", "keytool"),
		"echo \"keytool $*\" >> "+calls+"\n"+
			"case \"$1\" in -exportcert) "+export+";; esac\n")
	cc := filepath.Join(jdk, "lib", "security", "cacerts")
	if err := os.MkdirAll(filepath.Dir(cc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cc, cacerts, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JAVA_HOME", jdk)
	return calls
}

// jksBytes is a JKS keystore under pass trusting each certificate (DER)
// under its alias.
func jksBytes(t *testing.T, pass string, certs map[string][]byte) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ks.jks")
	writeJKS(t, p, pass, certs)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The retired alias's entry is read in Go from a JKS keystore: no alias, or
// one of another certificate, runs no keytool, and the retired CA's runs
// only keytool -delete. Before, every start that found a JDK ran keytool
// -exportcert, a JVM, to read the entry. A keystore Go cannot read (a JDK 18
// or later's PKCS12 cacerts, or a JKS under another password) still has
// keytool read it.
func TestTheRetiredJDKAliasIsReadInGoAndKeytoolOnlyDeletesIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake keytool is a shell script")
	}
	retired, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	other, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	fp := sha256.Sum256(retired.cert.Raw)
	retiredPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: retired.cert.Raw})
	otherPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: other.cert.Raw})
	const notRetired = "a 'keployCA' JDK alias exists but is not the retired CA; leaving it"
	for _, tc := range []struct {
		name       string
		cacerts    []byte
		export     []byte
		want       []string // keytool runs, by their first words
		notRetired bool     // says the alias is not the retired CA
		unopened   bool     // the keystore cannot be opened (not as root)
	}{
		{"a JKS with no such alias", jksBytes(t, "changeit", map[string][]byte{"debian:other.pem": other.cert.Raw}), retiredPEM, nil, false, false},
		{"a JKS whose alias is another CA", jksBytes(t, "changeit", map[string][]byte{legacyCAAlias: other.cert.Raw}), retiredPEM, nil, true, false},
		{"a JKS whose alias is the retired CA", jksBytes(t, "changeit", map[string][]byte{legacyCAAlias: retired.cert.Raw}), nil, []string{"keytool -delete -alias keployCA "}, false, false},
		{"a keystore Go cannot read, the retired CA's", []byte("PKCS12"), retiredPEM, []string{"keytool -exportcert ", "keytool -delete -alias keployCA "}, false, false},
		{"a keystore Go cannot read, another CA's", []byte("PKCS12"), otherPEM, []string{"keytool -exportcert "}, true, false},
		{"a keystore neither can read", []byte("PKCS12"), nil, []string{"keytool -exportcert "}, false, false},
		{"a JKS under another password", jksBytes(t, "secret", map[string][]byte{legacyCAAlias: retired.cert.Raw}), retiredPEM, []string{"keytool -exportcert ", "keytool -delete -alias keployCA "}, false, false},
		{"a JKS Go cannot open", jksBytes(t, "changeit", map[string][]byte{"debian:other.pem": other.cert.Raw}), retiredPEM, []string{"keytool -exportcert ", "keytool -delete -alias keployCA "}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unopened && os.Geteuid() == 0 {
				t.Skip("root opens a keystore of any mode")
			}
			calls := legacyFakeJDK(t, tc.cacerts, tc.export)
			if tc.unopened {
				if err := os.Chmod(filepath.Join(os.Getenv("JAVA_HOME"), "lib", "security", "cacerts"), 0); err != nil {
					t.Fatal(err)
				}
			}
			core, logs := observer.New(zapcore.DebugLevel)
			removeLegacyJavaAlias(context.Background(), zap.New(core), fp)
			got := readCalls(t, calls)
			if len(got) != len(tc.want) {
				t.Fatalf("keytool runs: %q, want %q", got, tc.want)
			}
			for i, w := range tc.want {
				if !strings.HasPrefix(got[i], w) {
					t.Fatalf("keytool runs: %q, want %q", got, tc.want)
				}
			}
			if said := logs.FilterMessage(notRetired).Len() > 0; said != tc.notRetired {
				t.Fatalf("said %q: %v, want %v", notRetired, said, tc.notRetired)
			}
		})
	}
}

// The start's sweep (SweepLegacyCA) runs no keytool, even for a keystore
// only keytool can read: the alias is SweepLegacyJavaAlias's, which the
// agent runs once its proxy listens, and `keploy ca clean`
// (SweepLegacyCAAggressive) runs too.
func TestTheStartsLegacySweepRunsNoJVM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake keytool is a shell script")
	}
	for _, tc := range []struct {
		name  string
		sweep func(context.Context, *zap.Logger, bool)
		want  []string
	}{
		{"the start's sweep", SweepLegacyCA, nil},
		{"keploy ca clean", SweepLegacyCAAggressive, []string{"keytool -exportcert "}},
		{"the sweep once the proxy listens", func(ctx context.Context, l *zap.Logger, _ bool) { SweepLegacyJavaAlias(ctx, l) }, []string{"keytool -exportcert "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := legacyFakeJDK(t, []byte("PKCS12"), nil)
			old := caStorePath
			caStorePath = []string{t.TempDir() + "/"}
			t.Cleanup(func() { caStorePath = old })
			t.Setenv("TMPDIR", t.TempDir())
			tc.sweep(context.Background(), zap.NewNop(), false)
			got := readCalls(t, calls)
			if len(got) != len(tc.want) {
				t.Fatalf("keytool runs: %q, want %q", got, tc.want)
			}
			for i, w := range tc.want {
				if !strings.HasPrefix(got[i], w) || !strings.Contains(got[i], " -alias keployCA ") {
					t.Fatalf("keytool runs: %q, want %q of the retired alias", got, tc.want)
				}
			}
		})
	}
}

// keytool rewrites the whole keystore, so keploy's writes to it run one at
// a time: the retired alias's removal, which runs beside the agent's start,
// waits for the run's import and teardown, and they for it.
func TestKeytoolRunsOnTheJDKKeystoreOneAtATime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake keytool is a shell script")
	}
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		run  func(ctx context.Context)
	}{
		{"the retired alias's removal", func(ctx context.Context) { removeLegacyJavaAlias(ctx, zap.NewNop(), sha256.Sum256(ca.cert.Raw)) }},
		{"the teardown's delete", func(ctx context.Context) { removeJavaAlias(ctx, zap.NewNop(), "keploy-run") }},
		{"the run's import", func(ctx context.Context) {
			_, _ = installJavaCAForHome(ctx, zap.NewNop(), caPath, os.Getenv("JAVA_HOME"), "keploy-run")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := legacyFakeJDK(t, []byte("PKCS12"), nil)
			jdkKeystoreMu.Lock()
			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.run(context.Background())
			}()
			select {
			case <-done:
				jdkKeystoreMu.Unlock()
				t.Fatalf("ran while another keytool held the keystore: %q", readCalls(t, calls))
			case <-time.After(300 * time.Millisecond):
			}
			if got := readCalls(t, calls); len(got) != 0 {
				jdkKeystoreMu.Unlock()
				<-done
				t.Fatalf("keytool ran while another held the keystore: %q", got)
			}
			jdkKeystoreMu.Unlock()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("did not run once the keystore was free")
			}
			if got := readCalls(t, calls); len(got) == 0 {
				t.Fatal("ran no keytool once the keystore was free")
			}
		})
	}
}
