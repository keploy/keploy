// Package switchtest keeps a package's test rows from inheriting the proven
// switch. provenswitch.On reads KEPLOY_PROVEN on every call, so a row that
// checks the rules with the switch off, and does not pin it, would check the
// other rules (or hang on a connection only those rules keep open) in a test
// process started with KEPLOY_PROVEN=1. A package whose rows read the switch
// clears it in its TestMain (Clear, or Main), and proves it does with
// OffAtEveryRowsStart.
package switchtest

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

// childEnv marks the run of the test binary OffAtEveryRowsStart starts.
const childEnv = "KEPLOY_SWITCHTEST_CHILD"

// Clear clears the KEPLOY_PROVEN the test process inherited. A package's
// TestMain calls it before m.Run, so every row starts with the switch off and a
// row that checks the switch-on rules turns it on itself
// (t.Setenv(provenswitch.Env, "1")).
func Clear() {
	_ = os.Unsetenv(provenswitch.Env)
}

// Main is the TestMain of a package that needs nothing else of one: Clear,
// then the rows.
func Main(m *testing.M) {
	Clear()
	os.Exit(m.Run())
}

// OffAtEveryRowsStart runs the calling row again in a test binary started with
// KEPLOY_PROVEN=1, where it checks that the switch is off: the package's
// TestMain calls Clear. It must be called from a top-level row, which it reruns
// by name.
func OffAtEveryRowsStart(t *testing.T) {
	t.Helper()
	if os.Getenv(childEnv) == "1" {
		if provenswitch.On() {
			t.Fatalf("a row started with the proven switch on, from the %s=%q its test process inherited: the package's TestMain does not call switchtest.Clear", provenswitch.Env, os.Getenv(provenswitch.Env))
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), provenswitch.Env+"=1", childEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the test binary started with %s=1 failed: %v\n%s", provenswitch.Env, err, out)
	}
	// A run that matched no row passes too: it must have run this one.
	if !strings.Contains(string(out), "--- PASS: "+t.Name()+" ") {
		t.Fatalf("the test binary started with %s=1 did not run %s:\n%s", provenswitch.Env, t.Name(), out)
	}
}
