package docker

import (
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/telemetry"
	"go.uber.org/zap"
)

// clearInternalEnv clears what telemetry.AgentEnv reads, so a test reads only
// what it sets, also in keploy's own CI.
func clearInternalEnv(t *testing.T) {
	t.Helper()
	for _, k := range telemetry.InternalVariables() {
		t.Setenv(k, "")
	}
}

// The agent's container gets neither KEPLOY_INTERNAL nor the CI's variables:
// a run that is keploy's own hands it the mark, on the docker run alias and on
// the compose service alike, and a user's run hands it nothing.
func TestTheAgentContainerGetsKeploysOwnMark(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "a user's run"},
		{name: "KEPLOY_INTERNAL=1", env: map[string]string{telemetry.InternalEnv: "1"}, want: true},
		{name: "keploy's CI", env: map[string]string{"CI": "woodpecker", "CI_REPO": "keploy/enterprise"}, want: true},
		{name: "a customer's CI", env: map[string]string{"CI": "woodpecker", "CI_REPO": "acme/shop"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearInternalEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			prev := DockerConfig.Envs
			t.Cleanup(func() { DockerConfig.Envs = prev })
			// A mark an earlier run in this process left is not this run's.
			DockerConfig.Envs = map[string]string{telemetry.InternalEnv: "1"}
			setAgentEnvs(&config.Config{InstallationID: "install-1"})
			v, ok := DockerConfig.Envs[telemetry.InternalEnv]
			if tc.want != ok || (ok && v != "1") {
				t.Errorf("docker run alias: %s=%q (set %v), want set %v", telemetry.InternalEnv, v, ok, tc.want)
			}
			if DockerConfig.Envs["INSTALLATION_ID"] != "install-1" {
				t.Errorf("docker run alias lost the installation id: %v", DockerConfig.Envs)
			}

			svc, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
				AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: models.MODE_TEST,
			})
			if err != nil {
				t.Fatalf("GenerateKeployAgentService: %v", err)
			}
			env := mappingValue(svc, "environment")
			got := env != nil && sequenceContainsPrefix(env, telemetry.InternalEnv+"=1")
			if got != tc.want {
				t.Errorf("compose agent service environment %s: has %s=1 %v, want %v", formatSequence(env), telemetry.InternalEnv, got, tc.want)
			}
		})
	}
}

func TestGenerateKeployAgentService_ForwardsInstallationID(t *testing.T) {
	const hostID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := []struct {
		name      string
		confID    string
		envID     string
		wantEntry string
		wantSet   bool
	}{
		{name: "from config when env unset", confID: hostID, wantEntry: "INSTALLATION_ID=" + hostID, wantSet: true},
		{name: "config wins over host env", confID: hostID, envID: "github-app-12345", wantEntry: "INSTALLATION_ID=" + hostID, wantSet: true},
		{name: "trims config installation id", confID: "  " + hostID + "\n", wantEntry: "INSTALLATION_ID=" + hostID, wantSet: true},
		{name: "does not leak raw host INSTALLATION_ID env when config empty", confID: "", envID: "unsafe$var", wantSet: false},
		{name: "omitted when config whitespace only", confID: "  \t ", envID: "", wantSet: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("INSTALLATION_ID", tc.envID)
			svc, err := (&Impl{
				logger: zap.NewNop(),
				conf:   &config.Config{InstallationID: tc.confID},
			}).GenerateKeployAgentService(models.SetupOptions{
				AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: models.MODE_RECORD,
			})
			if err != nil {
				t.Fatalf("GenerateKeployAgentService: %v", err)
			}
			env := mappingValue(svc, "environment")
			hasAny := env != nil && sequenceContainsPrefix(env, "INSTALLATION_ID=")
			if hasAny != tc.wantSet {
				t.Fatalf("compose agent env %s: has INSTALLATION_ID=%v, want %v", formatSequence(env), hasAny, tc.wantSet)
			}
			if tc.wantSet && !sequenceContainsPrefix(env, tc.wantEntry) {
				t.Fatalf("compose agent env %s missing %q", formatSequence(env), tc.wantEntry)
			}
		})
	}
}
