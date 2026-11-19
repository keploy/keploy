package app

import (
	"context"
	"errors"
	"strings"
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
				if s.a.agentNotSetUp || ended || s.a.composeAppHalt() != nil {
					t.Fatalf("after the next bring-up: agentNotSetUp=%v ended=%v halt=%v; want the agent set up and the run on",
						s.a.agentNotSetUp, ended, s.a.composeAppHalt())
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
				if !s.a.agentNotSetUp {
					t.Fatalf("after %s (trail %s), the agent left running is not marked as not set up", shape, s.trail())
				}
				first := s.trail()
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
				if s.a.agentNotSetUp {
					t.Fatal("the agent set up again is still marked as not set up")
				}
				if h := s.a.composeAppHalt(); h != nil {
					t.Fatalf("halt %+v kept; want none", h)
				}
			})
		})
	}
}

// An agent whose setup completed is the one the run set up: a later retry
// that finds it running keeps it, holding nothing and setting nothing up.
// A new run begins with no agent marked.
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
		if s.a.agentNotSetUp {
			t.Fatalf("an agent whose setup completed is marked as not set up (trail %s)", first)
		}
		s.a.retryComposeUp(context.Background(), nil, s.up(func() { s.d.set("app", "app-1", "t5", true) }))
		if g := strings.TrimPrefix(s.trail(), first); g != ",up" {
			t.Fatalf("the next retry did %q for an agent set up again; want it kept", g)
		}
		s.a.agentNotSetUp = true
		s.a.composeBeginsARun()
		if s.a.agentNotSetUp {
			t.Fatal("a new run begins with an agent marked as not set up")
		}
	})
}
