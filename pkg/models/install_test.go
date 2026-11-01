package models

import (
	"strings"
	"testing"
)

// The install command keploy prints must run the installer in the calling
// shell without saving it in the working directory: run from a repository
// root, a saved ./install.sh overwrote the repository's own.
func TestInstallCommandWritesNothingInTheWorkingDirectory(t *testing.T) {
	for _, saves := range []string{" -O", "--remote-name", "-o ", "--output", "install.sh &&", "&& source install.sh", "&& bash install.sh"} {
		if strings.Contains(InstallCommand, saves) {
			t.Errorf("InstallCommand %q saves the installer to a file (%q)", InstallCommand, saves)
		}
	}
	// Sourced, so the PATH the installer sets stays set in the caller's
	// shell; from /dev/stdin, because bash 3.2 (macOS's /bin/bash) sources a
	// `<(curl ...)` process substitution as an empty file and installs
	// nothing.
	if !strings.HasPrefix(InstallCommand, "source /dev/stdin <<<") || strings.Contains(InstallCommand, "<(") {
		t.Errorf("InstallCommand %q must source the installer from /dev/stdin through a here-string", InstallCommand)
	}
	// -f: a failed download (an HTTP error page) is not handed to the shell.
	if !strings.Contains(InstallCommand, "curl -fsSL https://keploy.io/install.sh") {
		t.Errorf("InstallCommand %q must fetch https://keploy.io/install.sh with curl -fsSL", InstallCommand)
	}
}
