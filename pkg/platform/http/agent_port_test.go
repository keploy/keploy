package http

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// A Docker agent's port is picked as a container's port published as itself
// (utils.GetAvailableContainerPort: above the container's ephemeral range,
// where Docker's embedded DNS resolver holds its ports), a native agent's as
// a host's (utils.GetAvailablePort), each never one of the ports the proxy
// and DNS server are handed (the Except variants): by every kind of command
// Setup starts an agent for.
func TestSetupPicksADockerAgentsPortAsAContainersPort(t *testing.T) {
	// The pickers Setup uses in production, before this test replaces them:
	// a swapped or host-wide picker here would undo the container rule.
	if reflect.ValueOf(containerPort).Pointer() != reflect.ValueOf(utils.GetAvailableContainerPortExcept).Pointer() {
		t.Fatal("containerPort is not utils.GetAvailableContainerPortExcept")
	}
	if reflect.ValueOf(hostPort).Pointer() != reflect.ValueOf(utils.GetAvailablePortExcept).Pointer() {
		t.Fatal("hostPort is not utils.GetAvailablePortExcept")
	}
	defer func(h, c func(...uint32) (uint32, error)) { hostPort, containerPort = h, c }(hostPort, containerPort)
	stop := errors.New("stop at the agent's port")
	var picked []string
	hostPort = func(...uint32) (uint32, error) { picked = append(picked, "host"); return 0, stop }
	containerPort = func(...uint32) (uint32, error) { picked = append(picked, "container"); return 0, stop }
	for _, c := range []struct {
		kind utils.CmdType
		want string
	}{
		{utils.Native, "host"},
		{utils.DockerRun, "container"},
		{utils.DockerStart, "container"},
		{utils.DockerCompose, "container"},
		{utils.FromContainer, "container"},
	} {
		picked = nil
		a := &AgentClient{logger: zap.NewNop(), conf: &config.Config{}}
		err := a.Setup(context.Background(), "app", models.SetupOptions{CommandType: string(c.kind)})
		if !errors.Is(err, stop) || len(picked) != 1 || picked[0] != c.want {
			t.Fatalf("%s: Setup returned %v after picking %v; want the %s picker's port, once", c.kind, err, picked, c.want)
		}
	}
}
