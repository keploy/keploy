package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

func TestEnsureInstallationID_LazyAndRespectsDisableTele(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("KEPLOY_INDOCKER", "true")
	t.Setenv("INSTALLATION_ID", "")

	keployYAML := filepath.Join(home, ".keploy", "keploy.yaml")

	// When telemetry is disabled (for example --disable-tele), no fallback ID
	// may be generated or persisted to disk.
	disabledCfg := config.New()
	disabledCfg.DisableTele = true
	spDisabled := NewServiceProvider(zap.NewNop(), disabledCfg)
	spDisabled.ensureInstallationID(context.Background())

	if disabledCfg.InstallationID != "" {
		t.Fatalf("DisableTele=true resolved InstallationID=%q, want empty", disabledCfg.InstallationID)
	}
	if _, err := os.Stat(keployYAML); !os.IsNotExist(err) {
		t.Fatalf("DisableTele=true must not create %s (stat err=%v)", keployYAML, err)
	}

	// When telemetry is enabled, the fallback ID is resolved lazily and reused.
	enabledCfg := config.New()
	enabledCfg.DisableTele = false
	spEnabled := NewServiceProvider(zap.NewNop(), enabledCfg)
	spEnabled.ensureInstallationID(context.Background())

	if len(enabledCfg.InstallationID) != 64 {
		t.Fatalf("DisableTele=false resolved InstallationID=%q (len %d), want 64-hex", enabledCfg.InstallationID, len(enabledCfg.InstallationID))
	}
	if _, err := os.Stat(keployYAML); err != nil {
		t.Fatalf("expected %s to be persisted on enabled run: %v", keployYAML, err)
	}

	// A second enabled invocation reads the persisted ID rather than minting a new one.
	secondCfg := config.New()
	secondCfg.DisableTele = false
	NewServiceProvider(zap.NewNop(), secondCfg).ensureInstallationID(context.Background())
	if secondCfg.InstallationID != enabledCfg.InstallationID {
		t.Fatalf("second enabled run got %q, want persisted %q", secondCfg.InstallationID, enabledCfg.InstallationID)
	}
}
