package provider

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The settings that made calls live, which the proven model deletes (design
// v14 U18, row 1): keploy.yml mock.real, a list of the destinations a run
// reaches for real, replaces every one, and a call to anything it does not
// list is recorded and mocked. keploy mock, keploy record and keploy test
// read mock.real, so under the switch their passthrough flags are refused,
// as unknown flags are, and their keploy.yml keys are
// ignored, with one notice. keploy mock keeps its own --pass-through-ports
// (the app's ingress port) until the ingress row (PR 4).

// passthroughFlags are the deleted flags of record and test.
var passthroughFlags = []string{"global-passthrough", "opportunistic-tls-intercept", "mocking", "pass-through-ports"}

// passthroughKeys are the keploy.yml keys of the same settings, which
// dropPassthroughKeys ignores (agent's are keploy's own, never in a
// keploy.yml it writes).
var passthroughKeys = []string{"record.globalPassthrough", "record.opportunisticTlsIntercept", "test.mocking", "record.passThroughPorts", "record.passThroughHosts", "bypassRules"}

// passthroughHint is what every refusal and notice tells the user to do.
const passthroughHint = "list the destinations keploy reaches for real in keploy.yml mock.real (localhost:5432, auth.example.com:443, container:redis); every other call is recorded and mocked"

// refusePassthroughFlags refuses, under the proven switch, a deleted
// passthrough flag the user gave: a usage error (exit 8), as an unknown flag
// is.
func refusePassthroughFlags(cmd *cobra.Command, names ...string) error {
	if !provenswitch.On() {
		return nil
	}
	for _, n := range names {
		if f := cmd.Flags().Lookup(n); f != nil && f.Changed {
			return utils.UsageError(fmt.Errorf("--%s is not used with the proven model (U18): %s", n, passthroughHint))
		}
	}
	return nil
}

// dropPassthroughKeys clears, under the proven switch, every keploy.yml
// setting that made cmdName's calls live, and says once which it ignored.
// keploy mock's own --pass-through-ports is the caller's to keep.
func dropPassthroughKeys(logger *zap.Logger, cfg *config.Config, cmdName string) {
	if !provenswitch.On() {
		return
	}
	var ignored []string
	if cfg.Record.GlobalPassthrough || cfg.Agent.GlobalPassthrough {
		ignored = append(ignored, "globalPassthrough")
		cfg.Record.GlobalPassthrough, cfg.Agent.GlobalPassthrough = false, false
	}
	if cfg.Record.OpportunisticTLSIntercept || cfg.Agent.OpportunisticTLSIntercept {
		ignored = append(ignored, "opportunisticTlsIntercept")
		cfg.Record.OpportunisticTLSIntercept, cfg.Agent.OpportunisticTLSIntercept = false, false
	}
	if cmdName == "test" && !cfg.Test.Mocking {
		ignored = append(ignored, "test.mocking: false")
		cfg.Test.Mocking = true
	}
	if len(cfg.Record.PassThroughPorts) > 0 || len(cfg.Record.PassThroughHosts) > 0 {
		ignored = append(ignored, "record.passThroughPorts/passThroughHosts")
		cfg.Record.PassThroughPorts, cfg.Record.PassThroughHosts = nil, nil
	}
	var fields []zap.Field
	if len(cfg.BypassRules) > 0 {
		ignored = append(ignored, "bypassRules")
		cfg.BypassRules = nil
		if cmdName == "mock" {
			fields = append(fields, zap.String("app_port", "keploy mock's --pass-through-ports still keeps the app's own port off keploy's proxy; mock.real is for its dependencies"))
		}
	}
	if len(ignored) == 0 {
		return
	}
	fields = append(fields, zap.Strings("ignored", ignored))
	logger.Warn(fmt.Sprintf("keploy.yml settings that made calls live are ignored under the proven model (U18): %s", passthroughHint), fields...)
}
