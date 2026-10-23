package tls

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.uber.org/zap/zaptest"
)

// makeFakeJDK builds a $javaHome with a bin/keytool script whose exit codes are
// controlled by importExit. -list always exits 1 (forces the import path);
// -import exits importExit. Returns the javaHome.
func makeFakeJDK(t *testing.T, importExit int) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "fake-jdk")
	binDir := filepath.Join(home, "bin")
	libSec := filepath.Join(home, "lib", "security")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(libSec, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libSec, "cacerts"), []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$1\" in\n  -list) exit 1;;\n" +
		"  -import) exit " + itoa(importExit) + ";;\n  *) exit 0;;\nesac\n"
	if err := os.WriteFile(filepath.Join(binDir, "keytool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	return "1"
}

// A keytool failure must NOT fail CA setup: on macOS the agent runs unprivileged
// and a system-installed JDK's cacerts is root-owned, so the import can EACCES
// for a user who is not even testing a Java app. The pre-fix behaviour returned
// that error and latched a CA failure for the whole run — the exact bug this
// path fixes.
func TestSetupNativeDarwin_KeytoolFailureIsNonFatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake keytool is a /bin/sh script")
	}
	ResetCAReadyForTest()

	javaHome := makeFakeJDK(t, 1) // -import exits 1
	putFakeJavaOnPath(t, javaHome)

	err := setupNativeDarwin(context.Background(), zaptest.NewLogger(t), javaHome)
	if err != nil {
		t.Fatalf("setupNativeDarwin returned %v; a keytool failure must be non-fatal on darwin", err)
	}
	if ready, _ := CAStatus(); !ready {
		t.Fatal("CA must be marked ready even when the Java import failed — Node/Python/Go do not depend on it")
	}
}

// A successful import must still mark ready, and must not leave the temp cert
// file behind (it is only keytool's -file argument).
func TestSetupNativeDarwin_SucceedsAndLeavesNoTempFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake keytool is a /bin/sh script")
	}
	ResetCAReadyForTest()

	before := tempCertFiles(t)

	javaHome := makeFakeJDK(t, 0) // -import exits 0
	putFakeJavaOnPath(t, javaHome)
	if err := setupNativeDarwin(context.Background(), zaptest.NewLogger(t), javaHome); err != nil {
		t.Fatalf("setupNativeDarwin: %v", err)
	}
	if ready, _ := CAStatus(); !ready {
		t.Fatal("CA must be marked ready after a successful setup")
	}

	after := tempCertFiles(t)
	if len(after) > len(before) {
		t.Errorf("a temporary CA file leaked: before=%d after=%d", len(before), len(after))
	}
}

// tempCertFiles lists the ca.crt* scratch files extractCertToTemp creates in
// the temp dir, so a leak is detectable.
// putFakeJavaOnPath makes util.IsJavaInstalled() true on the linux test runner
// by putting the fake JDK's bin (which we give a `java` too) first on PATH.
// On darwin IsJavaInstalled uses java_home instead, but these tests are
// Linux-runnable by design (the darwin build's tests never run in CI).
func putFakeJavaOnPath(t *testing.T, javaHome string) {
	t.Helper()
	bin := filepath.Join(javaHome, "bin")
	if err := os.WriteFile(filepath.Join(bin, "java"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func tempCertFiles(t *testing.T) []string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "ca.crt*"))
	return matches
}
