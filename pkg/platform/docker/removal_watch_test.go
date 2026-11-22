package docker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
			w.Watch(context.Background())
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
	w.Watch(context.Background())
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
	w.Watch(context.Background())
	cancel()
	<-waitInBackground(ctx, w)
	if f.askedCtx.Err() == nil {
		t.Error("the engine's wait outlived Wait")
	}
}

// silentDaemon is an engine whose daemon does not take a wait's question:
// ContainerWait returns only once its context ends, as the engine's client
// does, with that end on its error channel.
type silentDaemon struct{ asked chan struct{} }

func (d *silentDaemon) ContainerWait(ctx context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	close(d.asked)
	<-ctx.Done()
	failed := make(chan error, 1)
	failed <- ctx.Err()
	return make(chan container.WaitResponse), failed
}

func (d *silentDaemon) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return container.InspectResponse{}, nil
}

// A daemon that does not take the question holds Watch only until its ask
// ends: the question is withdrawn, so the stop goes on, and Wait finds the
// engine's wait ended and returns at once.
func TestRemovalWatchWithdrawsAnUntakenQuestionWhenItsAskEnds(t *testing.T) {
	d := &silentDaemon{asked: make(chan struct{})}
	w := NewRemovalWatch(d, "keploy-v3-0a1b2c3d4e5f6071")
	ask, end := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() { w.Watch(ask); close(returned) }()
	within(t, "the engine's being asked", d.asked)
	end()
	within(t, "Watch, once its ask ended", returned)
	within(t, "Wait, after a withdrawn question", waitInBackground(context.Background(), w))
}

// A question the engine has taken outlives Watch's ask: the ask bounds only
// the engine's taking it, and Wait needs the engine's answer after the stop.
func TestRemovalWatchKeepsATakenQuestionPastItsAsk(t *testing.T) {
	f := newFakeRemovals(goneByInspect)
	w := NewRemovalWatch(f, "keploy-v3-0a1b2c3d4e5f6071")
	ask := newEndingAsk()
	w.Watch(ask)
	if ask.registered() == 0 {
		t.Fatal("Watch registered nothing on its ask: the check below would hold of any Watch")
	}
	if f.askedCtx.Err() != nil {
		t.Fatal("the engine's wait ended before Watch's ask did")
	}
	if n := ask.end(); n != 0 {
		t.Fatalf("the end of Watch's ask withdrew the question the engine had taken (%d still registered)", n)
	}
}

// hangBound turns a regression that would hang a test into a failure that
// names what never came. It is part of no assertion.
const hangBound = 2 * time.Minute

// within fails the test, naming what, unless done is closed within hangBound.
func within(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(hangBound):
		t.Fatalf("%s did not come within %v", what, hangBound)
	}
}

// endingAsk is an ask the test ends (end). context.AfterFunc registers on it
// through its AfterFunc method, and end runs what is still registered on the
// test's goroutine and says how many: whether the ask's end withdrew
// anything is known as it ends, not by waiting for the withdrawal's goroutine.
type endingAsk struct {
	done chan struct{}
	mu   sync.Mutex
	fns  map[int]func()
	next int
}

func newEndingAsk() *endingAsk {
	return &endingAsk{done: make(chan struct{}), fns: map[int]func(){}}
}

func (a *endingAsk) Deadline() (time.Time, bool) { return time.Time{}, false }
func (a *endingAsk) Done() <-chan struct{}       { return a.done }
func (a *endingAsk) Value(any) any               { return nil }

func (a *endingAsk) Err() error {
	select {
	case <-a.done:
		return context.Canceled
	default:
		return nil
	}
}

// AfterFunc registers f, to run at end unless stopped first.
func (a *endingAsk) AfterFunc(f func()) func() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := a.next
	a.next++
	a.fns[id] = f
	return func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.fns[id]
		delete(a.fns, id)
		return ok
	}
}

// registered is how many funcs were ever registered.
func (a *endingAsk) registered() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.next
}

// end ends the ask, runs what is still registered, and returns how many.
func (a *endingAsk) end() int {
	a.mu.Lock()
	fns := a.fns
	a.fns = map[int]func(){}
	a.mu.Unlock()
	close(a.done)
	for _, f := range fns {
		f()
	}
	return len(fns)
}
