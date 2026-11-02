package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.uber.org/zap"
)

// fakeRemovals is the engine's wait and inspect: the test answers the wait
// itself, on its unbuffered channels, so an answer is taken only by a Wait
// that is waiting for it.
type fakeRemovals struct {
	said      chan container.WaitResponse
	failed    chan error
	asked     []container.WaitCondition
	askedCtx  context.Context
	inspect   func() (container.InspectResponse, error)
	inspected int
}

func newFakeRemovals(inspect func() (container.InspectResponse, error)) *fakeRemovals {
	return &fakeRemovals{said: make(chan container.WaitResponse), failed: make(chan error), inspect: inspect}
}

func (f *fakeRemovals) ContainerWait(ctx context.Context, _ string, cond container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	f.asked, f.askedCtx = append(f.asked, cond), ctx
	return f.said, f.failed
}

func (f *fakeRemovals) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	f.inspected++
	return f.inspect()
}

func inspectState(st *container.State) func() (container.InspectResponse, error) {
	return func() (container.InspectResponse, error) {
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{State: st}}, nil
	}
}

func goneByInspect() (container.InspectResponse, error) {
	return container.InspectResponse{}, errdefs.NotFound(errors.New("no such container"))
}

func undescribed() (container.InspectResponse, error) { return container.InspectResponse{}, nil }

// waitInBackground runs w.Wait and closes the channel it returns once Wait
// returns.
func waitInBackground(ctx context.Context, w *RemovalWatch) <-chan struct{} {
	returned := make(chan struct{})
	go func() { w.Wait(ctx, zap.NewNop()); close(returned) }()
	return returned
}

// A stopped container's removal is waited for until the engine says it is
// done (or that the container is gone): the engine frees the container's
// volumes only then, and a Wait that returned before let the run's CA volume
// removal be refused as in use. The wait asked is for the removal, and it is
// asked by Watch, before the stop, not by Wait.
func TestRemovalWatchWaitsForTheEnginesWord(t *testing.T) {
	for name, inspect := range map[string]func() (container.InspectResponse, error){
		"exited":             inspectState(&container.State{Status: container.StateExited}),
		"removing":           inspectState(&container.State{Status: container.StateRemoving}),
		"gone from its list": goneByInspect,
		"undescribed":        undescribed,
		"no state":           inspectState(nil),
	} {
		for _, answer := range []string{"removed", "not found"} {
			f := newFakeRemovals(inspect)
			w := NewRemovalWatch(f, "keploy-v3-0a1b2c3d4e5f6071")
			w.Watch()
			if len(f.asked) != 1 || f.asked[0] != container.WaitConditionRemoved {
				t.Fatalf("%s: Watch asked the engine for %v, want one wait for the removal", name, f.asked)
			}
			returned := waitInBackground(context.Background(), w)
			var send func() bool
			if answer == "removed" {
				send = func() bool {
					select {
					case f.said <- container.WaitResponse{}:
						return true
					case <-returned:
						return false
					}
				}
			} else {
				send = func() bool {
					select {
					case f.failed <- errdefs.NotFound(errors.New("no such container")):
						return true
					case <-returned:
						return false
					}
				}
			}
			if !send() {
				t.Fatalf("%s: Wait returned before the engine said %s", name, answer)
			}
			<-returned
			if len(f.asked) != 1 {
				t.Errorf("%s: the engine was asked %v, want the one wait Watch asked", name, f.asked)
			}
			if f.askedCtx.Err() == nil {
				t.Errorf("%s/%s: the engine's wait outlived Wait", name, answer)
			}
		}
	}
}

// A container that still runs once the stop returned is one the stop did not
// reach: nothing removes it, so Wait does not wait for it.
func TestRemovalWatchDoesNotWaitForARunningContainer(t *testing.T) {
	f := newFakeRemovals(inspectState(&container.State{Status: container.StateRunning, Running: true}))
	w := NewRemovalWatch(f, "keploy-v3-0a1b2c3d4e5f6071")
	w.Watch()
	returned := waitInBackground(context.Background(), w)
	select {
	case <-returned:
	case f.said <- container.WaitResponse{}:
		t.Fatal("Wait waited for the removal of a container that still runs")
	}
	if f.askedCtx.Err() == nil {
		t.Error("the engine's wait outlived Wait")
	}
}

// With no Watch -- the container ended on its own, and its `docker run`
// client, which waits for the removal itself, returned after it -- Wait
// returns at once, and asks the engine nothing.
func TestRemovalWatchUnaskedReturnsAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFakeRemovals(func() (container.InspectResponse, error) {
		cancel() // a Wait that asks would otherwise wait for ever
		return container.InspectResponse{}, nil
	})
	NewRemovalWatch(f, "keploy-v3-0a1b2c3d4e5f6071").Wait(ctx, zap.NewNop())
	if f.inspected != 0 || len(f.asked) != 0 {
		t.Fatalf("an unasked Wait asked the engine (inspected %d, waits %v)", f.inspected, f.asked)
	}
}

// Wait ends with its context (the runnable bound keploy gives it), and ends
// the engine's wait with it.
func TestRemovalWatchEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := newFakeRemovals(goneByInspect)
	w := NewRemovalWatch(f, "keploy-v3-0a1b2c3d4e5f6071")
	w.Watch()
	cancel()
	<-waitInBackground(ctx, w)
	if f.askedCtx.Err() == nil {
		t.Error("the engine's wait outlived Wait")
	}
}
