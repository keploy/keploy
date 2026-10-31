package http

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// flakyNameDocker is a liveNameDocker whose next inspect fails once failNext
// is set: a Docker daemon that drops one call on a loaded runner.
type flakyNameDocker struct {
	liveNameDocker
	fmu      sync.Mutex
	failNext bool
}

func (d *flakyNameDocker) fail() error {
	d.fmu.Lock()
	defer d.fmu.Unlock()
	if !d.failNext {
		return nil
	}
	d.failNext = false
	return errors.New("riso: the daemon dropped this inspect")
}

func (d *flakyNameDocker) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	if err := d.fail(); err != nil {
		return container.InspectResponse{}, err
	}
	return d.liveNameDocker.ContainerInspect(ctx, id)
}

func (d *flakyNameDocker) NetworkInspect(ctx context.Context, id string, o network.InspectOptions) (network.Inspect, error) {
	if err := d.fail(); err != nil {
		return network.Inspect{}, err
	}
	return d.liveNameDocker.NetworkInspect(ctx, id, o)
}

// A list the CLI could not give for an ask (one Docker inspect failed) settles
// that ask: the CLI's next stream of asks opens after it, so the DNS miss
// waiting for it is answered by keploy at once, and the CLI does not take the
// same ask again (a daemon that keeps failing would otherwise have the CLI
// reopen, fail and reopen as fast as the round trips go). The CLI keeps
// answering: a container started later is still found on a miss, not left to
// keploy's answer for the rest of the test set.
func TestAListTheCLICouldNotGiveLeavesLaterAsksAnswered(t *testing.T) {
	ag, uri := newNameAgent(t)
	t.Cleanup(func() { forgetNetworkNames(uri) })
	docker := &flakyNameDocker{}
	docker.set(stackNames("db"))
	a := New(zap.NewNop(), docker, &config.Config{
		KeployContainer: "keploy-v3-agent",
		CommandType:     string(utils.DockerCompose),
		Agent:           config.Agent{AgentURI: uri},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a.pushNetworkNames(ctx)
	store, _ := ag.current()
	if !store.Given() || !store.Owns("db") {
		t.Fatal("the agent was given no list holding db")
	}
	if after := <-ag.opened; after != 0 {
		t.Fatalf("the CLI's first stream of asks opened after %d, want 0", after)
	}

	docker.set(stackNames("db", "cache"))
	docker.fmu.Lock()
	docker.failNext = true
	docker.fmu.Unlock()
	if answered(store, "cache") {
		t.Fatal("cache is owned though the CLI could not list the names for the miss's ask")
	}
	if after := <-ag.opened; after != 1 {
		t.Fatalf("the CLI's next stream of asks opened after %d, want 1: the ask it could not list the names for is taken again", after)
	}

	docker.set(stackNames("db", "cache", "late"))
	if !answered(store, "late") {
		t.Fatal("late, started after a list the CLI could not give, is not found on a miss: nothing answers the agent's later asks")
	}
}
