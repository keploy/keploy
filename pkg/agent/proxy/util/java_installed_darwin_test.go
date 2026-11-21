package util

import (
	"os/exec"
	"testing"
)

// TestIsJavaInstalledReadsMacOSsJavaStubThroughJavaHome: with only /usr/bin
// on PATH, the java there is macOS's stub, which runs a JVM exactly when
// /usr/libexec/java_home names a JDK.
func TestIsJavaInstalledReadsMacOSsJavaStubThroughJavaHome(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	want := exec.Command("/usr/libexec/java_home").Run() == nil
	if got := IsJavaInstalled(); got != want {
		t.Fatalf("IsJavaInstalled() with PATH=/usr/bin = %v, want %v (java_home names a JDK: %v)", got, want, want)
	}
}
