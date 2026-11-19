package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// everyRunMode runs row under KEPLOY_PROVEN unset and =1, as a recording and
// as a test run.
func everyRunMode(t *testing.T, row func(t *testing.T, mode models.Mode)) {
	for _, proven := range []string{"", "1"} {
		for _, mode := range []models.Mode{models.MODE_RECORD, models.MODE_TEST} {
			t.Run("switch="+proven+"/"+string(mode), func(t *testing.T) {
				t.Setenv(provenswitch.Env, proven)
				row(t, mode)
			})
		}
	}
}

// up is a retried `up` (up) that the trail notes.
func (s *setupFailApp) up(steps ...func()) func() utils.CmdError {
	return func() utils.CmdError {
		s.note("up")
		return up(steps...)()
	}
}

// awaitTrail waits, while the retry's up still runs, until the retry's trail
// holds want; false after 30 s.
func (s *setupFailApp) awaitTrail(want string) bool {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.trail(), want) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// A SETUP KEPLOY'S OWN STOP CUT IS NO FAILED SETUP. A dependency crashes
// again while the agent compose's retry started again is being set up:
// keploy decides that exit under stopMu (stopping, agentStopping, the crash
// kept for the next bring-up), then its stop of the stack takes the agent the
// setup talks to, and a setup call fails while the retried up still runs. The
// setup was cut by keploy's own stop: no halt is kept, the run's end is not
// called, the hold is let go with nil, and the run's loop takes the next
// bring-up, whose agent is set up again. Each of keploy's stop marks alone
// decides it. Record and test, both KEPLOY_PROVEN modes.
func TestASetupKeploysOwnStopCutKeepsNoHaltAndTheNextBringUpSetsTheAgentUp(t *testing.T) {
	for _, marks := range []string{"crash", "agentStopping", "depCrash"} {
		t.Run(marks, func(t *testing.T) {
			everyRunMode(t, func(t *testing.T, mode models.Mode) {
				var setups atomic.Int32
				var s *setupFailApp
				s = newSetupFailApp(t, func(context.Context) error {
					if setups.Add(1) > 1 {
						return nil // the next bring-up's agent takes its setup
					}
					crash := &composeHalt{exitCode: 1, service: "flaky", serviceCode: 1, flags: "--exit-code-from runner", beforeApp: true}
					// flaky crashes again: keploy decides the exit under
					// stopMu, then its stop takes the agent the setup talks to.
					s.a.composeApp.stopMu.Lock()
					if marks != "depCrash" {
						s.a.composeApp.stopping.Store(true)
						s.a.composeApp.agentStopping.Store(true)
					}
					if marks != "agentStopping" {
						s.a.composeApp.depCrash.CompareAndSwap(nil, crash)
					}
					s.a.composeApp.stopMu.Unlock()
					s.d.set("keploy-v3-test", "agent-1", "t3", false)
					return errors.New("keploy's agent container exited while keploy set it up again")
				})
				s.a.Mode = mode
				s.d.set("keploy-v3-test", "agent-1", "t1", false) // keploy's stop for the first crash took the agent
				s.d.set("app", "app-1", "", false)
				ended := false
				s.a.retryComposeUp(context.Background(), func() { ended = true; s.end() }, func() utils.CmdError {
					s.note("up")
					s.d.set("keploy-v3-test", "agent-1", "t3", true) // the retried up starts the agent again
					// The up returns only once keploy's stop has taken every
					// container: after the setup's end was decided.
					if !s.awaitTrail("resume") {
						t.Error("the restarted agent's setup was never decided")
					}
					return utils.CmdError{}
				})
				if g := s.trail(); g != "hold,up,setup,resume" {
					t.Fatalf("the retry did %s; want the hold let go with nil, no failure and no end", g)
				}
				if len(s.resume) == 0 || s.resume[0] != nil {
					t.Fatalf("the hold was let go with %v; want nil: keploy's own stop cut the setup", s.resume)
				}
				if h := s.a.composeAppHalt(); h != nil {
					t.Fatalf("halt kept: %v (agentSetup=%v); want none for a setup keploy's own stop cut", h, h.agentSetup)
				}
				if ended {
					t.Fatal("the run's end on a halt was called for a setup keploy's own stop cut")
				}
				if marks != "agentStopping" &&
					!s.a.composeRetriesTheBringUp(nil, 2, utils.CmdError{}, func() []composeServiceState { return nil }) {
					t.Fatalf("the run's loop takes no further bring-up after the second crash (bound %d)", composeDepFailureRetries)
				}
				// The next bring-up: compose's up starts the agent again,
				// and it is set up before the hold is let go.
				if !s.a.composeBeginsABringUpAgain() {
					t.Fatal("the next bring-up did not begin")
				}
				s.a.retryComposeUp(context.Background(), func() { ended = true }, s.up(func() { s.d.set("keploy-v3-test", "agent-1", "t4", true) }))
				if g := s.trail(); g != "hold,up,setup,resume,hold,up,setup,resume" {
					t.Fatalf("the retries did %s; want the next bring-up's agent held for and set up", g)
				}
				if !s.a.agentKept("agent-1@t4") || ended || s.a.composeAppHalt() != nil {
					t.Fatalf("after the next bring-up: set up last %q (known %v) ended=%v halt=%v; want agent-1@t4 set up and the run on",
						s.a.agentSetUp, s.a.agentSetUpKnown, ended, s.a.composeAppHalt())
				}
			})
		})
	}
}

