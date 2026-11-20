package http

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// awaitReleaseWaits waits until a release the run asked for waits on the
// session to send its request (releaseWaits).
func awaitReleaseWaits(t *testing.T, s *agentSession) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		s.mu.Lock()
		waits := s.releaseWaits()
		s.mu.Unlock()
		if waits {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the run's release never waited on the session")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A RELEASE THE RUN ASKS FOR ONCE THE RESTARTED AGENT IS SET UP REACHES IT
// WHILE THE HOLD IS ON. A dependency crashed before keploy's agent answered:
// keploy stopped the stack, the run had set nothing up and asked for no
// release, and the retry's `up` started the agent again. The retry's watch
// set it up (nothing to make again) while its hold, kept until the app's
// container starts, held the releases; the run then set the agent up itself
// and asked for its release, which the hold held while the app, behind the
// agent's healthcheck, waited for it: the run waited until it was stopped
// (CI's replay isolation job, record-compose-flaky). The watch has the agent
// client make that release (the hook it handed the app), on the agent set
// up, with the hold still on; the run's own release goes on once the hold
// ends. Both KEPLOY_PROVEN modes.
func TestAReleaseAskedOnceTheRestartedAgentIsSetUpReachesItUnderTheHold(t *testing.T) {
	everySwitchMode(t, func(t *testing.T) {
		ag := newHandoverAgent(t, "ok", nil)
		app := &recordingApp{}
		ag.a.handRestartHooks(app)
		if app.held == nil {
			t.Fatal("the agent client handed the app no hook to make a held release")
		}
		resume := app.hold(context.Background()) // no agent ran before the retry's up
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := app.restart(ctx); err != nil {
			t.Fatalf("setting the agent the retry started up: %v", err)
		}
		released := make(chan error, 1)
		go func() { released <- ag.a.MakeAgentReadyForDockerCompose(context.Background()) }()
		awaitNoAsk(t, ag, 300*time.Millisecond, "the run's release under the hold")
		awaitReleaseWaits(t, ag.s)

		asked := 0
		current := func(context.Context) bool { asked++; return true }
		if err := app.held(ctx, current); err != nil {
			t.Fatalf("the release the hold held, made on the agent set up, ended with %v", err)
		}
		if got := ag.took(); strings.Join(got, ",") != "release" || asked != 1 {
			t.Fatalf("the agent took %v (current asked %d times); want the release the hold held, once the agent was confirmed the one set up", got, asked)
		}
		select {
		case err := <-released:
			t.Fatalf("the run's release ended (%v) while the hold was on", err)
		default:
		}
		if err := app.held(ctx, current); err != nil || len(ag.took()) != 1 || asked != 1 {
			t.Fatalf("a second look made the release again (err %v, took %v, current asked %d times); want it made once for the agent", err, ag.took(), asked)
		}

		resume(nil) // the app's container started: the watch ends
		if err := awaitErr(t, released, "the run's release"); err != nil {
			t.Fatalf("the run's release ended with %v once the hold was let go", err)
		}
		if got := ag.took(); strings.Join(got, ",") != "release,release" {
			t.Fatalf("the agent took %v; want the release the watch made, then the run's", got)
		}
	})
}

// A release the restarted agent's setup made, last, is the one the agent was
// owed: the watch makes none again, and asks nothing of the agent's identity.
// Both KEPLOY_PROVEN modes.
func TestTheWatchMakesNoReleaseTheSetupMade(t *testing.T) {
	everySwitchMode(t, func(t *testing.T) {
		ag := newHandoverAgent(t, "ok", nil)
		app := &recordingApp{}
		ag.a.handRestartHooks(app)
		if app.held == nil {
			t.Fatal("the agent client handed the app no hook to make a held release")
		}
		resume := app.hold(context.Background())
		released := make(chan error, 1)
		go func() { released <- ag.a.MakeAgentReadyForDockerCompose(context.Background()) }()
		awaitReleaseWaits(t, ag.s)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := app.restart(ctx); err != nil {
			t.Fatalf("setting the agent the retry started up: %v", err)
		}
		if got := ag.took(); strings.Join(got, ",") != "release" {
			t.Fatalf("the setup made %v; want its release, last", got)
		}
		asked := false
		if err := app.held(ctx, func(context.Context) bool { asked = true; return true }); err != nil || asked {
			t.Fatalf("the watch's look after a setup that released the agent: err %v, identity asked %v; want nothing owed", err, asked)
		}
		if got := ag.took(); len(got) != 1 {
			t.Fatalf("the agent took %v; want the setup's release alone", got)
		}
		resume(nil)
		if err := awaitErr(t, released, "the run's release"); err != nil {
			t.Fatalf("the run's release ended with %v once the hold was let go", err)
		}
	})
}

// heldSession is a session in the state the watch's look finds when a release
// is owed to the agent set up (releaseHeld): a hold on, the restarted agent's
// setup completed (gen 1, notSetUp clear) without a release, and the run's
// release waiting, its ready step recorded. made counts the times that step
// was made.
func heldSession(t *testing.T) (s *agentSession, rc *releaseCall, made *int) {
	t.Helper()
	s = newAgentSession()
	s.configure(true)
	made = new(int)
	_, cut := context.WithCancelCause(context.Background())
	rc, refused := s.beginRelease(false, cut, func(context.Context) error { *made++; return nil })
	if refused != nil {
		t.Fatalf("the run's release was refused with %v", refused)
	}
	s.held, s.gen = 1, 1
	return s, rc, made
}

// The watch's look makes a release only when one is owed to the agent set up,
// and asks the agent's identity (current) only then, right before. Each shape
// below takes one condition away from the owed state (heldSession); with it
// gone nothing is made and nothing asked. Both KEPLOY_PROVEN modes.
func TestTheWatchMakesAHeldReleaseOnlyWhenOneIsOwed(t *testing.T) {
	shapes := []struct {
		name  string
		owed  bool
		shape func(s *agentSession, rc *releaseCall)
	}{
		{"owed", true, func(*agentSession, *releaseCall) {}},
		{"no hold on", false, func(s *agentSession, _ *releaseCall) { s.held = 0 }},
		{"no setup completed since the hold", false, func(s *agentSession, _ *releaseCall) { s.notSetUp = true }},
		{"the run's releases refused", false, func(s *agentSession, _ *releaseCall) { s.refused = errors.New("setup failed") }},
		{"the release refused", false, func(_ *agentSession, rc *releaseCall) { rc.refused = errors.New("setup failed") }},
		{"a setup's release reached the agent", false, func(s *agentSession, _ *releaseCall) { s.releasedGen = s.gen }},
		{"the release has its request out", false, func(_ *agentSession, rc *releaseCall) { rc.posting = true }},
		{"only a setup's release in flight", false, func(_ *agentSession, rc *releaseCall) { rc.setup = true }},
		{"the release ended", false, func(s *agentSession, rc *releaseCall) { delete(s.releases, rc) }},
		{"no release step recorded", false, func(s *agentSession, _ *releaseCall) { s.steps = nil }},
		{"only another step recorded", false, func(s *agentSession, _ *releaseCall) {
			s.steps = []sessionStep{{kind: stepMockOutgoing, apply: func(context.Context) error { return errors.New("not a release") }}}
		}},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			everySwitchMode(t, func(t *testing.T) {
				s, rc, made := heldSession(t)
				sh.shape(s, rc)
				asked := 0
				if err := s.releaseHeld(context.Background(), func(context.Context) bool { asked++; return true }); err != nil {
					t.Fatalf("releaseHeld: %v", err)
				}
				want := 0
				if sh.owed {
					want = 1
				}
				if *made != want || asked != want {
					t.Fatalf("made the release %d times, asked the agent's identity %d times; want %d of each", *made, asked, want)
				}
				if sh.owed && s.releasedGen != s.gen {
					t.Fatalf("after the release the agent released is gen %d; want %d, the one set up", s.releasedGen, s.gen)
				}
			})
		})
	}
}

