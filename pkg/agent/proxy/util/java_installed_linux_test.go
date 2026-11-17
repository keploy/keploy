package util

import (
	"os"
	"os/exec"
	"testing"
)

// TestIsJavaInstalledOffMacOSIsTheJavaOnPATH: off macOS no /usr/bin/java is
// a launcher stub. With only /usr/bin on PATH, IsJavaInstalled says yes
// exactly when exec.LookPath finds a java there, as on a Linux machine whose
// java is the alternatives link, and never asks macOS's java_home.
func TestIsJavaInstalledOffMacOSIsTheJavaOnPATH(t *testing.T) {
	if _, err := os.Stat("/usr/bin/java"); err != nil {
		t.Skipf("no /usr/bin/java on this machine: %v", err)
	}
	t.Setenv("PATH", "/usr/bin")
	exe, err := exec.LookPath("java")
	want := err == nil
	if got := IsJavaInstalled(); got != want {
		t.Fatalf("IsJavaInstalled() with PATH=/usr/bin = %v, want %v (exec.LookPath(\"java\") = %q, %v)", got, want, exe, err)
	}
}
