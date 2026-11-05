package tls

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/pavlo-v-chernykh/keystore-go/v4"
	"go.uber.org/zap"
)

// envFakeKeytoolCalls names the file the fake keytool logs its runs to;
// with envFakeKeytoolFails set, its -delete fails, deleting nothing.
const (
	envFakeKeytoolCalls = "KEPLOY_TEST_FAKE_KEYTOOL_CALLS"
	envFakeKeytoolFails = "KEPLOY_TEST_FAKE_KEYTOOL_FAILS"
)

// TestHelperFakeKeytool is no test: it is the fake keytool of the rows
// below, which runs this test binary. It logs its run, and its -delete
// deletes the alias from a JKS keystore as keytool does: in Go, so the rows
// read the keystore it leaves.
func TestHelperFakeKeytool(t *testing.T) {
	calls := os.Getenv(envFakeKeytoolCalls)
	if calls == "" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	f, err := os.OpenFile(calls, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		os.Exit(2)
	}
	_, _ = f.WriteString("keytool " + strings.Join(args, " ") + "\n")
	_ = f.Close()
	var alias, path, pass string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-alias":
			alias = args[i+1]
		case "-keystore":
			path = args[i+1]
		case "-storepass":
			pass = args[i+1]
		}
	}
	if len(args) == 0 || args[0] != "-delete" || os.Getenv(envFakeKeytoolFails) != "" {
		os.Exit(1)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		os.Exit(1)
	}
	ks := keystore.New()
	if err := ks.Load(bytes.NewReader(b), []byte(pass)); err != nil || !ks.IsTrustedCertificateEntry(alias) {
		os.Exit(1)
	}
	ks.DeleteEntry(alias)
	var out bytes.Buffer
	if err := ks.Store(&out, []byte(pass)); err != nil {
		os.Exit(1)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// fakeDeletingKeytool writes, under dir, a keytool that runs
// TestHelperFakeKeytool, and returns it and the file it logs its runs to.
func fakeDeletingKeytool(t *testing.T, dir string) (keytool, calls string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	keytool, calls = filepath.Join(dir, "bin", "keytool"), filepath.Join(dir, "calls")
	writeScript(t, keytool, "exec '"+self+"' -test.run='^TestHelperFakeKeytool$' -- \"$@\"\n")
	t.Setenv(envFakeKeytoolCalls, calls)
	return keytool, calls
}

// keystoreAliases is the trusted entries of the JKS keystore at path, each
// alias with the name of its certificate in names.
func keystoreAliases(t *testing.T, path, pass string, names map[string]string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ks := keystore.New()
	if err := ks.Load(bytes.NewReader(b), []byte(pass)); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var got []string
	for _, alias := range ks.Aliases() {
		e, err := ks.GetTrustedCertificateEntry(alias)
		if err != nil {
			t.Fatalf("%s: %s: %v", path, alias, err)
		}
		got = append(got, alias+"="+names[string(e.Certificate.Content)])
	}
	sort.Strings(got)
	return got
}

// The system store's update on Debian and Ubuntu runs ca-certificates-java's
// hook, which imports the run's anchor into /etc/ssl/certs/java/cacerts (as
// debian:<anchor>.pem) and is not told when the teardown removes it: every
// native run left its CA there for good. The teardown deletes this run's CA
// from that keystore and from the keystore of the JDK the install found,
// under whatever alias holds it, matched by its certificate: an entry that
// holds another certificate stays, also one under this run's alias. Where
// Go cannot read the install's keystore, keytool deletes this run's alias
// by name, only when the install imported it. One keytool runs per entry
// deleted, once even when it fails, and none when no keystore holds the CA.
func TestTheTeardownDeletesThisRunsCAFromTheJDKKeystoresByItsCertificate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake keytool is a shell script")
	}
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	other, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{string(ca.cert.Raw): "ca", string(other.cert.Raw): "other"}
	run := runAlias(ca)
	hook := "debian:" + strings.TrimSuffix(runCertFileName(ca), ".crt") + ".pem"
	notJKS := []byte{0x30, 0x82, 0x01, 0x00, 0x02, 0x01, 0x03}

	type store struct {
		pass  string
		certs map[string][]byte // nil with raw unset: no keystore
		raw   []byte            // a keystore Go cannot read
	}
	for _, tc := range []struct {
		name        string
		jdk, debian store
		linked      bool // the JDK's cacerts links to Debian's keystore
		noJDK       bool // the install found no JDK; JAVA_HOME names one at the teardown
		cutShort    bool // the stop cut the install short before its JDK step; JAVA_HOME names one
		deleteFails bool
		unopened    bool // Go cannot open the JDK's keystore (its mode)
		imported    bool
		wantDeletes []string // "<keystore> <alias>", keystore "jdk" or "debian"
		wantJDK     []string
		wantDebian  []string
	}{
		{
			name:        "Debian's keystore keeps the hook's entry",
			jdk:         store{pass: "changeit", certs: map[string][]byte{"debian:other.pem": other.cert.Raw}},
			debian:      store{pass: "changeit", certs: map[string][]byte{hook: ca.cert.Raw, "debian:other.pem": other.cert.Raw}},
			wantDeletes: []string{"debian " + hook},
			wantJDK:     []string{"debian:other.pem=other"},
			wantDebian:  []string{"debian:other.pem=other"},
		},
		{
			name:        "the JDK's cacerts links to Debian's keystore, which holds the CA twice",
			debian:      store{pass: "changeit", certs: map[string][]byte{hook: ca.cert.Raw, run: ca.cert.Raw, "debian:other.pem": other.cert.Raw}},
			linked:      true,
			imported:    true,
			wantDeletes: []string{"jdk " + hook, "jdk " + run},
			wantDebian:  []string{"debian:other.pem=other"},
		},
		{
			name:        "this run's alias holds another certificate",
			jdk:         store{pass: "changeit", certs: map[string][]byte{run: other.cert.Raw}},
			imported:    true,
			wantJDK:     []string{run + "=other"},
			wantDeletes: nil,
		},
		{
			name:        "the install imported this run's alias",
			jdk:         store{pass: "changeit", certs: map[string][]byte{run: ca.cert.Raw, "debian:other.pem": other.cert.Raw}},
			imported:    true,
			wantDeletes: []string{"jdk " + run},
			wantJDK:     []string{"debian:other.pem=other"},
		},
		{
			name:        "no keystore holds the CA",
			jdk:         store{pass: "changeit", certs: map[string][]byte{"debian:other.pem": other.cert.Raw}},
			debian:      store{pass: "changeit", certs: map[string][]byte{"debian:other.pem": other.cert.Raw}},
			wantJDK:     []string{"debian:other.pem=other"},
			wantDebian:  []string{"debian:other.pem=other"},
			wantDeletes: nil,
		},
		{
			name:        "Go cannot read the JDK's keystore, where the install imported this run's alias",
			jdk:         store{raw: notJKS},
			imported:    true,
			wantDeletes: []string{"jdk " + run},
		},
		{
			name:        "Go cannot read the JDK's keystore, where the install imported nothing",
			jdk:         store{raw: notJKS},
			wantDeletes: nil,
		},
		{
			name:        "Debian's keystore under another password",
			debian:      store{pass: "secret", certs: map[string][]byte{hook: ca.cert.Raw}},
			imported:    true,
			wantDebian:  []string{hook + "=ca"},
			wantDeletes: nil,
		},
		{
			name:        "keytool fails to delete: one try for each entry",
			debian:      store{pass: "changeit", certs: map[string][]byte{hook: ca.cert.Raw, run: ca.cert.Raw, "debian:other.pem": other.cert.Raw}},
			linked:      true,
			imported:    true,
			deleteFails: true,
			wantDeletes: []string{"jdk " + hook, "jdk " + run},
			wantDebian:  []string{"debian:other.pem=other", hook + "=ca", run + "=ca"},
		},
		{
			name:        "Go cannot open the JDK's keystore, where the install imported this run's alias",
			jdk:         store{pass: "changeit", certs: map[string][]byte{run: ca.cert.Raw}},
			unopened:    true,
			imported:    true,
			wantDeletes: []string{"jdk " + run},
		},
		{
			name:        "the JDK's keystore is gone at the teardown",
			imported:    true,
			wantDeletes: nil,
		},
		{
			name:        "the stop cut the install short before its JDK step",
			jdk:         store{raw: notJKS},
			cutShort:    true,
			wantDeletes: nil,
		},
		{
			name:        "the install found no JDK: JAVA_HOME's keytool deletes the hook's entry",
			debian:      store{pass: "changeit", certs: map[string][]byte{hook: ca.cert.Raw, "debian:other.pem": other.cert.Raw}},
			noJDK:       true,
			wantDeletes: []string{"debian " + hook},
			wantDebian:  []string{"debian:other.pem=other"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unopened && os.Geteuid() == 0 {
				t.Skip("root opens a keystore of any mode")
			}
			dir := t.TempDir()
			keytool, calls := fakeDeletingKeytool(t, filepath.Join(dir, "jdk"))
			jdkStore := filepath.Join(dir, "jdk", "lib", "security", "cacerts")
			debianStore := filepath.Join(dir, "etc", "java", "cacerts")
			for _, p := range []string{jdkStore, debianStore} {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path string, s store) {
				switch {
				case s.raw != nil:
					if err := os.WriteFile(path, s.raw, 0o644); err != nil {
						t.Fatal(err)
					}
				case s.certs != nil:
					writeJKS(t, path, s.pass, s.certs)
				}
			}
			write(debianStore, tc.debian)
			if tc.linked {
				if err := os.Symlink(debianStore, jdkStore); err != nil {
					t.Fatal(err)
				}
			} else {
				write(jdkStore, tc.jdk)
			}
			if tc.unopened {
				if err := os.Chmod(jdkStore, 0); err != nil {
					t.Fatal(err)
				}
			}
			before := map[string][]byte{}
			for _, p := range []string{jdkStore, debianStore} {
				if b, err := os.ReadFile(p); err == nil {
					before[p] = b
				}
			}
			oldDebian := debianJavaKeystore
			debianJavaKeystore = debianStore
			t.Cleanup(func() {
				debianJavaKeystore = oldDebian
				recordNativeInstall(nil)
			})
			t.Setenv("PATH", filepath.Join(dir, "no-bin"))
			t.Setenv("JAVA_HOME", "")
			n := &nativeInstall{alias: run, remove: true, caDER: ca.cert.Raw, noJavaAlias: !tc.imported, keytool: keytool, keystore: jdkStore}
			if tc.noJDK {
				n.keytool, n.keystore = "", ""
				t.Setenv("JAVA_HOME", filepath.Join(dir, "jdk"))
				// resolveKeytoolAndCacerts wants a cacerts beside JAVA_HOME's keytool.
				writeJKS(t, jdkStore, "changeit", nil)
			}
			if tc.cutShort {
				// The install's note of its JDK step never came: the
				// record says nothing of a JDK, nor that none was imported.
				n.keytool, n.keystore, n.noJavaAlias = "", "", false
				t.Setenv("JAVA_HOME", filepath.Join(dir, "jdk"))
			}
			if tc.deleteFails {
				t.Setenv(envFakeKeytoolFails, "1")
			}
			recordNativeInstall(n)
			TeardownNativeCA(zap.NewNop())

			var want []string
			for _, d := range tc.wantDeletes {
				which, alias, _ := strings.Cut(d, " ")
				ks := jdkStore
				if which == "debian" {
					ks = debianStore
				}
				want = append(want, "keytool -delete -alias "+alias+" -keystore "+ks+" -storepass changeit -noprompt")
			}
			got := readCalls(t, calls)
			sort.Strings(got)
			sort.Strings(want)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("the teardown's keytool runs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			check := func(path string, s store, wantAliases []string) {
				switch {
				case s.raw != nil:
					if b, _ := os.ReadFile(path); !bytes.Equal(b, before[path]) {
						t.Errorf("%s: a keystore Go cannot read was rewritten", path)
					}
				case s.certs != nil:
					wantAliases = append([]string(nil), wantAliases...)
					sort.Strings(wantAliases)
					if got := keystoreAliases(t, path, s.pass, names); strings.Join(got, " ") != strings.Join(wantAliases, " ") {
						t.Errorf("%s after the teardown: %q, want %q", path, got, wantAliases)
					}
				}
			}
			check(debianStore, tc.debian, tc.wantDebian)
			if !tc.linked && !tc.noJDK && !tc.unopened {
				check(jdkStore, tc.jdk, tc.wantJDK)
			}
		})
	}
}
