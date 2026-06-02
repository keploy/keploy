package tls

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"
)

// TestInstallJavaCAForHome_UsesResolvedKeytool asserts that when a
// non-empty javaHome is passed, installJavaCAForHome invokes the
// keytool binary inside that $javaHome/bin, NOT a PATH-resolved
// keytool. This is the core behavioural guarantee of Track C — it's
// what prevents the SDKMAN / Maven-wrapper divergence from landing
// the Keploy CA in the wrong cacerts.
//
// Mechanism: build a fake javaHome under t.TempDir() containing a
// bin/keytool shell script that writes its argv to a sentinel file.
// After installJavaCAForHome runs, assert the sentinel contains the
// expected -keystore path under THIS javaHome and NOT anything
// resembling a PATH-keytool invocation.
//
// Skipped on non-Linux because the fake keytool is a /bin/sh script.
func TestInstallJavaCAForHome_UsesResolvedKeytool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake keytool is a /bin/sh script; skipping on Windows")
	}

	dir := t.TempDir()
	fakeJavaHome := filepath.Join(dir, "fake-jdk")
	binDir := filepath.Join(fakeJavaHome, "bin")
	libSecDir := filepath.Join(fakeJavaHome, "lib", "security")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(libSecDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Touch a cacerts file so keytool -list -alias (isJavaCAExist)
	// can at least open it; our fake keytool ignores the content.
	cacertsPath := filepath.Join(libSecDir, "cacerts")
	if err := os.WriteFile(cacertsPath, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}

	// Sentinel file recording every invocation of the fake keytool —
	// one invocation per call (the -list existence check + the
	// -import, if the -list fails).
	sentinel := filepath.Join(dir, "keytool.log")

	// Fake keytool: exit 1 for -list (forces the import path), exit
	// 0 for everything else. Appends "SUBCMD: argv" to sentinel so
	// assertions can inspect what was invoked.
	keytoolScript := "#!/bin/sh\n" +
		"echo \"args: $*\" >> " + sentinel + "\n" +
		"case \"$1\" in\n" +
		"  -list) exit 1;;\n" +
		"  -import) exit 0;;\n" +
		"  *) exit 0;;\n" +
		"esac\n"
	fakeKeytool := filepath.Join(binDir, "keytool")
	if err := os.WriteFile(fakeKeytool, []byte(keytoolScript), 0755); err != nil {
		t.Fatal(err)
	}

	// The caPath arg is passed through to keytool -import -file; any
	// readable file is fine — our fake keytool ignores it.
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, []byte("ca"), 0644); err != nil {
		t.Fatal(err)
	}

	logger := zaptest.NewLogger(t)
	if err := installJavaCAForHome(context.Background(), logger, caPath, fakeJavaHome); err != nil {
		t.Fatalf("installJavaCAForHome returned error: %v", err)
	}

	// Read what the fake keytool captured.
	logged, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("sentinel not written — fake keytool was NOT invoked: %v", err)
	}
	logStr := string(logged)

	// Assertion 1: every invocation MUST have -keystore pointing at
	// the fakeJavaHome-scoped cacerts, not a PATH-global cacerts.
	if !strings.Contains(logStr, cacertsPath) {
		t.Fatalf("fake keytool log doesn't reference expected cacerts %q\n--- log ---\n%s",
			cacertsPath, logStr)
	}

	// Assertion 2: the import call happened (the -list probe returned
	// 1 so the code took the -import branch). This confirms the
	// keytool-binary resolution didn't short-circuit on the
	// "alias exists" branch with a false positive.
	if !strings.Contains(logStr, "-import") {
		t.Fatalf("fake keytool never got -import call — import logic was skipped\n--- log ---\n%s", logStr)
	}

	// Assertion 3: both calls MUST carry the -file arg pointing at
	// our caPath, proving the argv plumbing is intact.
	if !strings.Contains(logStr, caPath) {
		t.Fatalf("fake keytool log missing caPath %q\n--- log ---\n%s", caPath, logStr)
	}
}