// AN AGENT COUNTS AS KEPT ONLY ONCE ITS SETUP COMPLETED. compose's retried
// up returns on its own (a service_completed_successfully dependency that
// exits non-zero, which keploy does not stop the stack on) and leaves the
// agent it started again running, its setup cut mid-way or never begun. The
// next retry finds that agent running before its up: it is not the one the
// run set up, so the releases are held and it is set up again. Record and
// test, both KEPLOY_PROVEN modes.
func TestAnAgentWhoseSetupWasCutIsSetUpAgainByTheNextRetry(t *testing.T) {
	for _, shape := range []string{"cut mid-setup", "up returned before the watch saw it", "no hold, cut", "next retry with no baseline"} {
		t.Run(shape, func(t *testing.T) {
			everyRunMode(t, func(t *testing.T, mode models.Mode) {
				var nextRetry atomic.Bool
				began := make(chan struct{}, 4)
				setUp := make(chan struct{}, 4)
				s := newSetupFailApp(t, func(ctx context.Context) error {
					if !nextRetry.Load() {
						began <- struct{}{}
						<-ctx.Done() // the up's end cuts the first retry's setup
						return ctx.Err()
					}
					setUp <- struct{}{}
					return nil
				})
				// The agent's start: after the first retry began where that
				// retry has no baseline, else before the next retry began.
				startAt := time.Now().Add(-time.Minute)
				if shape == "no hold, cut" {
					startAt = time.Now().Add(time.Second)
				}
				s.a.Mode = mode
				s.d.set("keploy-v3-test", "agent-1", "t1", false)
				s.d.set("app", "app-1", "", false)
				if shape == "no hold, cut" {
					s.d.failing = agentBaselineTries
				}
				s.a.retryComposeUp(context.Background(), nil, func() utils.CmdError {
					s.note("up")
					s.d.set("keploy-v3-test", "agent-1", startAt.Format(time.RFC3339Nano), true)
					if shape == "up returned before the watch saw it" {
						return utils.CmdError{} // compose's up returns at once
					}
					select {
					case <-began:
					case <-time.After(30 * time.Second):
						t.Error("the agent compose started again was never set up")
					}
					return utils.CmdError{} // compose's up returned on its own: the setup is cut
				})
				left := "agent-1@" + startAt.Format(time.RFC3339Nano)
				if s.a.agentKept(left) {
					t.Fatalf("after %s (trail %s), the agent left running is taken for the last one set up", shape, s.trail())
				}
				first := s.trail()
				// The up returned before the watch saw the agent: the releases are
				// held for it and let go, so none goes to it until a setup of it
				// completes.
				if shape == "up returned before the watch saw it" && first != "hold,up,hold,resume,resume" {
					t.Fatalf("the retry did %s; want the agent its up left held for once the up returned", first)
				}
				nextRetry.Store(true)
				if !s.a.composeBeginsABringUpAgain() {
					t.Fatal("the next bring-up did not begin")
				}
				if shape == "next retry with no baseline" {
					s.d.failing = agentBaselineTries
				}
				// The next retry: the same agent still runs before its up.
				s.a.retryComposeUp(context.Background(), nil, func() utils.CmdError {
					s.note("up")
					select {
					case <-setUp:
					case <-time.After(5 * time.Second):
					}
					s.d.set("app", "app-1", "t9", true)
					time.Sleep(50 * time.Millisecond)
					return utils.CmdError{}
				})
				want := ",hold,up,setup,resume"
				if shape == "next retry with no baseline" {
					want = ",up,setup" // no hold with no baseline; the agent is set up all the same
				}
				if g := strings.TrimPrefix(s.trail(), first); g != want {
					t.Fatalf("the next retry did %q after %q; want %q: the agent whose setup was cut held for and set up again", g, first, want)
				}
				if !s.a.agentKept(left) {
					t.Fatalf("the agent set up again (%s) is not the last one set up (%q)", left, s.a.agentSetUp)
				}
				if h := s.a.composeAppHalt(); h != nil {
					t.Fatalf("halt %+v kept; want none", h)
				}
			})
		})
	}
}

