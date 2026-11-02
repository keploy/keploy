//go:build linux

package http

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// A docker-run run's end is not held by a daemon that goes silent once its
// agent's container is stopped. The run ends by waiting for the engine's word
// that the agent's container is removed, and then removes the run's volumes:
// both are questions to the daemon, asked within keploy's bound on its answer
// (daemonAsk). Here the daemon takes the stop's question of the removal and
// answers the stop's signal and wait, then answers nothing: not a look at the
// container, not the removal it was asked to say, not a volume's removal. Once
// keploy has had daemonAnswerStall of its own runnable time, the bound ends
// the end's questions, and the run ends.
//
// The row drives startAgent itself, through the `docker run` it executes, to
// the stop and its end.
func TestADockerRunsEndIsNotHeldByADaemonThatGoesSilent(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "running")
	// The agent's container runs until the stop kills its client (or the
	// test is done with the engine); it exits at the stop's SIGTERM.
	fakeEngineForStartInDocker(t, dir, `#!/bin/sh
if [ "$1 $2" = "container run" ]; then
  : > "`+running+`"
  while [ -d "`+dir+`" ] && kill -0 "$PPID" 2>/dev/null; do sleep 0.05; done
fi
exit 0
`)
	clk := armsSaid{ManualClock: &stall.ManualClock{}, armed: make(chan struct{}, 64)}
	boundClock = clk
	t.Cleanup(func() { boundClock = nil })
	var asks atomic.Int32
	prev := daemonAsk
	daemonAsk = func() (context.Context, func()) {
		asks.Add(1)
		return prev()
	}
	t.Cleanup(func() { daemonAsk = prev })

	daemon := &goesSilent{taken: make(chan struct{}), looked: make(chan struct{}), lookEnded: make(chan error, 1), removed: make(chan string, 8)}
	a := &AgentClient{logger: zap.NewNop(), conf: &config.Config{InstallationID: "install-id"}, dockerClient: daemon}
	grp, gctx := errgroup.WithContext(context.Background())
	ctx, stop := context.WithCancel(context.WithValue(gctx, models.ErrGroupKey, grp))
	t.Cleanup(stop)
	name := kdocker.AgentContainerPrefix + "0a1b2c3d4e5f6071"
	exited, err := a.startAgent(ctx, true, models.SetupOptions{
		KeployContainer: name, Mode: models.MODE_RECORD, AgentPort: 43903, ProxyPort: 16792, DnsPort: 26792,
	})
	if err != nil {
		t.Fatalf("startAgent: %v", err)
	}
	// The stop comes once the agent's container runs: a stop before would
	// find no `docker run` to end.
	hung := time.After(hangBound)
	for {
		if _, err := os.Stat(running); err == nil {
			break
		}
		select {
		case <-hung:
			t.Fatalf("the agent's `docker run` never ran within %v", hangBound)
		case <-time.After(time.Millisecond):
		}
	}
	stop()
	within(t, "the stop's question of the removal", func() { <-daemon.taken })
	within(t, "the run's end's look at the agent's container", func() { <-daemon.looked })
	// Every bound asked so far, the end's last, has armed its first look at
	// the clock: none can miss the move.
	for range asks.Load() {
		within(t, "a bound's first look at keploy's clock", func() { <-clk.armed })
	}
	clk.Advance(daemonAnswerStall)
	within(t, "the docker run's end", func() {
		if err := <-exited; err != nil {
			t.Errorf("the docker run ended with %v", err)
		}
	})
	// The end waited for the removal's word until keploy's bound ended it.
	if cause := <-daemon.lookEnded; !errors.Is(cause, errDaemonSilent) {
		t.Errorf("the end's look at the agent's container ended with %v; want keploy's bound on the daemon's answer (%v)", cause, errDaemonSilent)
	}
	within(t, "the run's group", func() {
		if err := grp.Wait(); err != nil {
			t.Errorf("the run's group: %v", err)
		}
	})
	select {
	case got := <-daemon.removed:
		if want := kdocker.TLSVolumeFor(name); got != want {
			t.Errorf("the run's end asked to remove %q; want its CA volume %q", got, want)
		}
	default:
		t.Error("the run's end never asked to remove its CA volume")
	}
}

// armsSaid is a ManualClock that says each look at it a bound arms (After).
// A bound arms its first as it is set, and no other before the clock moves.
type armsSaid struct {
	*stall.ManualClock
	armed chan struct{}
}

func (c armsSaid) After(d time.Duration) (<-chan time.Time, func() bool) {
	ch, stop := c.ManualClock.After(d)
	c.armed <- struct{}{}
	return ch, stop
}

// goesSilent is a daemon that takes a stop's question of the removal of the
// agent's container (ContainerWait) and then answers nothing: neither that
// question, nor a look at the container, nor a volume's removal. Each
// returns only once its context ends.
type goesSilent struct {
	kdocker.Client
	taken     chan struct{}
	looked    chan struct{}
	lookEnded chan error // why the look's context ended
	removed   chan string
}

func (d *goesSilent) ContainerWait(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	close(d.taken)
	return make(chan container.WaitResponse), make(chan error)
}

func (d *goesSilent) ContainerInspect(ctx context.Context, _ string) (container.InspectResponse, error) {
	close(d.looked)
	<-ctx.Done()
	d.lookEnded <- context.Cause(ctx)
	return container.InspectResponse{}, ctx.Err()
}

func (d *goesSilent) VolumeRemove(ctx context.Context, name string, _ bool) error {
	d.removed <- name
	<-ctx.Done()
	return ctx.Err()
}
