package docker

import (
	"context"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// TestMain clears the session token file the suite leaves behind.
//
// GenerateKeployAgentService writes one, so every test in this package that
// calls it creates the file as a side effect — not just the token tests below.
// Without this, a package run drops a live token into the temp directory and
// leaves it there, once per CI run.
func TestMain(m *testing.M) {
	code := m.Run()
	if err := token.RemoveFile(); err != nil {
		panic(err)
	}
	os.Exit(code)
}

// TestGenerateKeployAgentService_CarriesTheTokenByReferenceNotByValue pins the
// reason the token is an env_file and not an `environment:` entry: this compose
// file is written into the user's project 0644 (see docker.go's os.WriteFile),
// so a token spelled out in it would be readable by every local user — exactly
// the ones the token exists to keep out of the control plane.
func TestGenerateKeployAgentService_CarriesTheTokenByReferenceNotByValue(t *testing.T) {
	serviceNode, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
		AgentPort:       16789,
		KeployContainer: "keploy-agent",
	})
	require.NoError(t, err)

	var svc struct {
		EnvFile     []string `yaml:"env_file"`
		Environment []string `yaml:"environment"`
	}
	raw, err := yaml.Marshal(serviceNode)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(raw, &svc), "the generated node must be valid compose YAML")

	require.Len(t, svc.EnvFile, 1, "the agent service must reference exactly one env file")
	tokenFile := svc.EnvFile[0]

	got, err := token.ReadFile(tokenFile)
	require.NoError(t, err, "compose must point at a file that actually holds the token")
	require.Equal(t, token.Session(), got)

	// The secret itself must appear nowhere in the file compose writes.
	require.NotContains(t, string(raw), token.Session(),
		"the token is spelled out in the compose file, which is written 0644 into the user's project")
	for _, env := range svc.Environment {
		require.NotContains(t, env, token.Env)
	}
}

// TestGetAlias_LinuxPassesTheTokenByFileNotOnTheCommandLine is the wiring
// assertion for the `docker run` arm. The token reaching the agent container at
// all is what makes the control plane usable once it is guarded, and a token
// spelled out in the alias would be visible in `ps` to every user on the box.
func TestGetAlias_LinuxPassesTheTokenByFileNotOnTheCommandLine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}

	alias, err := getAlias(context.Background(), zap.NewNop(), models.SetupOptions{
		KeployContainer: "keploy-test",
		AgentPort:       16789,
		Mode:            models.MODE_RECORD,
	}, false)
	require.NoError(t, err)

	path, err := token.WriteFile()
	require.NoError(t, err)
	// shellQuote'd: the alias is a shell command string on unix.
	require.Contains(t, alias, "--env-file "+shellQuote(path),
		"the agent container gets no token, so every control-plane call it receives from the CLI would be a 401")

	require.NotContains(t, alias, token.Session(),
		"the token is in the docker run command line, where `ps` shows it to every local user")
	require.NotContains(t, alias, "-e "+token.Env+"=",
		"the token must travel as a file reference, not an inline env assignment")
}