// An agent whose setup completed is the one the run set up: every later
// retry that finds it running keeps it, holding nothing and setting nothing
// up. A new run begins with no agent set up yet.
func TestAnAgentSetUpAgainIsKeptByTheNextRetry(t *testing.T) {
	everyRunMode(t, func(t *testing.T, mode models.Mode) {
		s := newSetupFailApp(t, func(context.Context) error { return nil })
		s.a.Mode = mode
		s.d.set("keploy-v3-test", "agent-1", "t1", false)
		s.d.set("app", "app-1", "", false)
		s.a.retryComposeUp(context.Background(), nil, s.up(func() { s.d.set("keploy-v3-test", "agent-1", "t3", true) }))
		first := s.trail()
		if first != "hold,up,setup,resume" {
			t.Fatalf("the first retry did %s; want the agent compose started again held for and set up", first)
		}
		if !s.a.agentKept("agent-1@t3") {
			t.Fatalf("an agent whose setup completed is not the last one set up (%q, trail %s)", s.a.agentSetUp, first)
		}
		for retry := 2; retry <= 3; retry++ {
			before := s.trail()
			s.a.retryComposeUp(context.Background(), nil, s.up())
			if g := strings.TrimPrefix(s.trail(), before); g != ",up" {
				t.Fatalf("retry %d did %q for an agent set up again; want it kept", retry, g)
			}
			if !s.a.agentKept("agent-1@t3") {
				t.Fatalf("after retry %d the last agent set up is %q (known %v); want agent-1@t3 kept", retry, s.a.agentSetUp, s.a.agentSetUpKnown)
			}
		}
		s.a.agentBlindSince = time.Now()
		s.a.composeBeginsARun()
		if s.a.agentSetUpKnown || s.a.agentSetUp != "" || !s.a.agentBlindSince.IsZero() {
			t.Fatalf("a new run begins with %q (known %v, blind since %v) set up; want none", s.a.agentSetUp, s.a.agentSetUpKnown, s.a.agentBlindSince)
		}
	})
}

// agentKept reports whether id is the last agent whose setup completed.
func (a *App) agentKept(id string) bool {
	return a.agentSetUpKnown && a.agentSetUp == id
}

// agentNow is the id of keploy's agent container while it runs, else "".
func (d *restartingDocker) agentNow() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.containers["keploy-v3-test"]
	if !ok || !st.Running {
		return ""
	}
	return d.ids["keploy-v3-test"]
}

