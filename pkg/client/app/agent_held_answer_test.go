package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// errHeldCALatched is the agent's answer to its release once its CA setup has
// failed, which it latches until it is started again: no later ask changes it.
var errHeldCALatched = errors.New("the keploy agent cannot release the application: CA setup failed: open /tmp/keploy-tls/ca.crt: read-only file system")

// heldAnswerRun is newSetupFailApp whose agent client also makes, on the
// agent the retry's watch set up, the release the hold holds (held, given the
// release's number from 1), noting "release" each time it is made, as the
// agent client does once current says that agent still runs.
type heldAnswerRun struct {
	*setupFailApp
	calls atomic.Int32
	// atUpEnd is the trail as the retry's up returned.
	atUpEnd string
}

func newHeldAnswerRun(t *testing.T, mode models.Mode, setUp func(context.Context) error, held func(r *heldAnswerRun, n int32) error) *heldAnswerRun {
	t.Helper()
	s := newSetupFailApp(t, setUp)
	s.a.Mode = mode
	r := &heldAnswerRun{setupFailApp: s}
	s.a.SetAgentHeldRelease(func(ctx context.Context, current func(context.Context) bool) error {
		if !current(ctx) {
			return nil
		}
		n := r.calls.Add(1)
		s.note("release")
		return held(r, n)
	})
	return r
}

// run is the retry of a bring-up whose stack keploy stopped: no agent runs
// before it, and its up starts keploy's agent again (agent-1 at t3), then
// waits, for at most 10 s, until the run ends, the app's container runs, or
// the retry's watch has ended and let its hold go, keeping the trail as it
// returns (atUpEnd). ended: the run ended.
func (r *heldAnswerRun) run(t *testing.T) (ended bool) {
	t.Helper()
	r.d.set("keploy-v3-test", "agent-1", "t1", false)
	r.d.set("app", "app-1", "", false)
	r.a.retryComposeUp(context.Background(), r.end, func() utils.CmdError {
		defer func() { r.atUpEnd = r.trail() }()
		r.note("up")
		r.d.set("keploy-v3-test", "agent-1", "t3", true)
		deadline := time.After(10 * time.Second)
		for {
			select {
			case <-r.ended:
				return utils.CmdError{}
			case <-deadline:
				return utils.CmdError{}
			case <-time.After(10 * time.Millisecond):
			}
			if r.a.appStarted(context.Background()) || strings.HasSuffix(r.trail(), ",resume") {
				return utils.CmdError{}
			}
		}
	})
	select {
	case <-r.ended:
		return true
	default:
		return false
	}
}

// A HELD RELEASE THE AGENT REFUSES FOR GOOD ENDS THE RUN AT ONCE. The agent
// compose's retry started again is set up, and the release the hold holds,
// made on it, gets the agent's latched CA setup failure. No later ask
// changes that answer, so the run ends on it at once, as a setup whose own
// release got it does: the halt names the agent, the failure and the next
// step, the releases the run asked for end with it, and the agent is asked
// once, never again each look until compose's healthcheck gives up. KEPLOY_PROVEN
// unset and =1, record and test.
func TestAHeldReleaseAnswerCALatchedEndsTheRunAtOnce(t *testing.T) {
	everyRunMode(t, func(t *testing.T, mode models.Mode) {
		r := newHeldAnswerRun(t, mode, func(context.Context) error { return nil },
			func(*heldAnswerRun, int32) error { return errHeldCALatched })
		if !r.run(t) {
			t.Fatalf("the run did not end within 10 s of the held release's latched CA failure (asked %d times; retry did %s)", r.calls.Load(), r.trail())
		}
		if g := r.trail(); !strings.HasPrefix(g, "hold,up,setup,release,resume-failed,end") || strings.Count(g, "release") != 1 {
			t.Fatalf("the retry did %s; want one held release, the releases ended with its failure, then the run's end", g)
		}
		if n := r.calls.Load(); n != 1 {
			t.Fatalf("the held release was made %d times; want once: the agent's answer for good is not asked again", n)
		}
		if len(r.resume) == 0 || !errors.Is(r.resume[0], errHeldCALatched) {
			t.Fatalf("the hold was let go with %v; want the releases ended with the latched CA failure", r.resume)
		}
		h := r.a.composeAppHalt()
		if h == nil || !errors.Is(h.agentSetup, errHeldCALatched) {
			t.Fatalf("halt %+v; want the held release's latched CA failure", h)
		}
		msg := h.Error()
		for _, want := range []string{`keploy's agent container "keploy-v3-test"`, "CA setup failed: open /tmp/keploy-tls/ca.crt", "keploy does not release the application to it",
			"Next step: read keploy's log above for why the setup failed"} {
			if !strings.Contains(msg, want) {
				t.Fatalf("the halt says %q; want it to say %q", msg, want)
			}
		}
	})
}

