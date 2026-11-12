// Package agentargs holds the one list of the CLI's root-level settings that
// every keploy agent launcher hands the agent.
package agentargs

import (
	"fmt"

	"go.keploy.io/server/v3/config"
)

// Root returns the CLI's resolved root-level settings as the agent's flags,
// each with its value, true or false.
//
// The agent is a keploy process of its own, and it resolves these settings
// again from its own config: a docker agent sees no keploy.yml, and the native
// agent reads the very keploy.yml (--config-path) that the CLI's flags may
// have overridden. A setting that travelled only when true could not say
// "false over a keploy.yml saying true", so each travels with its value, and
// the agent ends with the CLI's answer (flag > keploy.yml > default). Without
// them `keploy record --disable-tele` still pinged telemetry from the agent,
// and a docker-run agent never got --debug or --disable-mapping.
//
// Every launcher appends this list (pkg/platform/http nativeAgentArgs, and
// pkg/platform/docker getAlias and GenerateKeployAgentService), so a root
// setting added here reaches the agent the same way in each.
//
// Not here: --storage-format and --json shape the CLI's own output (test sets,
// reports, stdout), which the agent writes none of; --enable-testing travels
// per run, as SetupOptions.EnableTesting.
func Root(conf *config.Config) []string {
	return []string{
		fmt.Sprintf("--debug=%t", conf.Debug),
		fmt.Sprintf("--disable-tele=%t", conf.DisableTele),
		fmt.Sprintf("--disable-ansi=%t", conf.DisableANSI),
		fmt.Sprintf("--disable-mapping=%t", conf.DisableMapping),
	}
}
