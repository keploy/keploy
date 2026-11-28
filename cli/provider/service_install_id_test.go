package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
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

func TestGetService_ResolvesInstallationIDLazilyAndHonorsDisableTele(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("KEPLOY_INDOCKER", "true")
	t.Setenv("INSTALLATION_ID", "")

	keployYAML := filepath.Join(home, ".keploy", "keploy.yaml")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// DisableTele=true: GetService must not resolve or persist a fallback ID.
	disabledCfg := config.New()
	disabledCfg.DisableTele = true
	_, _ = NewServiceProvider(zap.NewNop(), disabledCfg).GetService(ctx, "unknown-cmd")
	if disabledCfg.InstallationID != "" {
		t.Fatalf("GetService with DisableTele=true set InstallationID=%q, want empty", disabledCfg.InstallationID)
	}
	if _, err := os.Stat(keployYAML); !os.IsNotExist(err) {
		t.Fatalf("GetService with DisableTele=true must not create %s (stat err=%v)", keployYAML, err)
	}

	// DisableTele=false: GetService itself invokes ensureInstallationID and persists a 64-hex ID.
	enabledCfg := config.New()
	enabledCfg.DisableTele = false
	_, _ = NewServiceProvider(zap.NewNop(), enabledCfg).GetService(ctx, "unknown-cmd")
	if len(enabledCfg.InstallationID) != 64 {
		t.Fatalf("GetService with DisableTele=false set InstallationID=%q (len %d), want 64-hex", enabledCfg.InstallationID, len(enabledCfg.InstallationID))
	}
	if _, err := os.Stat(keployYAML); err != nil {
		t.Fatalf("expected GetService to persist %s: %v", keployYAML, err)
	}
}

func TestHonorTelemetryOptOut_FlagValueRespected(t *testing.T) {
	newCmd := func(args ...string) *cobra.Command {
		cmd := &cobra.Command{Use: "config"}
		cmd.Flags().Bool("disable-tele", false, "")
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatalf("ParseFlags(%v): %v", args, err)
		}
		return cmd
	}

	// Explicit --disable-tele=false must keep telemetry enabled even when the env var is set.
	t.Setenv("KEPLOY_DISABLETELE", "true")
	cfgFalse := config.New()
	NewCmdConfigurator(zap.NewNop(), cfgFalse).HonorTelemetryOptOut(newCmd("--disable-tele=false"))
	if cfgFalse.DisableTele {
		t.Fatal("explicit --disable-tele=false must not disable telemetry")
	}

	// Explicit --disable-tele=true disables telemetry.
	t.Setenv("KEPLOY_DISABLETELE", "")
	cfgTrue := config.New()
	NewCmdConfigurator(zap.NewNop(), cfgTrue).HonorTelemetryOptOut(newCmd("--disable-tele=true"))
	if !cfgTrue.DisableTele {
		t.Fatal("explicit --disable-tele=true must disable telemetry")
	}

	// Unchanged flag falls back to KEPLOY_DISABLETELE=true.
	t.Setenv("KEPLOY_DISABLETELE", "true")
	cfgEnv := config.New()
	NewCmdConfigurator(zap.NewNop(), cfgEnv).HonorTelemetryOptOut(newCmd())
	if !cfgEnv.DisableTele {
		t.Fatal("KEPLOY_DISABLETELE=true must disable telemetry when flag is unset")
	}
}
