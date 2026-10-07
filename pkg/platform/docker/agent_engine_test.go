package docker

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/engine"
	"go.uber.org/zap"
)

// The agent container is started with the active engine's CLI and its
// security options. On Docker that is label=disable: an SELinux host denies
// the agent bpf(2) otherwise, and it exits before it is ready. On an engine a
// build registers (Podman), its own CLI and options.
func TestGetAlias_LinuxStartsTheAgentOnTheActiveEngine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	opts := models.SetupOptions{KeployContainer: "keploy-test", AgentPort: 16790, Mode: models.MODE_RECORD}

	alias, err := getAlias(context.Background(), zap.NewNop(), opts, false)
	require.NoError(t, err)
	require.Regexp(t, `^(sudo .*)?docker container run `, alias, "the agent is not started with docker's CLI")
	require.Contains(t, alias, " --security-opt label=disable ",
		"the agent runs SELinux-confined, so an SELinux host denies it bpf(2): %s", alias)

	engine.Register("podman-test", func(context.Context, *zap.Logger) (engine.Runtime, error) {
		return engine.Runtime{
			Name:              "podman-test",
			CLI:               "/usr/bin/podman",
			Compose:           []string{"/usr/bin/podman", "compose"},
			AgentSecurityOpts: []string{"label=disable", "seccomp=unconfined"},
		}, nil
	})
	require.NoError(t, engine.Prepare(context.Background(), zap.NewNop(), "podman-test"))
	t.Cleanup(func() { _ = engine.Prepare(context.Background(), zap.NewNop(), engine.Docker) })

	alias, err = getAlias(context.Background(), zap.NewNop(), opts, false)
	require.NoError(t, err)
	require.Contains(t, alias, "/usr/bin/podman container run ", "the agent is not started with the engine's CLI: %s", alias)
	require.NotContains(t, alias, "docker container run", "the agent is still started with docker: %s", alias)
	require.Contains(t, alias, " --security-opt label=disable --security-opt seccomp=unconfined ",
		"the agent is not given the engine's security options: %s", alias)
}
