package provider

import (
	"os"
	"testing"

	"go.keploy.io/server/v3/pkg/platform/telemetry"
)

// A native agent gets KEPLOY_INTERNAL as its --keploy-internal flag (sudo
// drops the variable): the agent marks its own run from it, so its telemetry
// is keploy's own, and without the flag it marks nothing.
func TestAgentInternalFlagMarksTheAgentsRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "with the flag", args: []string{"--" + telemetry.AgentFlag}, want: "1"},
		{name: "without it", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(telemetry.InternalEnv, "")
			runAgentFlags(t, tc.args...)
			if got := os.Getenv(telemetry.InternalEnv); got != tc.want {
				t.Fatalf("`keploy agent %v`: %s=%q, want %q", tc.args, telemetry.InternalEnv, got, tc.want)
			}
		})
	}
}
