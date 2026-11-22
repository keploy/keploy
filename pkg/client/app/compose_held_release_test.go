package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

// heldReleaseApp is newRestartApp with the agent client's hold, setup and held
// release (SetAgentHeldRelease) noted in order, the retry's `up` among them.
// The held release asks current, as the agent client does right before its
// request goes, and notes "release" the first time current says the agent set
// up still runs; released is closed then. currents are current's answers.
type heldReleaseApp struct {
	a        *App
	d        *restartingDocker
	mu       sync.Mutex
	got      []string
	currents []bool
	released chan struct{}
	once     sync.Once
	// onHeld, when set, runs in the held release before it asks current.
	onHeld func(calls int)
}

func newHeldReleaseApp(t *testing.T, mode models.Mode) *heldReleaseApp {
	t.Helper()
	a, d, _ := newRestartApp(t)
	a.Mode = mode
	h := &heldReleaseApp{a: a, d: d, released: make(chan struct{})}
	a.SetOnAgentRestart(func(context.Context) error { h.note("setup"); return nil })
	a.SetAgentReleaseHold(func(context.Context) func(error) {
		h.note("hold")
		return func(error) { h.note("resume") }
	})
	a.SetAgentHeldRelease(func(ctx context.Context, current func(context.Context) bool) error {
		h.mu.Lock()
		calls := len(h.currents)
		h.mu.Unlock()
		if h.onHeld != nil {
			h.onHeld(calls)
		}
		ok := current(ctx)
		h.mu.Lock()
		h.currents = append(h.currents, ok)
		h.mu.Unlock()
		if ok {
			h.once.Do(func() {
				h.note("release")
				close(h.released)
			})
		}
		return nil
	})
	return h
}

func (h *heldReleaseApp) note(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, s)
}

func (h *heldReleaseApp) trail() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.got, ",")
}

func (h *heldReleaseApp) asked() []bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bool(nil), h.currents...)
}

// upBehindTheRelease is a retried `up` that starts the agent again (agent-1,
// started at t3) and starts the app's container only once the agent was
// released, as compose starts it behind the agent's healthcheck; with no
// release within 10 s it fails the row, as the run waited for good.
func (h *heldReleaseApp) upBehindTheRelease(t *testing.T) func() utils.CmdError {
	return func() utils.CmdError {
		h.note("up")
		h.d.set("keploy-v3-test", "agent-1", "t3", true)
		select {
		case <-h.released:
		case <-time.After(10 * time.Second):
			t.Error("no release went to the agent the retry set up: the app's container, behind the agent's healthcheck, never started, and the hold, kept until it starts, held the run's release")
			return utils.CmdError{}
		}
		h.d.set("app", "app-1", "t4", true)
		time.Sleep(100 * time.Millisecond)
		return utils.CmdError{}
	}
}

// THE RETRY MAKES THE RELEASE ITS HOLD HOLDS ON THE AGENT IT SET UP. keploy's
// stop of the stack for a dependency's crash took the agent before it
// answered, so the run had set nothing up and asked for no release; the
// retry's `up` started the agent again, the watch set it up (nothing to make
// again) and held the releases until the app's container starts, and the
// run's release, asked once the run had set the agent up itself, waited on
// that hold while the app waited on the release (CI's replay isolation job,
// record-compose-flaky: the run waited until it was stopped). The watch has
// the agent client make that release on the agent it set up, still running,
// and the app starts. Both KEPLOY_PROVEN modes, record and test.
func TestARetryMakesTheReleaseItsHoldHoldsOnTheAgentItSetUp(t *testing.T) {
	everyRunMode(t, func(t *testing.T, mode models.Mode) {
		h := newHeldReleaseApp(t, mode)
		h.d.set("keploy-v3-test", "agent-1", "t1", false) // keploy's stop of the stack took it
		h.d.set("app", "app-1", "", false)
		h.a.retryComposeUp(context.Background(), nil, h.upBehindTheRelease(t))
		if g := h.trail(); g != "hold,up,setup,release,resume" {
			t.Fatalf("the retry did %s; want the agent it started set up, released under the hold, and the hold let go once the app started", g)
		}
		if !h.a.agentKept("agent-1@t3") {
			t.Fatalf("the last agent set up is %q; want agent-1@t3", h.a.agentSetUp)
		}
	})
}

