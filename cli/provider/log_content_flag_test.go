package provider

import (
	"testing"

	"github.com/spf13/viper"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils/logcontent"
)

// A native agent started through sudo never sees KEPLOY_LOG_CONTENT: it must
// accept the flag its launcher appends instead (logcontent.AgentArgs), and its
// lines then carry content as the CLI's do. Without the flag its switch is
// what its own environment said.
func TestAgentLogContentFlagTurnsTheSwitchOn(t *testing.T) {
	t.Cleanup(logcontent.Set(false))
	runAgentFlags(t)
	if logcontent.On() {
		t.Fatal("`keploy agent` without --log-content turned the switch on")
	}
	runAgentFlags(t, "--"+logcontent.AgentFlag)
	if !logcontent.On() {
		t.Fatalf("`keploy agent --%s` left the switch off: an agent the CLI started with %s=1 logs no content", logcontent.AgentFlag, logcontent.EnvVar)
	}
}

// The flag is internal: set by the launcher, never shown to users.
func TestAgentLogContentFlagIsHidden(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	cmd, _ := newAgentCmdForTest(t, config.New())
	f := cmd.Flags().Lookup(logcontent.AgentFlag)
	if f == nil {
		t.Fatalf("`keploy agent` has no --%s flag; an agent started by a CLI with %s=1 would refuse to start", logcontent.AgentFlag, logcontent.EnvVar)
	}
	if !f.Hidden {
		t.Fatalf("--%s is shown in `keploy agent --help`; it is the launcher's, not the user's", logcontent.AgentFlag)
	}
}
