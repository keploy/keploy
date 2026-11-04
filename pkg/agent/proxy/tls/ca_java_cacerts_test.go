package tls

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

// fakeJDK makes a JDK home at dir/name whose bin/keytool appends its argv to
// dir/name.log, exits 1 for -list (so an import runs) and 0 otherwise. cacerts
// is the keystore's path under the home ("" for none): "lib/security/cacerts"
// as JDK 9+ and a JRE keep it, "jre/lib/security/cacerts" as JDK 8 does.
func fakeJDK(t *testing.T, dir, name, cacerts string) (home, log string) {
	t.Helper()
	return fakeJDKListing(t, dir, name, cacerts, "")
}

// fakeJDKListing is fakeJDK whose keystore holds the entries listing lists,
// as `keytool -list` prints them: its keytool prints listing for a -list of
// the whole keystore, and a -list of one alias exits 0 only when listing has
// that alias's line. With listing "" every -list exits 1, as fakeJDK's. A
// -delete of an alias the file dir/name.delfail names (one a line) prints
// "keytool error: injected for <alias>" and exits 1.
// The script uses only shell builtins: the tests' PATH has no cat or grep.
func fakeJDKListing(t *testing.T, dir, name, cacerts, listing string) (home, log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake keytool is a /bin/sh script")
	}
	home = filepath.Join(dir, name)
	log = filepath.Join(dir, name+".log")
	list := filepath.Join(dir, name+".list")
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if listing != "" {
		if err := os.WriteFile(list, []byte(listing), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The paths are quoted: a subtest's name, and so its temp dir, may hold
	// characters the shell reads as syntax.
	q := func(p string) string { return "'" + p + "'" }
	delfail := filepath.Join(dir, name+".delfail")
	script := "#!/bin/sh\necho \"$*\" >> " + q(log) + "\n" +
		"if [ \"$1\" = -delete ] && [ -f " + q(delfail) + " ]; then\n" +
		"  while IFS= read -r x; do [ \"$x\" = \"$3\" ] && { echo \"keytool error: injected for $3\"; exit 1; }; done < " + q(delfail) + "\nfi\n" +
		"[ \"$1\" = -list ] || exit 0\n" +
		"[ -f " + q(list) + " ] || exit 1\n" +
		"a=; prev=; for x in \"$@\"; do [ \"$prev\" = -alias ] && a=$x; prev=$x; done\n" +
		"while IFS= read -r l; do\n" +
		"  if [ -z \"$a\" ]; then printf '%s\\n' \"$l\"; else case \"$l\" in \"$a\",*) exit 0 ;; esac; fi\n" +
		"done < " + q(list) + "\n[ -z \"$a\" ]\n"
	if err := os.WriteFile(filepath.Join(home, "bin", "keytool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if cacerts != "" {
		p := filepath.Join(home, filepath.FromSlash(cacerts))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("keystore"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home, log
}

// keytoolCalls returns the argv lines a fake keytool logged (nil: never run).
func keytoolCalls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestJdkCacertsFindsEachLayout(t *testing.T) {
	cases := []struct {
		name  string
		files []string // keystores present under the home
		want  string   // "" = none found
	}{
		{"JDK 9+ or a JRE", []string{"lib/security/cacerts"}, "lib/security/cacerts"},
		{"JDK 8", []string{"jre/lib/security/cacerts"}, "jre/lib/security/cacerts"},
		{"both: the home's own first", []string{"lib/security/cacerts", "jre/lib/security/cacerts"}, "lib/security/cacerts"},
		{"neither", nil, ""},
		{"a directory named cacerts is not a keystore", []string{"lib/security/cacerts/"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			for _, f := range c.files {
				p := filepath.Join(home, filepath.FromSlash(strings.TrimSuffix(f, "/")))
				if strings.HasSuffix(f, "/") {
					if err := os.MkdirAll(p, 0o755); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, ok := jdkCacerts(home)
			if c.want == "" {
				if ok || got != "" {
					t.Fatalf("jdkCacerts = %q, %v; want none", got, ok)
				}
				return
			}
			if want := filepath.Join(home, filepath.FromSlash(c.want)); !ok || got != want {
				t.Fatalf("jdkCacerts = %q, %v; want %q", got, ok, want)
			}
		})
	}
}

// fakeNativeLinux sets up a native Linux install for a test: n temporary CA
// store directories (returned in caStorePath's order), a store update command
// that does nothing, and an environment whose JAVA_HOME and PATH name the JDK
// at pathJDK (given a java, so Java counts as installed). It restores the
// package state when the test ends.
func fakeNativeLinux(t *testing.T, n int, pathJDK string) []string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the trust-store anchors are Linux's")
	}
	if err := os.WriteFile(filepath.Join(pathJDK, "bin", "java"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "keploy-test-update-ca"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(pathJDK, "bin")+string(os.PathListSeparator)+bin)
	t.Setenv("JAVA_HOME", pathJDK)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SUDO_USER", "")
	for _, k := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"} {
		t.Setenv(k, "")
	}
	stores := make([]string, n)
	paths := make([]string, n)
	for i := range stores {
		stores[i] = t.TempDir()
		paths[i] = stores[i] + "/"
	}
	oldPaths, oldCmds := caStorePath, caStoreUpdateCmd
	caStorePath, caStoreUpdateCmd = paths, []string{"keploy-test-update-ca"}
	t.Cleanup(func() {
		caStorePath, caStoreUpdateCmd = oldPaths, oldCmds
		recordNativeInstall(nil)
		ResetCAReadyForTest()
	})
	return stores
}

// withPrefix is the calls that start with prefix.
func withPrefix(calls []string, prefix string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// TestNativeInstallImportsIntoTheAppJDK8KeystoreAndTeardownDeletesIt runs the
// native Linux install end to end (two temporary trust stores, a fake update
// command) for an app whose JDK 8 home keeps cacerts in jre/lib/security, on a
// host whose JAVA_HOME and PATH name another JDK: the install imports this
// run's certificate once (not once per store) into the app JDK's keystore, and
// teardown deletes the alias from that same file. The other JDK's keytool
// never runs.
func TestNativeInstallImportsIntoTheAppJDK8KeystoreAndTeardownDeletesIt(t *testing.T) {
	dir := t.TempDir()
	app, appLog := fakeJDK(t, dir, "app-jdk8", "jre/lib/security/cacerts")
	other, otherLog := fakeJDK(t, dir, "other-jdk", "lib/security/cacerts")
	stores := fakeNativeLinux(t, 2, other)

	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	if err := setupNativeForApp(context.Background(), zap.NewNop(), ca, nil, 0, app); err != nil {
		t.Fatal(err)
	}
	keystore := filepath.Join(app, "jre", "lib", "security", "cacerts")
	// The certificate is the last store's copy: every store holds the same one.
	imp := "-import -trustcacerts -keystore " + keystore + " -storepass changeit -noprompt -alias " + runAlias(ca) + " -file " + filepath.Join(stores[1], runCertFileName(ca))
	installCalls := keytoolCalls(t, appLog)
	if imports := withPrefix(installCalls, "-import "); len(imports) != 1 || imports[0] != imp {
		t.Fatalf("install's keytool -import calls: %q, want exactly [%q]", imports, imp)
	}

	TeardownNativeCA(zap.NewNop())
	del := "-delete -alias " + runAlias(ca) + " -keystore " + keystore + " -storepass changeit -noprompt"
	if got := keytoolCalls(t, appLog); len(got) != len(installCalls)+1 || got[len(got)-1] != del {
		t.Fatalf("teardown's keytool calls: %q, want one more after the install's, %q", got[len(installCalls):], del)
	}
	if got := keytoolCalls(t, otherLog); got != nil {
		t.Fatalf("the JDK that JAVA_HOME and PATH name ran keytool: %q", got)
	}
}

// TestNativeInstallSweepsEarlierRunsJDKAliases: the app JDK's keystore holds
// aliases of earlier runs that crashed (their teardown never ran) and aliases
// that are not keploy's, and in one case this run's own alias too (an earlier
// install in this run imported it). The native Linux install deletes the
// crashed runs' aliases, at most javaSweepMax of them, and no other alias: this
// run's stays, imported once. One keytool -list both finds the leftovers and
// says whether this run's alias is there.
func TestNativeInstallSweepsEarlierRunsJDKAliases(t *testing.T) {
	entry := func(alias string) string {
		return alias + ", Nov 3, 2026, trustedCertEntry,\nCertificate fingerprint (SHA-256): 0A:1B:2C\n"
	}
	crashed := func(i int) string { return fmt.Sprintf("keploy-mitm-0123456789abcd%02d", i) }
	cases := []struct {
		name          string
		crashed       int  // crashed runs' aliases in the keystore
		thisRunListed bool // this run's alias is in it too
		deletes       int
		imports       int
		remaining     int // -1: no "remaining" line
	}{
		{"a new run", 1, false, 1, 1, -1},
		{"this run's alias already there", 1, true, 1, 0, -1},
		{"a backlog of crashed runs", javaSweepMax + 7, false, javaSweepMax, 1, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ca, err := generateRunCA()
			if err != nil {
				t.Fatal(err)
			}
			listing := "Keystore type: PKCS12\nKeystore provider: SUN\n\nYour keystore contains some entries\n\n" +
				entry("digicertglobalrootca [jdk]") + entry("mycorp-root")
			for i := 0; i < c.crashed; i++ {
				if crashed(i) == runAlias(ca) {
					t.Fatal("a crashed run's alias is this run's")
				}
				listing += entry(crashed(i))
			}
			if c.thisRunListed {
				listing += entry(runAlias(ca))
			}
			dir := t.TempDir()
			app, appLog := fakeJDKListing(t, dir, "app-jdk", "lib/security/cacerts", listing)
			other, _ := fakeJDK(t, dir, "other-jdk", "lib/security/cacerts")
			fakeNativeLinux(t, 1, other)

			core, logs := observer.New(zapcore.InfoLevel)
			if err := setupNativeForApp(context.Background(), zap.New(core), ca, nil, 0, app); err != nil {
				t.Fatal(err)
			}
			keystore := filepath.Join(app, "lib", "security", "cacerts")
			calls := keytoolCalls(t, appLog)
			var want []string
			for i := 0; i < c.deletes; i++ {
				want = append(want, "-delete -alias "+crashed(i)+" -keystore "+keystore+" -storepass changeit -noprompt")
			}
			if dels := withPrefix(calls, "-delete "); strings.Join(dels, "|") != strings.Join(want, "|") {
				t.Fatalf("install's keytool -delete calls: %q, want %q", dels, want)
			}
			if imports := withPrefix(calls, "-import "); len(imports) != c.imports {
				t.Fatalf("install's keytool -import calls: %q, want %d", imports, c.imports)
			}
			if lists := withPrefix(calls, "-list "); len(lists) != 1 {
				t.Fatalf("install's keytool -list calls: %q, want the one listing", lists)
			}
			checkRemaining(t, logs, c.deletes, c.remaining, "")
		})
	}
}

// checkRemaining checks the sweep's line about the aliases it left: none when
// remaining is -1, else one saying it removed removed and left remaining, and
// naming a failed delete only when failedAlias is not "": that alias, with
// keytool's output.
func checkRemaining(t *testing.T, logs *observer.ObservedLogs, removed, remaining int, failedAlias string) {
	t.Helper()
	lines := logs.FilterMessage("the JDK keystore holds CA aliases that other keploy runs left there").All()
	if remaining < 0 {
		if len(lines) != 0 {
			t.Fatalf("logged %d lines about aliases left, want none", len(lines))
		}
		return
	}
	if len(lines) != 1 {
		t.Fatalf("logged %d lines about aliases left, want one", len(lines))
	}
	f := lines[0].ContextMap()
	if f["removed"] != int64(removed) || f["remaining"] != int64(remaining) {
		t.Fatalf("the line about aliases left says %v, want removed=%d remaining=%d", f, removed, remaining)
	}
	got, named := f["deleteFailed"]
	if failedAlias == "" {
		if named {
			t.Fatalf("the line about aliases left names a failed delete: %v", f)
		}
		return
	}
	if msg, _ := got.(string); !strings.Contains(msg, "-alias "+failedAlias+":") || !strings.Contains(msg, "keytool error: injected for "+failedAlias) {
		t.Fatalf("the line about aliases left names the failed delete as %q, want its alias %s and keytool's output", msg, failedAlias)
	}
}

// A delete that fails leaves its alias, and the sweep stops there (the
// keystore is most likely not writable) and says what is left and which
// delete failed.
func TestSweepJavaLeftoversStopsAtAFailedDelete(t *testing.T) {
	three := []string{"keploy-mitm-aaaa", "keploy-mitm-bbbb", "keploy-mitm-cccc"}
	cases := []struct {
		name      string
		leftovers []string
		fail      string   // the alias whose delete fails
		deletes   []string // the deletes run, in order
		removed   int
		remaining int
	}{
		{"the first fails (a keystore keploy cannot write)", three, "keploy-mitm-aaaa", three[:1], 0, 3},
		{"one after it", three, "keploy-mitm-bbbb", three[:2], 1, 2},
		{"none after it", three[:2], "keploy-mitm-bbbb", three[:2], 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			listing := ""
			for _, a := range c.leftovers {
				listing += a + ", Nov 3, 2026, trustedCertEntry,\n"
			}
			home, log := fakeJDKListing(t, dir, "jdk", "lib/security/cacerts", listing)
			if err := os.WriteFile(filepath.Join(dir, "jdk.delfail"), []byte(c.fail+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			ks := javaKeystore{keytool: filepath.Join(home, "bin", "keytool"), cacerts: filepath.Join(home, "lib", "security", "cacerts")}
			core, logs := observer.New(zapcore.InfoLevel)
			present, listed := sweepJavaLeftovers(context.Background(), zap.New(core), ks, "keploy-mitm-cafe")
			if present || !listed {
				t.Fatalf("sweepJavaLeftovers = present %v, listed %v; want false, true", present, listed)
			}
			var want []string
			for _, a := range c.deletes {
				want = append(want, "-delete -alias "+a+" -keystore "+ks.cacerts+" -storepass changeit -noprompt")
			}
			if dels := withPrefix(keytoolCalls(t, log), "-delete "); strings.Join(dels, "|") != strings.Join(want, "|") {
				t.Fatalf("keytool -delete calls: %q, want %q", dels, want)
			}
			checkRemaining(t, logs, c.removed, c.remaining, c.fail)
		})
	}
}

func TestJavaLeftovers(t *testing.T) {
	// JDK 8 keytool, a JKS keystore; Windows line ends.
	jks := "Keystore type: jks\r\nKeystore provider: SUN\r\n\r\nYour keystore contains 3 entries\r\n\r\n" +
		"keploy-mitm-aaaa, Nov 3, 2026, trustedCertEntry, \r\nCertificate fingerprint (SHA1): 0A:1B\r\n" +
		"keploy-mitm-cafe, Nov 4, 2026, trustedCertEntry, \r\nCertificate fingerprint (SHA1): 2C:3D\r\n" +
		"keploy-mitm-bbbb, Nov 3, 2026, trustedCertEntry, \r\nCertificate fingerprint (SHA1): 4E:5F\r\n"
	cases := []struct {
		name    string
		list    string
		alias   string
		present bool
		want    []string
	}{
		{"every other run's, in order", jks, "keploy-mitm-cafe", true, []string{"keploy-mitm-aaaa", "keploy-mitm-bbbb"}},
		{"this run's alias in another case is still this run's", jks, "keploy-mitm-CAFE", true, []string{"keploy-mitm-aaaa", "keploy-mitm-bbbb"}},
		{"this run's alias absent", jks, "keploy-mitm-dddd", false, []string{"keploy-mitm-aaaa", "keploy-mitm-cafe", "keploy-mitm-bbbb"}},
		{"Japanese keytool: no space after the comma", "keploy-mitm-aaaa,2026/11/03,trustedCertEntry,\nkeploy-mitm-cafe,2026/11/04,trustedCertEntry,\n", "keploy-mitm-cafe", true, []string{"keploy-mitm-aaaa"}},
		{"not keploy's", "mycorp-keploy-mitm-x, Nov 3, 2026, trustedCertEntry,\nkeploy-mitm, Nov 3, 2026, trustedCertEntry,\nKeploy MITM CA, Nov 3, 2026, trustedCertEntry,\n", "keploy-mitm-cafe", false, nil},
		{"empty keystore", "Keystore type: PKCS12\n\nYour keystore contains 0 entries\n", "keploy-mitm-cafe", false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			present, got := javaLeftovers([]byte(c.list), c.alias)
			if present != c.present || strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Fatalf("javaLeftovers = %v, %q; want %v, %q", present, got, c.present, c.want)
			}
		})
	}
}

// A keystore that cannot be listed is left as it is (no delete runs), and
// keytool is asked for this run's alias alone: the import runs only when
// that says it is not there.
func TestImportJavaCAWhenTheKeystoreCannotBeListed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake keytool is a /bin/sh script")
	}
	for _, aliasThere := range []bool{false, true} {
		t.Run(map[bool]string{false: "alias absent", true: "alias there"}[aliasThere], func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "jdk")
			log := filepath.Join(dir, "jdk.log")
			if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			aliasRC := map[bool]string{false: "1", true: "0"}[aliasThere]
			// A -list of the whole keystore fails; a -list of one alias exits aliasRC.
			script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n[ \"$1\" = -list ] || exit 0\n" +
				"case \" $* \" in *\" -alias \"*) exit " + aliasRC + " ;; esac\nexit 1\n"
			if err := os.WriteFile(filepath.Join(home, "bin", "keytool"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			ks := javaKeystore{keytool: filepath.Join(home, "bin", "keytool"), cacerts: filepath.Join(home, "lib", "security", "cacerts")}
			if err := importJavaCA(context.Background(), zap.NewNop(), ks, "/ca.crt", "keploy-mitm-r1"); err != nil {
				t.Fatal(err)
			}
			calls := keytoolCalls(t, log)
			if dels := withPrefix(calls, "-delete "); dels != nil {
				t.Fatalf("deleted from a keystore it could not list: %q", dels)
			}
			wantImports := 1
			if aliasThere {
				wantImports = 0
			}
			if imports := withPrefix(calls, "-import "); len(imports) != wantImports {
				t.Fatalf("keytool calls %q, want %d -import", calls, wantImports)
			}
		})
	}
}

// With no keystore under the home, resolveJavaKeystore resolves none (so no
// keytool runs: it would only fail, or create a stray keystore) and says
// where it looked.
func TestResolveJavaKeystoreWithoutCacerts(t *testing.T) {
	dir := t.TempDir()
	home, _ := fakeJDK(t, dir, "nokeystore", "")
	ks, err := resolveJavaKeystore(context.Background(), zaptest.NewLogger(t), home)
	if ks != (javaKeystore{}) || err == nil || !strings.Contains(err.Error(), "jre/lib/security") {
		t.Fatalf("resolveJavaKeystore = %+v, %v; want no keystore and an error naming where it looked", ks, err)
	}
}

// The run's alias is deleted from the keystore it was imported into, not from
// the one this process's JAVA_HOME or PATH names (another JDK on the host).
func TestTeardownDeletesTheAliasFromTheImportedKeystore(t *testing.T) {
	dir := t.TempDir()
	app, appLog := fakeJDK(t, dir, "app-jdk8", "jre/lib/security/cacerts")
	other, otherLog := fakeJDK(t, dir, "other-jdk", "lib/security/cacerts")
	t.Setenv("JAVA_HOME", other)
	t.Setenv("PATH", filepath.Join(other, "bin"))

	ks, err := resolveJavaKeystore(context.Background(), zap.NewNop(), app)
	if err != nil {
		t.Fatal(err)
	}
	recordNativeInstall(&nativeInstall{alias: "keploy-mitm-r1", java: ks, remove: true})
	TeardownNativeCA(zap.NewNop())

	want := "-delete -alias keploy-mitm-r1 -keystore " + filepath.Join(app, "jre", "lib", "security", "cacerts") + " -storepass changeit -noprompt"
	if got := keytoolCalls(t, appLog); len(got) != 1 || got[0] != want {
		t.Fatalf("app JDK keytool calls %q, want [%q]", got, want)
	}
	if got := keytoolCalls(t, otherLog); got != nil {
		t.Fatalf("teardown ran the other JDK's keytool: %q", got)
	}

	// Nothing imported (no keystore was resolved): nothing to delete, and no
	// failed delete to log.
	core, logs := observer.New(zapcore.DebugLevel)
	recordNativeInstall(&nativeInstall{alias: "keploy-mitm-r2", remove: true})
	TeardownNativeCA(zap.New(core))
	if got := keytoolCalls(t, otherLog); got != nil {
		t.Fatalf("teardown ran a keytool for an alias it never imported: %q", got)
	}
	if n := logs.FilterMessage("could not delete this run's JDK alias").Len(); n != 0 {
		t.Fatalf("teardown tried to delete an alias it never imported (%d failed deletes logged)", n)
	}
	if got := keytoolCalls(t, appLog); len(got) != 1 {
		t.Fatalf("teardown ran the app JDK's keytool again: %q", got)
	}
}

func TestImportJavaCAOrWarn(t *testing.T) {
	dir := t.TempDir()
	home, log := fakeJDK(t, dir, "jdk", "lib/security/cacerts")
	ks := javaKeystore{keytool: filepath.Join(home, "bin", "keytool"), cacerts: filepath.Join(home, "lib", "security", "cacerts")}
	const warn = "could not import the keploy CA into the Java keystore; Java uses the JAVA_TOOL_OPTIONS truststore instead"
	run := func(ks javaKeystore, resolveErr error) (warned bool, calls []string) {
		_ = os.Remove(log)
		core, logs := observer.New(zapcore.WarnLevel)
		importJavaCAOrWarn(context.Background(), zap.New(core), ks, resolveErr, "/ca.crt", "keploy-mitm-r1")
		return logs.FilterMessage(warn).Len() == 1, keytoolCalls(t, log)
	}

	if warned, calls := run(ks, nil); warned || len(withPrefix(calls, "-import ")) != 1 {
		t.Fatalf("resolved keystore: warned=%v calls=%q, want an import and no warning", warned, calls)
	}
	if warned, calls := run(javaKeystore{}, nil); warned || calls != nil {
		t.Fatalf("no Java: warned=%v calls=%q, want nothing", warned, calls)
	}
	if warned, calls := run(javaKeystore{}, errors.New("no cacerts keystore")); !warned || calls != nil {
		t.Fatalf("unresolved keystore: warned=%v calls=%q, want a warning and no keytool", warned, calls)
	}
	failing := filepath.Join(dir, "failing-keytool")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if warned, _ := run(javaKeystore{keytool: failing, cacerts: ks.cacerts}, nil); !warned {
		t.Fatal("a failed import did not warn")
	}
}

// The retired-alias cleanup finds a JDK 8's keystore from its JAVA_HOME too.
func TestResolveKeytoolAndCacertsFindsJDK8(t *testing.T) {
	dir := t.TempDir()
	home, _ := fakeJDK(t, dir, "jdk8", "jre/lib/security/cacerts")
	t.Setenv("JAVA_HOME", home)
	t.Setenv("PATH", filepath.Join(dir, "empty"))
	kt, cc, ok := resolveKeytoolAndCacerts()
	if !ok || kt != filepath.Join(home, "bin", "keytool") || cc != filepath.Join(home, "jre", "lib", "security", "cacerts") {
		t.Fatalf("resolveKeytoolAndCacerts = %q, %q, %v", kt, cc, ok)
	}

	// A JAVA_HOME with a keystore but no keytool, or a keytool but no
	// keystore, is not used (and PATH has none here).
	noKeytool := filepath.Join(dir, "jre-no-keytool")
	if err := os.MkdirAll(filepath.Join(noKeytool, "jre", "lib", "security"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noKeytool, "jre", "lib", "security", "cacerts"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	noKeystore, _ := fakeJDK(t, dir, "no-keystore", "")
	for _, jh := range []string{noKeytool, noKeystore} {
		t.Setenv("JAVA_HOME", jh)
		if kt, cc, ok := resolveKeytoolAndCacerts(); ok {
			t.Fatalf("JAVA_HOME=%s: resolveKeytoolAndCacerts = %q, %q, true; want none", jh, kt, cc)
		}
	}

	// From PATH: a keytool whose home has no keystore is not used.
	t.Setenv("JAVA_HOME", "")
	t.Setenv("PATH", filepath.Join(noKeystore, "bin"))
	if kt, cc, ok := resolveKeytoolAndCacerts(); ok {
		t.Fatalf("from PATH, no keystore: resolveKeytoolAndCacerts = %q, %q, true; want none", kt, cc)
	}

	// From PATH: <home>/bin/keytool, the home's jre keystore.
	t.Setenv("JAVA_HOME", "")
	t.Setenv("PATH", filepath.Join(home, "bin"))
	kt, cc, ok = resolveKeytoolAndCacerts()
	if !ok || kt != filepath.Join(home, "bin", "keytool") || cc != filepath.Join(home, "jre", "lib", "security", "cacerts") {
		t.Fatalf("from PATH: resolveKeytoolAndCacerts = %q, %q, %v", kt, cc, ok)
	}
}