// AGENTS ARE TOLD APART BY IDENTITY, AND THE WATCH SETS UP EVERY ONE THAT IS
// NOT THE LAST SET UP. A recreate flag on two retries in a row. On the
// first, compose replaces the agent the run set up (agent-1) with agent-2,
// and its up returns on its own while agent-2's setup runs, cutting it (a
// service_completed_successfully dependency that exits non-zero). On the
// next, agent-2 still runs as the up starts, and compose replaces it with
// agent-3 100 ms in. agent-2 is held for and set up, and agent-3 after it,
// whether agent-2's setup ends before the recreate (20 ms) or the recreate
// cuts it (300 ms: compose replaced the agent, which no longer runs, so the
// setup is cut, not failed). The run ends on nothing, and agent-3 is the last
// agent set up. Record and test, both KEPLOY_PROVEN modes.
func TestARecreatedAgentAfterACutSetupIsSetUpAndEndsNothing(t *testing.T) {
	for _, setupTakes := range []time.Duration{20 * time.Millisecond, 300 * time.Millisecond} {
		t.Run("setup_takes_"+setupTakes.String(), func(t *testing.T) {
			everyRunMode(t, func(t *testing.T, mode models.Mode) {
				a, d, _ := newRestartApp(t)
				a.Mode = mode
				var mu sync.Mutex
				var setUpOn []string
				calls := 0
				a.SetOnAgentRestart(func(ctx context.Context) error {
					id := d.agentNow()
					mu.Lock()
					calls++
					n := calls
					mu.Unlock()
					if n == 1 {
						<-ctx.Done() // the first retry's up returns on its own, cutting this setup
						return ctx.Err()
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(setupTakes):
					}
					if now := d.agentNow(); now != id {
						return errors.New("keploy's agent container exited while keploy set it up again")
					}
					mu.Lock()
					setUpOn = append(setUpOn, id)
					mu.Unlock()
					return nil
				})
				var holds atomic.Int32
				a.SetAgentReleaseHold(func(context.Context) func(error) { holds.Add(1); return func(error) {} })
				ended := false
				d.set("keploy-v3-test", "agent-1", "t1", true)
				d.set("app", "app-1", "", false)
				a.retryComposeUp(context.Background(), func() { ended = true }, func() utils.CmdError {
					time.Sleep(60 * time.Millisecond)
					d.set("keploy-v3-test", "agent-2", "t3", true)
					time.Sleep(200 * time.Millisecond)
					return utils.CmdError{}
				})
				if a.agentKept("agent-2@t3") || !a.agentKept("agent-1@t1") {
					t.Fatalf("after the first retry the last agent set up is %q (known %v); want agent-1@t1, agent-2's setup cut", a.agentSetUp, a.agentSetUpKnown)
				}
				holdsBefore := holds.Load()
				a.retryComposeUp(context.Background(), func() { ended = true }, func() utils.CmdError {
					time.Sleep(100 * time.Millisecond)
					d.set("keploy-v3-test", "agent-2", "t3", false)
					time.Sleep(20 * time.Millisecond)
					d.set("keploy-v3-test", "agent-3", "t5", true)
					time.Sleep(800 * time.Millisecond)
					d.set("app", "app-1", "t6", true)
					time.Sleep(100 * time.Millisecond)
					return utils.CmdError{}
				})
				mu.Lock()
				got := append([]string(nil), setUpOn...)
				mu.Unlock()
				if h := a.composeAppHalt(); h != nil {
					t.Fatalf("the run ended on a halt (agentSetup=%v) after compose recreated the agent; set up: %v", h.agentSetup, got)
				}
				if ended {
					t.Fatalf("the run's end on a halt was called after compose recreated the agent; set up: %v", got)
				}
				if len(got) == 0 || got[len(got)-1] != "agent-3" || !a.agentKept("agent-3@t5") {
					t.Fatalf("set up %v, last %q; want agent-3, the agent compose's recreate left running, set up last", got, a.agentSetUp)
				}
				if holds.Load() == holdsBefore {
					t.Fatal("the next retry held no release though agent-2, never set up, ran before its up")
				}
			})
		})
	}
}

// A RETRY DOCKER SAID NOTHING AT LEAVES NO AGENT TAKEN FOR KEPT. docker could
// not name the agent before a retry (keploy's stop had taken the run's), and
// that retry's up started an agent again and returned before the watch saw
// it. The next retry, with docker answering, finds that agent: it started
// after the first such retry began, so it is not the run's, and it is held
// for and set up. Both KEPLOY_PROVEN modes, record and test.
func TestAnAgentAnUnnamedRetryStartedIsSetUpByTheNext(t *testing.T) {
	everyRunMode(t, func(t *testing.T, mode models.Mode) {
		s := newSetupFailApp(t, func(context.Context) error { return nil })
		s.a.Mode = mode
		s.d.set("keploy-v3-test", "agent-1", "t1", false) // keploy's stop took the agent
		s.d.set("app", "app-1", "", false)
		s.d.failing = agentBaselineTries
		started := time.Now().Add(time.Second).Format(time.RFC3339Nano)
		s.a.retryComposeUp(context.Background(), nil, func() utils.CmdError {
			s.note("up")
			s.d.parkTheWatch(t)
			s.d.set("keploy-v3-test", "agent-2", started, true) // the up starts the agent again
			return utils.CmdError{}                             // and returns before the watch saw it
		})
		first := s.trail()
		if first != "up,hold,resume" {
			t.Fatalf("the unnamed retry did %s; want no hold before its up, and the agent it left held for once it returned", first)
		}
		s.a.retryComposeUp(context.Background(), nil, s.up(func() { s.d.set("app", "app-1", "t9", true) }))
		if g := strings.TrimPrefix(s.trail(), first); g != ",hold,up,setup,resume" {
			t.Fatalf("the next retry did %q; want agent-2, started by the unnamed retry and never set up, held for and set up", g)
		}
		if !s.a.agentKept("agent-2@" + started) {
			t.Fatalf("the last agent set up is %q; want agent-2", s.a.agentSetUp)
		}
	})
}