// A HELD RELEASE ANSWERED WITH ANOTHER PROVEN SWITCH ENDS THE RUN AS A SETUP
// ANSWERED SO DOES. The agent's proven switch is not this keploy's: the
// setup whose release reads it, and the held release made on the agent that
// setup completed on, end the run alike, with the same halt, the same end
// and the releases ended with the switch's error. KEPLOY_PROVEN unset and
// =1, record and test.
func TestAHeldReleaseAnswerOfAnotherSwitchEndsTheRunAsTheSetupDoes(t *testing.T) {
	everyRunMode(t, func(t *testing.T, mode models.Mode) {
		other := fmt.Errorf("%w: the switch is on in this keploy (%s) and its agent reports none, so the agent predates it; run the agent of this keploy version",
			provenswitch.ErrAgentSwitch, provenswitch.Env)
		setup := newHeldAnswerRun(t, mode, func(context.Context) error { return other },
			func(*heldAnswerRun, int32) error { return nil })
		held := newHeldAnswerRun(t, mode, func(context.Context) error { return nil },
			func(*heldAnswerRun, int32) error { return other })
		for _, c := range []struct {
			name, trail string
			r           *heldAnswerRun
		}{{"the setup", "hold,up,setup,resume-failed,end", setup}, {"the held release", "hold,up,setup,release,resume-failed,end", held}} {
			if !c.r.run(t) {
				t.Fatalf("%s answered with another proven switch, and the run did not end within 10 s (retry did %s)", c.name, c.r.trail())
			}
			if g := c.r.trail(); !strings.HasPrefix(g, c.trail) {
				t.Fatalf("%s answered with another proven switch, and the retry did %s; want %s", c.name, g, c.trail)
			}
			if len(c.r.resume) == 0 || !errors.Is(c.r.resume[0], provenswitch.ErrAgentSwitch) {
				t.Fatalf("%s answered with another proven switch, and the hold was let go with %v; want the switch's error", c.name, c.r.resume)
			}
			if h := c.r.a.composeAppHalt(); h == nil || !errors.Is(h.agentSetup, provenswitch.ErrAgentSwitch) {
				t.Fatalf("%s answered with another proven switch, and the halt is %+v; want the switch's error", c.name, h)
			}
		}
		hs, hh := setup.a.composeAppHalt(), held.a.composeAppHalt()
		if hs.Error() != hh.Error() {
			t.Fatalf("the held release ends the run on %q; the setup on %q: want the same halt", hh.Error(), hs.Error())
		}
		if es, eh := hs.appError(""), hh.appError(""); es.AppErrorType != eh.AppErrorType || es.ExitCode != eh.ExitCode || !errors.Is(eh.Err, models.ErrAppExitedOnItsOwn) {
			t.Fatalf("the held release ends the run %+v; the setup %+v: want the same end", eh, es)
		}
	})
}

// A HELD RELEASE CUT BY COMPOSE RECREATING THE AGENT IS MADE AGAIN ON THE ONE
// IN ITS PLACE. The release made on the agent the watch set up ends unanswered
// as compose recreates that agent: no answer of the agent's, and no halt.
// The watch's next look sets up the recreated agent and makes the release on
// it; the app then starts, and nothing ended the run. KEPLOY_PROVEN unset
// and =1, record and test.
func TestAHeldReleaseAnswerCutByARecreatedAgentIsMadeAgainOnTheNext(t *testing.T) {
	everyRunMode(t, func(t *testing.T, mode models.Mode) {
		r := newHeldAnswerRun(t, mode, func(context.Context) error { return nil }, func(r *heldAnswerRun, n int32) error {
			if n == 1 {
				r.d.set("keploy-v3-test", "agent-2", "t4", true)
				return errors.New("the keploy agent did not take its release of the application (/agent/ready): its container exited")
			}
			r.d.set("app", "app-1", "t5", true)
			return nil
		})
		if r.run(t) {
			t.Fatalf("a held release cut by compose recreating the agent ended the run (halt %+v)", r.a.composeAppHalt())
		}
		if g := r.trail(); g != "hold,up,setup,release,setup,release,resume" {
			t.Fatalf("the retry did %s; want the recreated agent set up and released, then the hold let go", g)
		}
		if h := r.a.composeAppHalt(); h != nil {
			t.Fatalf("halt %+v kept for a held release that compose's recreate cut; want none", h)
		}
		if !r.a.appStarted(context.Background()) {
			t.Fatal("the app never started behind the recreated agent's release")
		}
	})
}

// A HELD RELEASE THAT FAILS WHILE KEPLOY'S OWN STOP IS UNDER WAY KEEPS NO
// HALT. keploy stops the stack as the release made on the agent it set up
// ends: that stop took the agent, and the release's failure ends nothing.
// The watch ends at once and lets its hold go with nil, before the retry's
// up returns: a watch that went on looking would let it go only once the up
// returned. KEPLOY_PROVEN unset and =1, record and test.
func TestAHeldReleaseAnswerWhileKeploysStopIsUnderWayEndsNothing(t *testing.T) {
	everyRunMode(t, func(t *testing.T, mode models.Mode) {
		r := newHeldAnswerRun(t, mode, func(context.Context) error { return nil }, func(r *heldAnswerRun, _ int32) error {
			r.a.composeApp.agentStopping.Store(true)
			return errHeldCALatched
		})
		if r.run(t) {
			t.Fatalf("a held release that failed under keploy's own stop ended the run (halt %+v)", r.a.composeAppHalt())
		}
		if g := r.trail(); g != "hold,up,setup,release,resume" {
			t.Fatalf("the retry did %s; want the hold let go with nil once keploy's stop cut the held release", g)
		}
		if g := r.atUpEnd; g != "hold,up,setup,release,resume" {
			t.Fatalf("the retry's up returned after %s; want the watch ended, its hold let go with nil, before the up returned", g)
		}
		if h := r.a.composeAppHalt(); h != nil {
			t.Fatalf("halt %+v kept for a held release keploy's own stop cut; want none", h)
		}
	})
}
