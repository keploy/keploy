package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// The upgrade banner tells the user how to install Keploy from keploy.io. The
// command it printed before saved the installer as ./install.sh, which
// overwrote a repository's own install.sh when run from the repo root; it now
// prints models.InstallCommand, which writes nothing there.
func TestTheUpgradeBannerPrintsTheInstallCommand(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("KEPLOY_INDOCKER", "")
	savedArgs, savedStderr := os.Args, os.Stderr
	t.Cleanup(func() { os.Args, os.Stderr = savedArgs, savedStderr })
	os.Args = []string{"keploy", "record"}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	os.Stderr = w
	printEnterpriseUpgradeBanner()
	os.Stderr = savedStderr
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Install:  "+models.InstallCommand+"\n") {
		t.Fatalf("the banner must print the install command %q, got:\n%s", models.InstallCommand, out)
	}
	if strings.Contains(string(out), "-O") {
		t.Fatalf("the banner prints a command that saves the installer in the working directory:\n%s", out)
	}
}
