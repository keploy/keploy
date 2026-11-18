package app

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// projectDaemon is a compose daemon (abortDocker's containers, by project
// label) whose events stream as dockerd's does: it logs each container start
// it makes, stamped by its clock; a stream gets the logged events stamped at
// or after its since, then each one logged after it is subscribed, and only
// those its filters match (type, event, label and container, as dockerd
// reads them). onStop, by container ID, runs as keploy's stop of that
// container has taken it down (compose's up starting a service whose wait
// that exit met). breakFirst breaks the first stream it is asked for before
// it gives any event (a daemon restarting, a dropped connection); asked gets
// one value per stream asked for.
type projectDaemon struct {
	*abortDocker
	logMu      sync.Mutex
	logged     []events.Message
	subs       []projectSub
	asks       int
	asked      chan int
	breakFirst bool
	onStop     map[string]func()
}

type projectSub struct {
	ch      chan events.Message
	filters filters.Args
}

func newProjectDaemon(started time.Time) *projectDaemon {
	return &projectDaemon{abortDocker: newAbortDocker(started), asked: make(chan int, 16), onStop: map[string]func(){}}
}

// matches reports whether f, a stream's filters, takes m, as dockerd's
// events filter does: one of the type's and one of the event's values, every
// label (key=value, or a key the actor has), one of the containers (by ID or
// name). A filter it does not know takes nothing, so a row cannot pass on one
// it does not read.
func (d *projectDaemon) matches(f filters.Args, m events.Message) bool {
	for _, key := range f.Keys() {
		vals := f.Get(key)
		switch key {
		case "type":
			if !slices.Contains(vals, string(m.Type)) {
				return false
			}
		case "event":
			if !slices.Contains(vals, string(m.Action)) {
				return false
			}
		case "label":
			for _, l := range vals {
				k, v, kv := strings.Cut(l, "=")
				got, has := m.Actor.Attributes[k]
				if !has || (kv && got != v) {
					return false
				}
			}
		case "container":
			if !slices.Contains(vals, m.Actor.ID) && !slices.Contains(vals, m.Actor.Attributes["name"]) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (d *projectDaemon) Events(ctx context.Context, o events.ListOptions) (<-chan events.Message, <-chan error) {
	msgs, errs := make(chan events.Message, 64), make(chan error, 1)
	d.logMu.Lock()
	d.asks++
	n := d.asks
	if n == 1 && d.breakFirst {
		d.logMu.Unlock()
		errs <- io.ErrUnexpectedEOF
		d.asked <- n
		return msgs, errs
	}
	since := sinceNanos(o.Since)
	for _, m := range d.logged {
		if m.TimeNano >= since && d.matches(o.Filters, m) {
			msgs <- m
		}
	}
	d.subs = append(d.subs, projectSub{ch: msgs, filters: o.Filters})
	d.logMu.Unlock()
	go func() {
		<-ctx.Done()
		errs <- ctx.Err()
	}()
	d.asked <- n
	return msgs, errs
}

// start makes container id of service in project and starts it, logging its
// start as dockerd does, with the container's labels as its attributes.
func (d *projectDaemon) start(id, service, project string) {
	c := d.add(id, service, runningState(time.Now()))
	d.mu.Lock()
	c.labels[composeProjectLabelKey] = project
	attrs := map[string]string{"name": id}
	for k, v := range c.labels {
		attrs[k] = v
	}
	d.mu.Unlock()
	m := events.Message{Type: events.ContainerEventType, Action: events.ActionStart, TimeNano: time.Now().UnixNano(),
		Actor: events.Actor{ID: id, Attributes: attrs}}
	d.logMu.Lock()
	defer d.logMu.Unlock()
	d.logged = append(d.logged, m)
	for _, s := range d.subs {
		if d.matches(s.filters, m) {
			s.ch <- m
		}
	}
}

func (d *projectDaemon) ContainerStop(ctx context.Context, ref string, o container.StopOptions) error {
	err := d.abortDocker.ContainerStop(ctx, ref, o)
	if f := d.onStop[ref]; f != nil {
		f()
	}
	return err
}

// stopAsked reports whether keploy asked the daemon to stop container id.
func (d *projectDaemon) stopAsked(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Contains(d.stopAsks, id)
}

func projectDaemonApp(d *projectDaemon) *App {
	a := newAbortApp(d.abortDocker, "docker compose up --exit-code-from ender", nil)
	a.docker = d
	return a
}

// KEPLOY'S STOP OF THE STACK WATCHES FOR LATE STARTERS BEFORE IT LISTS. The
// shape of the hang it ends: the stop's SIGTERM makes holder exit 0 at once,
// and compose's up starts late (waiting on holder's completion) while the
// stop still waits on the other listed containers' graces. Here the daemon
// starts late as holder's stop ends, inside the listed stops, and its events
// stream from a since as dockerd's do: a watch begun after the listed stops
// reads from after late's start and never stops it, and the up, waiting on
// late, never returned. late is stopped, and the stops are said ended only
// once the up has returned. Both KEPLOY_PROVEN modes.
func TestKeploysStopOfTheStackStopsALateStarterThatStartsDuringItsListedStops(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			d := newProjectDaemon(time.Now().Add(-time.Minute))
			d.add("holder-id", "holder", runningState(time.Now().Add(-time.Minute)))
			d.onStop["holder-id"] = func() { d.start("late-id", "late", "proj") }
			a := projectDaemonApp(d)
			returned := a.composeUpRunning()
			defer returned()
			stopped, late := a.stopComposeStack(context.Background())
			if !stopped || !d.stopAsked("holder-id") {
				t.Fatalf("stopped %v, holder's stop asked %v; want the listed holder stopped", stopped, d.stopAsked("holder-id"))
			}
			eventually(t, "late, started during the listed stops, is stopped", func() bool { return d.stopAsked("late-id") })
			if isClosed(late, 200*time.Millisecond) {
				t.Fatal("the late starters' stops were said ended with the up running")
			}
			returned()
			if !isClosed(late, time.Minute) {
				t.Fatal("the late starters' stops were not said ended once the up returned")
			}
		})
	}
}