// The release goes only to the agent set up: when current says another runs,
// or the release fails, none is counted made, and the next look makes it
// again. Both KEPLOY_PROVEN modes.
func TestAHeldReleaseGoesOnlyToTheAgentSetUp(t *testing.T) {
	everySwitchMode(t, func(t *testing.T) {
		s, _, made := heldSession(t)
		if err := s.releaseHeld(context.Background(), func(context.Context) bool { return false }); err != nil || *made != 0 || s.releasedGen == s.gen {
			t.Fatalf("with another agent running: err %v, made %d, released gen %d; want nothing made", err, *made, s.releasedGen)
		}

		failed := errors.New("the agent's container exited")
		s.steps[0].apply = func(context.Context) error { *made++; return failed }
		if err := s.releaseHeld(context.Background(), func(context.Context) bool { return true }); !errors.Is(err, failed) || s.releasedGen == s.gen {
			t.Fatalf("a release that failed: err %v, released gen %d; want its error, and the agent not counted released", err, s.releasedGen)
		}
		s.steps[0].apply = func(context.Context) error { *made++; return nil }
		if err := s.releaseHeld(context.Background(), func(context.Context) bool { return true }); err != nil || *made != 2 || s.releasedGen != s.gen {
			t.Fatalf("the next look: err %v, made %d, released gen %d; want the release made again", err, *made, s.releasedGen)
		}
	})
}