// The release goes to the agent set up only while it runs: current reads the
// agent's identity again, right before the request, and says no once compose
// put another in its place; that one is set up, and released, in turn. Both
// KEPLOY_PROVEN modes, record and test.
func TestARetrysHeldReleaseAsksWhetherTheAgentSetUpStillRuns(t *testing.T) {
	everyRunMode(t, func(t *testing.T, mode models.Mode) {
		h := newHeldReleaseApp(t, mode)
		h.onHeld = func(calls int) {
			if calls == 0 {
				h.d.set("keploy-v3-test", "agent-2", "t5", true) // compose replaced the agent set up
			}
		}
		h.d.set("keploy-v3-test", "agent-1", "t1", false)
		h.d.set("app", "app-1", "", false)
		h.a.retryComposeUp(context.Background(), nil, h.upBehindTheRelease(t))
		asked := h.asked()
		if len(asked) < 2 || asked[0] || !asked[len(asked)-1] {
			t.Fatalf("current answered %v; want no while agent-2 ran in place of agent-1, then yes for agent-2 once set up", asked)
		}
		if g := h.trail(); g != "hold,up,setup,setup,release,resume" {
			t.Fatalf("the retry did %s; want agent-1 and agent-2 set up, and only agent-2 released", g)
		}
		if !h.a.agentKept("agent-2@t5") {
			t.Fatalf("the last agent set up is %q; want agent-2@t5", h.a.agentSetUp)
		}
	})
}

// The watch makes no release for an agent it did not set up: one kept from
// before the retry (the run set it up, and its releases are not held), or
// none running at all. Both KEPLOY_PROVEN modes, record and test.
func TestARetryMakesNoHeldReleaseForAnAgentItDidNotSetUp(t *testing.T) {
	for _, shape := range []string{"kept", "none running"} {
		t.Run(shape, func(t *testing.T) {
			everyRunMode(t, func(t *testing.T, mode models.Mode) {
				h := newHeldReleaseApp(t, mode)
				h.d.set("keploy-v3-test", "agent-1", "t1", shape == "kept")
				h.d.set("app", "app-1", "", false)
				h.a.retryComposeUp(context.Background(), nil, func() utils.CmdError {
					h.note("up")
					time.Sleep(200 * time.Millisecond) // many looks of the watch
					if shape == "kept" {
						h.d.set("app", "app-1", "t2", true)
						time.Sleep(100 * time.Millisecond)
					}
					return utils.CmdError{}
				})
				if asked := h.asked(); len(asked) != 0 {
					t.Fatalf("the watch made the held release %d times for an agent it did not set up (trail %s)", len(asked), h.trail())
				}
			})
		})
	}
}

// Nor while keploy stops the stack (its stop for a crash under way, or a
// dependency's crash kept for the next bring-up): that stop takes the agent,
// and the next bring-up sets one up again. Both KEPLOY_PROVEN modes, record
// and test.
func TestARetryMakesNoHeldReleaseWhileKeployStopsTheStack(t *testing.T) {
	for _, stop := range []string{"agent stopping", "dependency crash kept"} {
		t.Run(stop, func(t *testing.T) {
			everyRunMode(t, func(t *testing.T, mode models.Mode) {
				h := newHeldReleaseApp(t, mode)
				h.a.SetOnAgentRestart(func(context.Context) error {
					h.note("setup")
					if stop == "agent stopping" {
						h.a.composeApp.agentStopping.Store(true)
					} else {
						h.a.composeApp.depCrash.Store(&composeHalt{exitCode: 1, service: "dep", serviceCode: 1, beforeApp: true})
					}
					return nil
				})
				h.d.set("keploy-v3-test", "agent-1", "t1", false)
				h.d.set("app", "app-1", "", false)
				h.a.retryComposeUp(context.Background(), nil, func() utils.CmdError {
					h.note("up")
					h.d.set("keploy-v3-test", "agent-1", "t3", true)
					time.Sleep(300 * time.Millisecond) // many looks of the watch after the setup
					return utils.CmdError{}
				})
				if g := h.trail(); !strings.Contains(g, "setup") {
					t.Fatalf("the retry did %s; want agent-1 set up", g)
				}
				if asked := h.asked(); len(asked) != 0 {
					t.Fatalf("the watch made the held release %d times while keploy stopped the stack (trail %s)", len(asked), h.trail())
				}
			})
		})
	}
}
