//go:build windows

package tls

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.uber.org/zap/zaptest"
	"golang.org/x/sys/windows"
)

// On the Windows lane, as a standard user in a session with no desktop (the
// agent's service, as a CI runner, an OpenSSH session or a scheduled task
// runs keploy): Windows cannot ask whether to trust keploy's CA there, and the
// CA's setup still succeeds, with no administrator and nothing added to the
// machine's trust. This row uses only what keploy had before it, so it also
// runs on the commit before the fix, where the setup fell back to the machine
// store and failed.
func TestOnWindowsAStandardUserWithNoDesktopSetsUpTheCAWithNoAdministrator(t *testing.T) {
	if os.Getenv("KEPLOY_ROW_NO_DESKTOP_STANDARD_USER") != "1" {
		t.Skip("runs on the Windows lane as a standard user with no desktop (KEPLOY_ROW_NO_DESKTOP_STANDARD_USER=1)")
	}
	if windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("this row must run as a standard user: an elevated setup may change the machine's trust")
	}
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	ResetCAReadyForTest()
	t.Cleanup(ResetCAReadyForTest)
	logger := zaptest.NewLogger(t)
	err := SetupCA(context.Background(), logger, false)
	t.Cleanup(func() { TeardownNativeCA(logger) })
	if err != nil {
		t.Fatalf("SetupCA as a standard user with no desktop = %v, want nil", err)
	}
	if ready, err := CAStatus(); !ready || err != nil {
		t.Fatalf("CAStatus = %v, %v; want ready", ready, err)
	}
	b, err := os.ReadFile(filepath.Join(local, "Keploy", "ca", "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatal("the kept CA is not PEM")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("certutil", "-store", "ROOT", ca.SerialNumber.Text(16)).CombinedOutput(); err == nil {
		t.Fatalf("the machine's ROOT store holds keploy's CA:\n%s", out)
	}
}
