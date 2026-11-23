package http

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// An agent addressed again is reached where it is now: the address keploy
// reads (config.(*Config).AgentURI) is the one addressAgent was given last,
// not one an earlier agent of the same run was reached at.
func TestAnAgentAddressedAgainIsReachedWhereItIsNow(t *testing.T) {
	conf := &config.Config{}
	conf.SetAgentURI("http://localhost:40999/agent")
	a := &AgentClient{logger: zap.NewNop(), conf: conf}
	a.addressAgent("http://localhost:41234/agent")
	if got := conf.AgentURI(); got != "http://localhost:41234/agent" {
		t.Fatalf("AgentURI() = %q after addressAgent; want http://localhost:41234/agent", got)
	}
}

// A Setup's agent is reached where that Setup put it, also when an earlier
// agent on the same config was reached elsewhere (replay sets an agent up
// again, and a compose start may have moved the last one). The Setup stops at
// the write of perf_event_paranoid, past the agent's address.
func TestASetupAddressesItsAgentWhereItPutIt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the Setup is stopped at perf_event_paranoid's write, which is Linux's")
	}
	defer func(h, c func(...uint32) (uint32, error)) { hostPort, containerPort = h, c }(hostPort, containerPort)
	containerPort = func(...uint32) (uint32, error) { return 41234, nil }
	hostPort = containerPort
	was := perfEventParanoidPath
	perfEventParanoidPath = filepath.Join(t.TempDir(), "missing", "perf_event_paranoid")
	t.Cleanup(func() { perfEventParanoidPath = was })
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })

	conf := &config.Config{}
	conf.SetAgentURI("http://localhost:40999/agent")
	a := &AgentClient{logger: zap.NewNop(), conf: conf}
	if err := a.Setup(context.Background(), "docker compose up", models.SetupOptions{CommandType: string(utils.DockerCompose)}); err == nil {
		t.Fatal("Setup went past the missing perf_event_paranoid; the row stops it there")
	}
	if got := conf.AgentURI(); got != "http://localhost:41234/agent" {
		t.Fatalf("AgentURI() = %q after the Setup; want the address it chose, http://localhost:41234/agent", got)
	}
	if got := conf.Agent.AgentURI; got != "http://localhost:41234/agent" {
		t.Fatalf("Agent.AgentURI = %q after the Setup; want its record of the address it chose", got)
	}
}