// keploy's stop of the stack stops the late starters of the app's compose
// project only: the daemon's start events of every project reach a stream
// that does not filter by the project, and another project's container that
// starts while the up runs (a second CI job's stack on one runner) is not
// keploy's to stop. Both KEPLOY_PROVEN modes.
func TestKeploysStopOfTheStackStopsNoLateStarterOfAnotherProject(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			d := newProjectDaemon(time.Now().Add(-time.Minute))
			a := projectDaemonApp(d)
			returned := a.composeUpRunning()
			defer returned()
			stopped, late := a.stopComposeStack(context.Background())
			if !stopped {
				t.Fatal("the stack's stop could not tell the project")
			}
			d.start("other-id", "app", "otherproj")
			d.start("late-id", "late", "proj")
			eventually(t, "this project's late starter is stopped", func() bool { return d.stopAsked("late-id") })
			returned()
			if !isClosed(late, time.Minute) {
				t.Fatal("the late starters' stops were not said ended once the up returned")
			}
			if d.stopAsked("other-id") {
				t.Fatal("keploy stopped another compose project's container that started while its up ran; want only its own project's late starters stopped")
			}
		})
	}
}

// keploy's stop of the stack asks the daemon again for the project's starts
// when the stream breaks (a daemon restarting, a dropped connection), and a
// late starter on the stream asked for again is stopped: a watch that ended
// with the stream left it running, and the up, so the run, hung. Both
// KEPLOY_PROVEN modes.
func TestKeploysStopOfTheStackStopsALateStarterOnceItsStreamBrokeAndWasAskedAgain(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			d := newProjectDaemon(time.Now().Add(-time.Minute))
			d.breakFirst = true
			a := projectDaemonApp(d)
			returned := a.composeUpRunning()
			defer returned()
			if stopped, _ := a.stopComposeStack(context.Background()); !stopped {
				t.Fatal("the stack's stop could not tell the project")
			}
			for want := 1; want <= 2; want++ {
				select {
				case n := <-d.asked:
					if n != want {
						t.Fatalf("stream %d asked for; want %d", n, want)
					}
				case <-time.After(30 * time.Second):
					t.Fatalf("stream %d was not asked for (the first broke): the watch of late starters ended with its stream", want)
				}
			}
			d.start("late-id", "late", "proj")
			eventually(t, "the late starter on the stream asked for again is stopped", func() bool { return d.stopAsked("late-id") })
		})
	}
}
