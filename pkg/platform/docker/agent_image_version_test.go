package docker

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// keploy runs its agent in docker, and under docker compose, from the image
// of its own version, never another: a keploy mock replay reads, from the
// agent it starts, what that agent relayed to a live server (GET
// /agent/relayed, or the account a compose agent leaves as it stops), and a
// keploy older than that read never drives an agent newer than itself, whose
// relayed connections (a Redis the open-source keploy does not mock) it
// would not know of and would claim the run isolated over. (Natively the
// agent is this binary: startNativeAgent runs utils.GetCurrentBinaryPath.)
// An image tag derived from anything but this keploy's version fails this
// test. (A compose service hook can rewrite the agent's service afterwards,
// as Keploy Enterprise's in-pod replay stack does; keploy mock replay
// installs none.)
func TestTheAgentImageIsThisKeploysVersion(t *testing.T) {
	saved := utils.Version
	t.Cleanup(func() { utils.Version = saved })
	utils.Version = "3.6.999"
	const want = "ghcr.io/keploy/keploy:v3.6.999"

	service, err := (&Impl{logger: zap.NewNop(), conf: &config.Config{}}).GenerateKeployAgentService(models.SetupOptions{
		KeployContainer: "keploy-agent", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, Mode: models.MODE_TEST,
	})
	require.NoError(t, err)
	image := mappingValue(service, "image")
	require.NotNil(t, image, "the compose agent service names no image")
	require.Equal(t, want, image.Value, "the compose agent service runs an image of another keploy version")

	if runtime.GOOS != "linux" {
		return
	}
	alias, err := getAlias(context.Background(), zap.NewNop(), models.SetupOptions{KeployContainer: "keploy-test", AgentPort: 16790, Mode: models.MODE_TEST}, false)
	require.NoError(t, err)
	require.Contains(t, alias, " "+want+" ", "the docker agent runs an image of another keploy version: %s", alias)
}
