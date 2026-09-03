package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
)

// newRecordCmdForTest builds the `record` command with the real flag set.
// AddFlags calls AddUncommonFlags itself, so it must not be called twice —
// pflag panics on a redefined flag.
func newRecordCmdForTest(t *testing.T, cfg *config.Config) *cobra.Command {
	t.Helper()
	c := NewCmdConfigurator(zap.NewNop(), cfg)
	cmd := &cobra.Command{Use: "record"}
	if err := c.AddFlags(cmd); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}
	return cmd
}

// writeKeployYML drops a keploy.yml into a fresh temp dir and returns the dir,
// for use as --config-path.
func writeKeployYML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keploy.yml"), []byte(body), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir
}

// record.upstreamTls is the first NESTED block under record: that also has CLI
// flags, so it exercises a corner the flat siblings (capturePackets,
// opportunisticTlsIntercept) never touch: utils.BindFlagsToViper derives a
// viper key from the kebab flag name (upstream-tls-verify →
// record.upstreamTlsVerify), which does not address record.upstreamTls.verify.
// The yaml therefore has to reach the struct on viper.Unmarshal's own mapstructure
// path, and this pins that it does — a silently ignored keploy.yml block would
// otherwise look exactly like "the user left the feature off".
func TestUpstreamTLSConfigBindsFromYAML(t *testing.T) {
	viper.Reset()
	cfg := config.New()
	cmd := newRecordCmdForTest(t, cfg)

	dir := writeKeployYML(t, "record:\n  upstreamTls:\n    verify: true\n    caCert: \"/etc/corp/ca.pem\"\n")
	if err := cmd.Flags().Set("configPath", dir); err != nil {
		t.Fatalf("set configPath: %v", err)
	}

	c := NewCmdConfigurator(zap.NewNop(), cfg)
	if err := c.PreProcessFlags(cmd); err != nil {
		t.Fatalf("PreProcessFlags: %v", err)
	}

	if !cfg.Record.UpstreamTLS.Verify {
		t.Error("record.upstreamTls.verify: true in keploy.yml did not reach config.Record.UpstreamTLS.Verify")
	}
	if got := cfg.Record.UpstreamTLS.CACert; got != "/etc/corp/ca.pem" {
		t.Errorf("record.upstreamTls.caCert = %q, want %q", got, "/etc/corp/ca.pem")
	}
}

// ValidateFlags applies these two flags only when Changed() reports the user
// actually typed them. That gate is load-bearing: flags are registered before
// keploy.yml is read, so their defaults are the pre-config zero values, and an
// unconditional read would push `verify: false` over a keploy.yml that asked for
// true. This pins the invariant the gate depends on — reading a config file does
// NOT mark a flag as changed.
func TestUpstreamTLSFlagsUnchangedWhenOnlyYAMLSetsThem(t *testing.T) {
	viper.Reset()
	cfg := config.New()
	cmd := newRecordCmdForTest(t, cfg)

	dir := writeKeployYML(t, "record:\n  upstreamTls:\n    verify: true\n    caCert: \"/etc/corp/ca.pem\"\n")
	if err := cmd.Flags().Set("configPath", dir); err != nil {
		t.Fatalf("set configPath: %v", err)
	}

	c := NewCmdConfigurator(zap.NewNop(), cfg)
	if err := c.PreProcessFlags(cmd); err != nil {
		t.Fatalf("PreProcessFlags: %v", err)
	}

	for _, name := range []string{"upstream-tls-verify", "upstream-tls-ca-cert"} {
		if cmd.Flags().Changed(name) {
			t.Errorf("--%s reports Changed() after reading keploy.yml; the ValidateFlags gate would overwrite the user's yaml value with the flag default", name)
		}
	}
	// And when the user does type them, Changed() flips and the typed value wins.
	if err := cmd.Flags().Set("upstream-tls-verify", "false"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if !cmd.Flags().Changed("upstream-tls-verify") {
		t.Fatal("--upstream-tls-verify does not report Changed() after being set")
	}
	got, err := cmd.Flags().GetBool("upstream-tls-verify")
	if err != nil {
		t.Fatalf("GetBool: %v", err)
	}
	if got {
		t.Error("explicit --upstream-tls-verify=false did not read back as false")
	}
}

// The alias map lets a keploy.yml key be looked up as its flag. These two are
// dotted where every sibling entry is a single segment, so a typo would resolve
// to nothing and fail silently.
func TestUpstreamTLSFlagAliasesResolve(t *testing.T) {
	cfg := config.New()
	cmd := newRecordCmdForTest(t, cfg)

	for alias, want := range map[string]string{
		"upstreamTls.verify": "upstream-tls-verify",
		"upstreamTls.caCert": "upstream-tls-ca-cert",
	} {
		f := cmd.Flags().Lookup(alias)
		if f == nil {
			t.Errorf("alias %q does not resolve to any flag", alias)
			continue
		}
		if f.Name != want {
			t.Errorf("alias %q resolved to %q, want %q", alias, f.Name, want)
		}
	}
}

// newAgentCmdForTest builds the `agent` command with the real flag set, plus a
// real logger.
//
// ValidateFlags rebuilds the logger for the agent subcommand (log.AddMode),
// which reads the log package's own globals — a zap.NewNop() would leave those
// unset and the rebuilt logger would panic on its first Debug. log.New()
// initialises them exactly as the binary does.
func newAgentCmdForTest(t *testing.T, cfg *config.Config) (*cobra.Command, *zap.Logger) {
	t.Helper()
	logger, logFile, err := log.New()
	if err != nil {
		t.Fatalf("log.New: %v", err)
	}
	t.Cleanup(func() {
		_ = logger.Sync()
		if logFile != nil {
			_ = logFile.Close()
		}
	})
	c := NewCmdConfigurator(logger, cfg)
	cmd := &cobra.Command{Use: "agent"}
	if err := c.AddFlags(cmd); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}
	return cmd, logger
}

