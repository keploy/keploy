package provider

import (
	"fmt"
	"strings"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The settings that made calls live, which the proven model deletes (design
// v14 U18, row 1): keploy.yml mock.real, a list of the destinations a run
// reaches for real, replaces every one, and a call to anything it does not
// list is recorded and mocked. Each command's are deleted with the PR that
// makes it read mock.real (release1-cut §5.2): keploy mock's here. keploy
// record and keploy test keep theirs, and refuse a mock.real they would
// record and mock instead of reaching.

// passthroughHint is what every notice tells the user to do.
const passthroughHint = "list the destinations keploy reaches for real in keploy.yml mock.real (localhost:5432, auth.example.com:443, container:redis); every other call is recorded and mocked"

// dropPassthroughKeys clears, under the proven switch, every keploy.yml
// setting that made keploy mock's calls live, and says once which it
// ignored. keploy mock's own --pass-through-ports is the caller's to keep.
func dropPassthroughKeys(logger *zap.Logger, cfg *config.Config) {
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
	if len(cfg.Record.PassThroughPorts) > 0 || len(cfg.Record.PassThroughHosts) > 0 {
		ignored = append(ignored, "record.passThroughPorts/passThroughHosts")
		cfg.Record.PassThroughPorts, cfg.Record.PassThroughHosts = nil, nil
	}
	var fields []zap.Field
	if len(cfg.BypassRules) > 0 {
		ignored = append(ignored, "bypassRules")
		cfg.BypassRules = nil
		fields = append(fields, zap.String("app_port", "keploy mock's --pass-through-ports still keeps the app's own port off keploy's proxy; mock.real is for its dependencies"))
	}
	if len(ignored) == 0 {
		return
	}
	fields = append(fields, zap.Strings("ignored", ignored))
	logger.Warn(fmt.Sprintf("keploy.yml settings that made calls live are ignored by keploy mock under %s (U18): %s", provenswitch.Env, passthroughHint), fields...)
}

// refuseRealOutsideMock refuses, under the proven switch, a keploy.yml
// mock.real for a command that does not read it yet (keploy record, keploy
// test): it would record and mock the destinations the list says are
// reached for real, saying nothing.
func refuseRealOutsideMock(cfg *config.Config, cmdName string) error {
	if !provenswitch.On() || len(cfg.Mock.Real) == 0 {
		return nil
	}
	return fmt.Errorf("keploy %s does not read keploy.yml mock.real in this build (keploy mock record and keploy mock replay do): it would record and mock %s instead of reaching them; use keploy mock, or run keploy %s with a keploy.yml that has no mock.real (--config-path names the directory it is in)",
		cmdName, strings.Join(cfg.Mock.Real, ", "), cmdName)
}