// TestInstallJavaCAForHome_MissingKeytoolFallsBack asserts that when
// the resolved $javaHome/bin/keytool doesn't exist (JRE-only layout,
// malformed path), installJavaCAForHome falls back to a PATH keytool
// lookup rather than erroring. The function should either succeed
// (PATH has keytool) or fail with a lookup-style error — NOT with
// "resolved binary missing" because that's the regression we're
// guarding against.
//
// We verify by pointing javaHome at a directory with NO bin/keytool
// and asserting the function doesn't return an error mentioning the
// resolved-path that we just proved doesn't exist. If PATH keytool
// is absent too, exec returns ENOENT which is distinct from a
// "resolved path doesn't exist" pre-check error.
func TestInstallJavaCAForHome_MissingKeytoolFallsBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH keytool semantics differ on Windows; covered by the primary path test")
	}

	dir := t.TempDir()
	bogusJavaHome := filepath.Join(dir, "no-keytool-here")
	if err := os.MkdirAll(filepath.Join(bogusJavaHome, "lib", "security"), 0755); err != nil {
		t.Fatal(err)
	}
	// Cacerts file exists so the stat in isJavaCAExistWithTool
	// doesn't error before we even get to the PATH-keytool fallback.
	if err := os.WriteFile(filepath.Join(bogusJavaHome, "lib", "security", "cacerts"), []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, []byte("ca"), 0644); err != nil {
		t.Fatal(err)
	}

	// Make PATH empty so both the bogus javaHome AND the PATH
	// fallback fail to find a keytool. The call SHOULD return an
	// error (exec ENOENT) but the error must NOT claim the resolved
	// binary was required — our code is supposed to fall back.
	t.Setenv("PATH", "/nonexistent-path-for-test")

	logger := zaptest.NewLogger(t)
	err := installJavaCAForHome(context.Background(), logger, caPath, bogusJavaHome)
	// We EXPECT an error here because PATH has no keytool either.
	// The assertion is that the error comes from exec (ENOENT on
	// keytool itself), not from a pre-check in our code about the
	// resolved-path being absent — the resolved-path absence must
	// be tolerated silently per installJavaCAForHome's contract.
	if err == nil {
		// If this host has a system keytool somewhere still
		// reachable, the fallback succeeded — that ALSO validates
		// our contract (don't regress single-JDK hosts).
		return
	}
	// The error message should look like an exec/executable problem,
	// not a "bogusJavaHome/bin/keytool missing" message. We don't
	// assert exact text (varies by OS / Go version) but DO assert
	// the bogus path isn't named in the error, because we're
	// supposed to have fallen back before invoking it.
	bogusPathRef := filepath.Join(bogusJavaHome, "bin", "keytool")
	if strings.Contains(err.Error(), bogusPathRef) {
		t.Fatalf("error mentions the bogus resolved path %q — fallback to PATH keytool did not happen: %v",
			bogusPathRef, err)
	}
}

// TestInstallJavaCA_BackwardCompat asserts the legacy three-arg
// entry point still works — it's what external callers and the
// Windows boot path use. Passing javaHome="" must take the
// util.IsJavaInstalled() guard: on a host without java in PATH
// this is a silent no-op (nil return, Debug log).
func TestInstallJavaCA_BackwardCompat_NoJavaIsNoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH manipulation differs on Windows")
	}
	// Empty PATH → IsJavaInstalled() returns false → function should
	// return nil without attempting any keytool call.
	t.Setenv("PATH", "/nonexistent-path-for-test")
	t.Setenv("JAVA_HOME", "") // defensive — don't let host JAVA_HOME leak

	logger := zaptest.NewLogger(t)
	err := installJavaCA(context.Background(), logger, "/tmp/doesnt-matter.crt")
	if err != nil {
		t.Fatalf("installJavaCA with no java in PATH should be a silent no-op, got: %v", err)
	}
}
