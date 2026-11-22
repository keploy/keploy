package tls

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// fakeListingJDK makes a JDK home at home whose bin/keytool appends its argv
// to calls and whose keystore (lib/security/cacerts) holds the entries listing
// lists, as `keytool -list` prints them: a -list of the whole keystore prints
// listing, a -list of one alias exits 0 only when listing has that alias's
// line, and with unlisted set every -list exits 1. A -delete of an alias the
// file calls+".delfail" names (one a line) prints "keytool error: injected for
// <alias>" and exits 1; every other run exits 0. The script uses only shell
// builtins.
func fakeListingJDK(t *testing.T, home, listing, calls string, unlisted bool) (cacerts string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake keytool is a /bin/sh script")
	}
	list := calls + ".list"
	if err := os.WriteFile(list, []byte(listing), 0o644); err != nil {
		t.Fatal(err)
	}
	q := func(p string) string { return "'" + p + "'" }
	listFails := ""
	if unlisted {
		listFails = "exit 1\n"
	}
	writeScript(t, filepath.Join(home, "bin", "keytool"),
		"echo \"keytool $*\" >> "+q(calls)+"\n"+
			"if [ \"$1\" = -delete ] && [ -f "+q(calls+".delfail")+" ]; then\n"+
			"  while IFS= read -r x; do [ \"$x\" = \"$3\" ] && { echo \"keytool error: injected for $3\"; exit 1; }; done < "+q(calls+".delfail")+"\nfi\n"+
			"[ \"$1\" = -list ] || exit 0\n"+
			listFails+
			"a=; prev=; for x in \"$@\"; do [ \"$prev\" = -alias ] && a=$x; prev=$x; done\n"+
			"while IFS= read -r l; do\n"+
			"  if [ -z \"$a\" ]; then printf '%s\\n' \"$l\"; else case \"$l\" in \"$a\",*) exit 0 ;; esac; fi\n"+
			"done < "+q(list)+"\n[ -z \"$a\" ]\n")
	cacerts = filepath.Join(home, "lib", "security", "cacerts")
	if err := os.MkdirAll(filepath.Dir(cacerts), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacerts, []byte("keystore"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cacerts
}

// entry is a `keytool -list` line of alias, and the fingerprint line after it.
func entry(alias string) string {
	return alias + ", 20 Nov 2026, trustedCertEntry,\nCertificate fingerprint (SHA-256): 00:11:22\n"
}

// The import's start-up sweep removed only keploy-mitm-*.crt anchors of runs
// that are over, so a run whose teardown did not delete its JDK alias (it
// crashed, or its teardown deleted from another JDK's keystore) left that
// alias in the app's keystore for good: the JDK trusted a CA whose key no
// longer exists, one more for every such run. The install now lists the
// keystore once and removes up to javaSweepMax such aliases of runs that are
// over, a run's that is alive never, nor this run's, nor any alias that is not
// keploy's; it stops at the first delete that fails, and says how many are
// left.
func TestInstallJavaCAForHomeSweepsTheAliasesOfRunsThatAreOver(t *testing.T) {
	const self = "keploy-mitm-self"
	dead := []string{"keploy-mitm-dead1", "keploy-mitm-dead2", "keploy-mitm-dead3", "keploy-mitm-dead4", "keploy-mitm-dead5"}
	for _, tc := range []struct {
		name        string
		listing     string
		unlisted    bool     // every keytool -list fails
		delfail     string   // the alias whose -delete fails
		wantRuns    []string // each keytool run: "-list", "-list <alias>", "-delete <alias>", "-import <alias>"
		wantLeft    int      // the remaining count the install says, -1 for no such line
		wantRemoved int
	}{
		{
			name:     "five over, one alive, one not keploy's",
			listing:  "Keystore type: JKS\n\nYour keystore contains 8 entries\n\n" + entry("digicertroot") + entry(dead[0]) + entry("keploy-mitm-live") + entry(dead[1]) + entry(dead[2]) + entry(dead[3]) + entry(dead[4]),
			wantRuns: []string{"-list", "-delete " + dead[0], "-delete " + dead[1], "-delete " + dead[2], "-import " + self},
			wantLeft: 2, wantRemoved: 3,
		},
		{
			name:     "no more than the sweep removes at once",
			listing:  entry(dead[0]) + entry(dead[1]),
			wantRuns: []string{"-list", "-delete " + dead[0], "-delete " + dead[1], "-import " + self},
			wantLeft: -1,
		},
		{
			name:     "a delete that fails stops the sweep",
			listing:  entry(dead[0]) + entry(dead[1]) + entry(dead[2]),
			delfail:  dead[1],
			wantRuns: []string{"-list", "-delete " + dead[0], "-delete " + dead[1], "-import " + self},
			wantLeft: 2, wantRemoved: 1,
		},
		{
			name:     "this run's alias is there, in the keystore's lower case",
			listing:  entry(dead[0]) + entry(self),
			wantRuns: []string{"-list", "-delete " + dead[0]},
			wantLeft: -1,
		},
		{
			name:     "only keploy's own aliases and none of a live run",
			listing:  entry("keploy-mitm-live") + entry("mitm-keploy-x") + entry("keploy-ca"),
			wantRuns: []string{"-list", "-import " + self},
			wantLeft: -1,
		},
		{
			name:     "a keystore that cannot be listed is asked for this run's alias",
			listing:  entry(dead[0]),
			unlisted: true,
			wantRuns: []string{"-list", "-list " + self, "-import " + self},
			wantLeft: -1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useRunsDir(t)
			lockOwnFileElsewhere(t, "live")
			dir := t.TempDir()
			home, calls := filepath.Join(dir, "jdk"), filepath.Join(dir, "calls")
			cacerts := fakeListingJDK(t, home, tc.listing, calls, tc.unlisted)
			if tc.delfail != "" {
				if err := os.WriteFile(calls+".delfail", []byte(tc.delfail+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			caPath := filepath.Join(dir, "ca.crt")
			if err := os.WriteFile(caPath, []byte("ca"), 0o644); err != nil {
				t.Fatal(err)
			}
			core, logs := observer.New(zapcore.DebugLevel)
			// The run's alias as the install is given it, in upper case: the
			// keystore lists it in lower case.
			if _, err := installJavaCAForHome(installCtx(t), zap.New(core), caPath, home, strings.ToUpper(self[:6])+self[6:]); err != nil {
				t.Fatalf("installJavaCAForHome: %v", err)
			}
			got := readCalls(t, calls)
			if len(got) != len(tc.wantRuns) {
				t.Fatalf("keytool runs = %q, want %q", got, tc.wantRuns)
			}
			for i, w := range tc.wantRuns {
				op, alias, _ := strings.Cut(w, " ")
				c := got[i] + " "
				if !strings.HasPrefix(c, "keytool "+op+" ") || !strings.Contains(c, " -keystore "+cacerts+" ") {
					t.Fatalf("keytool run %d = %q, want %s on %s (all: %q)", i, got[i], w, cacerts, got)
				}
				if alias == "" && strings.Contains(c, " -alias ") {
					t.Fatalf("keytool run %d = %q, want a -list of the whole keystore", i, got[i])
				}
				if alias != "" && !strings.Contains(strings.ToLower(c), " -alias "+alias+" ") {
					t.Fatalf("keytool run %d = %q, want it on the alias %s", i, got[i], alias)
				}
			}
			left := logs.FilterMessage("the JDK keystore holds CA aliases that other keploy runs left there").All()
			if tc.wantLeft < 0 {
				if len(left) != 0 {
					t.Fatalf("the install says aliases are left: %v", left[0].ContextMap())
				}
				return
			}
			if len(left) != 1 {
				t.Fatalf("the install says %d times that aliases are left, want once", len(left))
			}
			m := left[0].ContextMap()
			if m["remaining"] != int64(tc.wantLeft) || m["removed"] != int64(tc.wantRemoved) || m["removedPerStart"] != int64(javaSweepMax) || m["cacerts"] != cacerts {
				t.Fatalf("the install says %v, want %d removed and %d remaining of %s", m, tc.wantRemoved, tc.wantLeft, cacerts)
			}
			failed, _ := m["deleteFailed"].(string)
			if (tc.delfail != "") != strings.Contains(failed, "injected for "+tc.delfail) {
				t.Fatalf("deleteFailed = %q, want the failed delete of %q with keytool's output", failed, tc.delfail)
			}
		})
	}
}

func TestJavaLeftovers(t *testing.T) {
	for _, tc := range []struct {
		name, list, alias string
		present           bool
		leftovers         []string
	}{
		{"empty", "", "keploy-mitm-a", false, nil},
		{"English", "Your keystore contains 2 entries\n\n" + entry("keploy-mitm-a") + entry("keploy-mitm-b"), "keploy-mitm-a", true, []string{"keploy-mitm-b"}},
		{"CRLF and another language", "Ihr Keystore enthält 2 Einträge\r\n\r\nkeploy-mitm-b, 20.11.2026, trustedCertEntry,\r\nZertifikat-Fingerprint (SHA-256): 00\r\nkeploy-mitm-a, 20.11.2026, trustedCertEntry,\r\n", "KEPLOY-MITM-A", true, []string{"keploy-mitm-b"}},
		{"not keploy's", entry("mykeploy-mitm-x") + entry("keploy-mitm") + entry("digicert"), "keploy-mitm-a", false, nil},
		{"a fingerprint line is no alias", "Certificate fingerprint (SHA-256): keploy-mitm-z\n", "keploy-mitm-a", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			present, left := javaLeftovers([]byte(tc.list), tc.alias)
			if present != tc.present || strings.Join(left, ",") != strings.Join(tc.leftovers, ",") {
				t.Fatalf("javaLeftovers = %v %q, want %v %q", present, left, tc.present, tc.leftovers)
			}
		})
	}
}

// A sweep that is cancelled lists nothing and says so: the install then asks
// for the alias alone, as when the keystore cannot be listed.
func TestSweepJavaLeftoversWithAnEndedContext(t *testing.T) {
	dir := t.TempDir()
	home, calls := filepath.Join(dir, "jdk"), filepath.Join(dir, "calls")
	cacerts := fakeListingJDK(t, home, entry("keploy-mitm-dead"), calls, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	present, listed := sweepJavaLeftovers(ctx, zap.NewNop(), filepath.Join(home, "bin", "keytool"), cacerts, "keploy-mitm-self")
	if present || listed {
		t.Fatalf("sweepJavaLeftovers = %v %v, want neither", present, listed)
	}
}
