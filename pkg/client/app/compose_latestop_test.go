package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// heldStopDocker holds keploy's stop of a container in held until its
// channel is closed (its main process outlasting the stop's grace), saying
// on asked that the stop was asked; listFails fails the project's listing.
type heldStopDocker struct {
	*abortDocker
	held      map[string]chan struct{}
	asked     chan string
	listFails bool
}

func (d *heldStopDocker) ContainerStop(ctx context.Context, ref string, o container.StopOptions) error {
	if ch := d.held[ref]; ch != nil {
		d.asked <- ref
		<-ch
	}
	return d.abortDocker.ContainerStop(ctx, ref, o)
}

func (d *heldStopDocker) ContainerList(ctx context.Context, o container.ListOptions) ([]container.Summary, error) {
	if d.listFails {
		return nil, errdefs.NotFound(errors.New("the daemon will not list them"))
	}
	return d.abortDocker.ContainerList(ctx, o)
}

// isClosed reports whether ch is closed within wait.
func isClosed(ch <-chan struct{}, wait time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(wait):
		return false
	}
}

// keploy's stop of the stack says when the stops of its late starters have
// ended: at once when no up runs, or when it cannot tell the project; with an
// up running, only once the up has returned and a late starter's stop still
// in its grace has ended, whether or not the project's listing answered.
// Both KEPLOY_PROVEN modes.
func TestKeploysStopOfTheStackSaysWhenItsLateStartersStopsHaveEnded(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			newApp := func(listFails bool) (*App, *heldStopDocker) {
				d := &heldStopDocker{abortDocker: newAbortDocker(time.Now().Add(-time.Minute)),
					held: map[string]chan struct{}{}, asked: make(chan string, 1), listFails: listFails}
				a := newAbortApp(d.abortDocker, "docker compose up --exit-code-from runner", nil)
				a.docker = d
				return a, d
			}

			a, _ := newApp(false)
			stopped, late := a.stopComposeStack(context.Background())
			if !stopped || !isClosed(late, time.Second) {
				t.Fatalf("no up running: stopped %v, late starters' stops ended %v; want both", stopped, isClosed(late, 0))
			}

			a, _ = newApp(false)
			a.keployContainer = "no-such-agent"
			returned := a.composeUpRunning()
			stopped, late = a.stopComposeStack(context.Background())
			if stopped || !isClosed(late, time.Second) {
				t.Fatalf("a project keploy cannot tell: stopped %v, late starters' stops ended %v; want not stopped, and nothing to wait for", stopped, isClosed(late, 0))
			}
			returned()

			for _, listFails := range []bool{false, true} {
				a, d := newApp(listFails)
				returned := a.composeUpRunning()
				stopped, late := a.stopComposeStack(context.Background())
				if stopped == listFails {
					t.Fatalf("listing fails %v: stopped %v", listFails, stopped)
				}
				var stream chan events.Message
				select {
				case stream = <-d.streams:
				case <-time.After(time.Minute):
					t.Fatalf("listing fails %v: keploy's stop did not watch the project's starts", listFails)
				}
				release := make(chan struct{})
				d.held["runner-id"] = release
				d.add("runner-id", "runner", runningState(time.Now()))
				stream <- events.Message{Type: events.ContainerEventType, Action: events.ActionStart, TimeNano: time.Now().UnixNano(),
					Actor: events.Actor{ID: "runner-id", Attributes: map[string]string{composeProjectLabelKey: "proj", composeServiceLabelKey: "runner"}}}
				select {
				case <-d.asked:
				case <-time.After(time.Minute):
					t.Fatalf("listing fails %v: the late starter's stop was not asked", listFails)
				}
				if isClosed(late, 200*time.Millisecond) {
					t.Fatalf("listing fails %v: the late starters' stops were said ended with the up running", listFails)
				}
				returned()
				if isClosed(late, 200*time.Millisecond) {
					t.Fatalf("listing fails %v: the late starters' stops were said ended with the late starter still in its stop's grace", listFails)
				}
				close(release)
				if !isClosed(late, time.Minute) {
					t.Fatalf("listing fails %v: the late starters' stops were not said ended once the up returned and the late starter's stop ended", listFails)
				}
			}
		})
	}
}