// TestAgentRecordsThatTheFlagWasPresent is the CLI half of the off-switch fix.
//
// A native agent reads the SAME keploy.yml as the orchestrator that spawned it
// (--config-path is forwarded), so `record.upstreamTls.verify: true` lands in
// cfg.Record regardless of what the user typed. The only thing that can
// override it is knowing that the orchestrator EXPLICITLY said false — which a
// bare bool cannot express, since false is also the zero value. The markers
// carry that, and proxy.resolveUpstreamTLSConfig consumes them.
//
// FAILS BEFORE THE FIX: there were no markers, and the agent OR-ed the two
// sources, so this configuration resolved to verify=true on native and
// verify=false on docker.
func TestAgentRecordsThatTheFlagWasPresent(t *testing.T) {
	viper.Reset()
	cfg := config.New()
	cmd, logger := newAgentCmdForTest(t, cfg)

	dir := writeKeployYML(t, "record:\n  upstreamTls:\n    verify: true\n    caCert: \"/etc/corp/ca.pem\"\n")
	if err := cmd.Flags().Set("configPath", dir); err != nil {
		t.Fatalf("set configPath: %v", err)
	}
	// Exactly what the orchestrator now forwards for `--upstream-tls-verify=false`.
	if err := cmd.Flags().Set("upstream-tls-verify", "false"); err != nil {
		t.Fatalf("set upstream-tls-verify: %v", err)
	}
	if err := cmd.Flags().Set("upstream-tls-ca-cert", ""); err != nil {
		t.Fatalf("set upstream-tls-ca-cert: %v", err)
	}

	c := NewCmdConfigurator(logger, cfg)
	if err := c.PreProcessFlags(cmd); err != nil {
		t.Fatalf("PreProcessFlags: %v", err)
	}
	if err := c.ValidateFlags(context.Background(), cmd); err != nil {
		t.Fatalf("ValidateFlags: %v", err)
	}

	// The yaml still says true — that is the whole hazard, and it is why the
	// agent cannot simply read cfg.Record.
	if !cfg.Record.UpstreamTLS.Verify {
		t.Fatal("precondition: keploy.yml verify: true did not reach cfg.Record")
	}
	if !cfg.Agent.UpstreamTLSVerifySet {
		t.Fatal("--upstream-tls-verify=false did not mark the flag as present; the agent cannot distinguish it from silence")
	}
	if cfg.Agent.UpstreamTLSVerify {
		t.Fatal("--upstream-tls-verify=false read back as true")
	}
	if !cfg.Agent.UpstreamTLSCACertSet {
		t.Fatal("--upstream-tls-ca-cert= did not mark the flag as present; an empty CA path could not clear a yaml one")
	}
}

// TestAgentMarkersStayUnsetWithoutArgv is the other side: a hand-started
// `keploy agent` with no --upstream-tls-* flags must leave the markers clear so
// its own keploy.yml still governs.
func TestAgentMarkersStayUnsetWithoutArgv(t *testing.T) {
	viper.Reset()
	cfg := config.New()
	cmd, logger := newAgentCmdForTest(t, cfg)

	dir := writeKeployYML(t, "record:\n  upstreamTls:\n    verify: true\n    caCert: \"/etc/corp/ca.pem\"\n")
	if err := cmd.Flags().Set("configPath", dir); err != nil {
		t.Fatalf("set configPath: %v", err)
	}

	c := NewCmdConfigurator(logger, cfg)
	if err := c.PreProcessFlags(cmd); err != nil {
		t.Fatalf("PreProcessFlags: %v", err)
	}
	if err := c.ValidateFlags(context.Background(), cmd); err != nil {
		t.Fatalf("ValidateFlags: %v", err)
	}

	if cfg.Agent.UpstreamTLSVerifySet || cfg.Agent.UpstreamTLSCACertSet {
		t.Fatal("the markers were set without the flags appearing on argv; the agent would ignore its own keploy.yml")
	}
}

// TestDisableAppReadyProbeFlagIsRegistered pins the operator escape
// hatch for a destructive readiness probe.
//
// The field is reachable from keploy.yml and programmatically, but the
// CLI user who hits the hazard most directly — `keploy test` against an
// app behind their own `kubectl port-forward`, where a
// connect-then-close probe can tear the forward down — needs a
// discoverable switch. A struct field alone is not one.
//
// Registration and the camelCase->kebab mapping are pinned separately
// because either half missing leaves the flag unusable: no
// registration and `--disable-app-ready-probe` is rejected outright; no
// mapping and `disableAppReadyProbe` in a config file never binds to it.
func TestDisableAppReadyProbeFlagIsRegistered(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	cfg := &config.Config{}
	c := &CmdConfigurator{cfg: cfg, logger: zap.NewNop()}
	if err := c.AddFlags(cmd); err != nil {
		t.Fatalf("AddFlags: %v", err)
	}

	f := cmd.Flags().Lookup("disable-app-ready-probe")
	if f == nil {
		t.Fatal("--disable-app-ready-probe is not registered: a CLI user behind a port-forward " +
			"has no way to turn off a probe that breaks their connection")
	}
	if f.Value.Type() != "bool" {
		t.Errorf("flag type = %q, want bool", f.Value.Type())
	}

	if err := cmd.Flags().Set("disable-app-ready-probe", "true"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if got := cmd.Flags().Lookup("disable-app-ready-probe").Value.String(); got != "true" {
		t.Errorf("flag value = %q, want true", got)
	}
}