// Two retries in a row that docker names no agent before: an agent the first
// one's up started (and that it never set up) is still not the run's at the
// second, which sets it up. Both KEPLOY_PROVEN modes, record and test.
func TestAnAgentStartedInAnUnnamedRetryIsSetUpByTheNextUnnamedOne(t *testing.T) {
	everyRunMode(t, func(t *testing.T, mode models.Mode) {
		s := newSetupFailApp(t, func(context.Context) error { return nil })
		s.a.Mode = mode
		s.d.set("keploy-v3-test", "agent-1", time.Now().Add(-time.Hour).Format(time.RFC3339Nano), true) // the run's
		s.d.set("app", "app-1", "", false)
		s.d.failing = agentBaselineTries
		started := ""
		s.a.retryComposeUp(context.Background(), nil, func() utils.CmdError {
			s.note("up")
			s.d.parkTheWatch(t)
			started = time.Now().Format(time.RFC3339Nano)
			s.d.set("keploy-v3-test", "agent-2", started, true) // compose recreated the run's agent
			return utils.CmdError{}
		})
		first := s.trail()
		time.Sleep(agentStartSkew + 500*time.Millisecond) // the retry's backoff
		s.d.failing = agentBaselineTries
		s.a.retryComposeUp(context.Background(), nil, s.up(func() { s.d.set("app", "app-1", "t9", true) }))
		if g := strings.TrimPrefix(s.trail(), first); g != ",up,setup" {
			t.Fatalf("the second unnamed retry did %q after %q; want agent-2, which the first started, set up", g, first)
		}
		if !s.a.agentKept("agent-2@" + started) {
			t.Fatalf("the last agent set up is %q (known %v); want agent-2", s.a.agentSetUp, s.a.agentSetUpKnown)
		}
	})
}

// A SET-UP AGENT THAT COMPOSE REPLACED, LEFT BY THE UP, IS HELD FOR. The
// watch set the restarted agent up; compose then replaced it, and its up
// returned before the watch saw the new one. The releases are held for that
// agent and let go, so none goes to it until a setup of it completes: the
// next retry's, which sets it up. Both KEPLOY_PROVEN modes, record and test.
func TestAnAgentReplacedAfterItsSetupIsHeldForUntilSetUp(t *testing.T) {
	everyRunMode(t, func(t *testing.T, mode models.Mode) {
		setUp := make(chan struct{}, 4)
		s := newSetupFailApp(t, func(context.Context) error { setUp <- struct{}{}; return nil })
		s.a.Mode = mode
		s.d.set("keploy-v3-test", "agent-1", "t1", false)
		s.d.set("app", "app-1", "", false)
		s.a.retryComposeUp(context.Background(), nil, func() utils.CmdError {
			s.note("up")
			s.d.set("keploy-v3-test", "agent-1", "t3", true)
			select {
			case <-setUp:
			case <-time.After(30 * time.Second):
				t.Error("the agent compose started again was never set up")
			}
			s.d.parkTheWatch(t)
			s.d.set("keploy-v3-test", "agent-2", "t4", true) // compose replaced it
			return utils.CmdError{}
		})
		first := s.trail()
		if first != "hold,up,setup,hold,resume,resume" {
			t.Fatalf("the retry did %s; want the agent compose put in place of the one set up held for once the up returned", first)
		}
		if !s.a.agentKept("agent-1@t3") {
			t.Fatalf("the last agent set up is %q; want agent-1@t3", s.a.agentSetUp)
		}
		s.a.retryComposeUp(context.Background(), nil, s.up(func() { s.d.set("app", "app-1", "t9", true) }))
		if g := strings.TrimPrefix(s.trail(), first); g != ",hold,up,setup,resume" {
			t.Fatalf("the next retry did %q; want agent-2 held for and set up", g)
		}
		if !s.a.agentKept("agent-2@t4") {
			t.Fatalf("the last agent set up is %q; want agent-2@t4", s.a.agentSetUp)
		}
	})
}

// The up's end holds nothing for an agent that is the last set up, for none
// running, or once the app's container runs: the app starts only behind an
// agent set up. Both KEPLOY_PROVEN modes, record and test.
func TestTheUpsEndHoldsNothingForAKeptOrNoAgent(t *testing.T) {
	for _, shape := range []string{"kept, no app", "kept, stopped by the up", "app started behind an agent the watch missed"} {
		t.Run(shape, func(t *testing.T) {
			everyRunMode(t, func(t *testing.T, mode models.Mode) {
				s := newSetupFailApp(t, func(context.Context) error { return nil })
				s.a.Mode = mode
				s.d.set("keploy-v3-test", "agent-1", "t1", true)
				s.d.set("app", "app-1", "", false)
				s.a.retryComposeUp(context.Background(), nil, func() utils.CmdError {
					s.note("up")
					s.d.parkTheWatch(t)
					switch shape {
					case "kept, stopped by the up":
						s.d.set("keploy-v3-test", "agent-1", "t1", false)
					case "app started behind an agent the watch missed":
						s.d.set("keploy-v3-test", "agent-2", "t4", true)
						s.d.set("app", "app-1", "t5", true)
					}
					return utils.CmdError{}
				})
				if g := s.trail(); g != "up" {
					t.Fatalf("the retry did %s; want nothing held", g)
				}
			})
		})
	}
}

// A setup that failed where docker cannot say whether its agent still runs
// is a failed setup: the run ends on it, as on one whose agent runs. Both
// KEPLOY_PROVEN modes, record and test.
func TestAFailedSetupDockerCannotPlaceEndsTheRun(t *testing.T) {
	everyRunMode(t, func(t *testing.T, mode models.Mode) {
		var s *setupFailApp
		s = newSetupFailApp(t, func(context.Context) error {
			s.d.mu.Lock()
			s.d.failing = 1 // the look after the failed setup gets no answer
			s.d.mu.Unlock()
			return errRestartedAgentSetup
		})
		s.a.Mode = mode
		s.d.set("keploy-v3-test", "agent-1", "t1", false)
		s.d.set("app", "app-1", "", false)
		s.a.retryComposeUp(context.Background(), s.end, func() utils.CmdError {
			s.note("up")
			s.d.set("keploy-v3-test", "agent-1", "t3", true)
			select {
			case <-s.ended:
			case <-time.After(30 * time.Second):
				t.Error("the run did not end within 30 s of the restarted agent's failed setup")
			}
			return utils.CmdError{}
		})
		if g := s.trail(); !strings.HasPrefix(g, "hold,up,setup,resume-failed,end") {
			t.Fatalf("the retry did %s; want the releases ended with the setup's failure, then the run ended", g)
		}
		if h := s.a.composeAppHalt(); h == nil || !errors.Is(h.agentSetup, errRestartedAgentSetup) {
			t.Fatalf("halt %+v; want the restarted agent's failed setup", h)
		}
	})
}

// The run's first agent, kept running, is the one the run set up: the first
// retry docker names it at keeps it, also after a retry docker named no agent
// at. Both KEPLOY_PROVEN modes, record and test.
func TestTheRunsFirstAgentIsKeptOnceDockerNamesIt(t *testing.T) {
	for _, shape := range []string{"first retry named", "an unnamed retry, then a named one"} {
		t.Run(shape, func(t *testing.T) {
			everyRunMode(t, func(t *testing.T, mode models.Mode) {
				s := newSetupFailApp(t, func(context.Context) error { return nil })
				s.a.Mode = mode
				runs := "agent-1@" + time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
				s.d.set("keploy-v3-test", "agent-1", strings.TrimPrefix(runs, "agent-1@"), true)
				s.d.set("app", "app-1", "", false)
				if shape != "first retry named" {
					s.d.failing = agentBaselineTries
					s.a.retryComposeUp(context.Background(), nil, s.up())
				}
				first := s.trail()
				s.a.retryComposeUp(context.Background(), nil, s.up(func() { s.d.set("app", "app-1", "t9", true) }))
				if g := strings.TrimPrefix(s.trail(), first); g != ",up" && g != "up" {
					t.Fatalf("the named retry did %q after %q; want the run's agent kept", g, first)
				}
				if !s.a.agentKept(runs) {
					t.Fatalf("the last agent set up is %q (known %v); want the run's, %s", s.a.agentSetUp, s.a.agentSetUpKnown, runs)
				}
			})
		})
	}
}
