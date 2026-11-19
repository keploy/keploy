package http

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// everySwitchMode runs row under KEPLOY_PROVEN unset and =1.
func everySwitchMode(t *testing.T, row func(t *testing.T)) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			row(t)
		})
	}
}

// recordingApp is an application client that keeps the hooks the agent
// client hands it (restartHooks).
type recordingApp struct {
	restart func(ctx context.Context) error
	hold    func(ctx context.Context) (resume func(failed error))
	refuse  func(failed error)
}

func (r *recordingApp) SetOnAgentRestart(f func(ctx context.Context) error) { r.restart = f }
func (r *recordingApp) SetAgentReleaseHold(f func(ctx context.Context) (resume func(failed error))) {
	r.hold = f
}
func (r *recordingApp) SetAgentReleaseRefusal(f func(failed error)) { r.refuse = f }

// The agent client hands the application client the three hooks of an agent
// compose's retry starts again, and each reaches this client's session: the
// setup, the hold on the releases, and their refusal once the setup failed.
// Without the hold or the refusal, every release would go to an agent that
// is not set up.
func TestTheAgentClientHandsTheAppHooksThatReachItsSession(t *testing.T) {
	everySwitchMode(t, func(t *testing.T) {
		ag := newHandoverAgent(t, "ok", nil)
		app := &recordingApp{}
		ag.a.handRestartHooks(app)
		if app.restart == nil || app.hold == nil || app.refuse == nil {
			t.Fatalf("hooks handed: setup %v, hold %v, refusal %v; want all three", app.restart != nil, app.hold != nil, app.refuse != nil)
		}

		resume := app.hold(context.Background())
		ag.s.mu.Lock()
		held, notSetUp := ag.s.held, ag.s.notSetUp
		ag.s.mu.Unlock()
		if held != 1 || !notSetUp {
			t.Fatalf("the hold hook left the session held=%d notSetUp=%v; want it held for an agent not set up", held, notSetUp)
		}
		resume(nil)
		ag.s.end()

		// The setup hook is this client's setup of the restarted agent: it
		// marks the session's agent not set up as it begins.
		cut, cancel := context.WithCancel(context.Background())
		cancel()
		if err := app.restart(cut); err == nil {
			t.Fatal("a setup cut before it began reported success")
		}
		ag.s.mu.Lock()
		notSetUp = ag.s.notSetUp
		ag.s.mu.Unlock()
		if !notSetUp {
			t.Fatal("the setup hook did not reach this client's session")
		}
		ag.s.end()

		failed := errors.New("the restarted agent refused its mocks (store-mocks): 500")
		app.refuse(failed)
		err := ag.a.MakeAgentReadyForDockerCompose(context.Background())
		if !errors.Is(err, failed) {
			t.Fatalf("a release after the refusal hook ended with %v; want the failed setup", err)
		}
		if got := ag.took(); len(got) != 0 {
			t.Fatalf("the agent took %v after the refusal; want nothing", got)
		}
	})
}

// The application client Setup keeps (registerApp, its only source of the
// app it runs) has the three hooks of an agent compose's retry starts again.
// Without them the retry would run a plain `up`: no hold, no setup and no
// refusal, and the agent compose started again would take the run's releases
// with none of its mocks or settings. Both KEPLOY_PROVEN modes.
func TestTheAppSetupKeepsHasTheRestartHooks(t *testing.T) {
	everySwitchMode(t, func(t *testing.T) {
		ag := newHandoverAgent(t, "ok", nil)
		usrApp := ag.a.registerApp("docker compose up", models.SetupOptions{CommandType: string(utils.DockerCompose)})
		stored, ok := ag.a.apps.Load(uint64(0))
		if !ok || stored != usrApp {
			t.Fatalf("the app Setup keeps is %v; want the one it runs", stored)
		}
		v := reflect.ValueOf(usrApp).Elem()
		for _, hook := range []string{"onAgentRestart", "holdAgentRelease", "refuseAgentReleases"} {
			if f := v.FieldByName(hook); !f.IsValid() || f.IsNil() {
				t.Errorf("the app Setup keeps has no %s hook", hook)
			}
		}
		ag.s.end()
	})
}

// A RELEASE HELD FOR AN AGENT COMPOSE'S RETRY STARTS GOES ON ONLY ONCE THAT
// AGENT IS SET UP. The retry's `up` returned while the restarted agent's
// setup was cut, or before the watch saw it: the hold is let go, and the
// agent left running has none of the run's setup. A release held meanwhile
// waits for a setup of the agent to complete, then goes on, after the
// setup's own release. Both KEPLOY_PROVEN modes.
func TestAHeldReleaseWaitsForTheRestartedAgentsSetupNotTheHoldsEnd(t *testing.T) {
	everySwitchMode(t, func(t *testing.T) {
		ag := newHandoverAgent(t, "ok", nil)
		resume := ag.s.holdReleases(context.Background())
		released := make(chan error, 1)
		go func() { released <- ag.a.MakeAgentReadyForDockerCompose(context.Background()) }()
		time.Sleep(100 * time.Millisecond)
		resume(nil) // the retry's up ended with no setup of the agent completed
		awaitNoAsk(t, ag, 300*time.Millisecond, "a held release once the hold was let go with the agent not set up")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := ag.a.setUpRestartedAgent(ctx); err != nil {
			t.Fatalf("setting the agent up again: %v", err)
		}
		if err := awaitErr(t, released, "the held release"); err != nil {
			t.Fatalf("the held release ended with %v once the agent was set up", err)
		}
		if got := ag.took(); strings.Join(got, ",") != "release,release" {
			t.Fatalf("the agent took %v; want the setup's release, then the run's", got)
		}
	})
}

// A setup of the restarted agent that was cut (no hold on: docker could not
// say which agent ran before, or compose recreated one) leaves an agent with
// part of the run's setup: no release goes to it until a setup completes,
// whose end wakes the release at once. Both KEPLOY_PROVEN modes.
func TestACutSetupLetsNoReleaseGoUntilASetupCompletes(t *testing.T) {
	everySwitchMode(t, func(t *testing.T) {
		ag := newHandoverAgent(t, "ok", nil)
		cut, cancel := context.WithCancel(context.Background())
		cancel()
		if err := ag.a.setUpRestartedAgent(cut); err == nil {
			t.Fatal("a cut setup reported success")
		}
		released := make(chan error, 1)
		go func() { released <- ag.a.MakeAgentReadyForDockerCompose(context.Background()) }()
		awaitNoAsk(t, ag, 300*time.Millisecond, "a release to an agent whose setup was cut")

		ctx, cancelSetup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelSetup()
		if err := ag.a.setUpRestartedAgent(ctx); err != nil {
			t.Fatalf("setting the agent up again: %v", err)
		}
		select {
		case err := <-released:
			if err != nil {
				t.Fatalf("the release ended with %v once the agent was set up", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the release did not go on within 5 s of the completed setup")
		}
	})
}

// The run's end lets the next run's releases go: its first bring-up's agent
// is set up by the run itself.
func TestARunsEndLetsTheNextRunsReleaseGo(t *testing.T) {
	everySwitchMode(t, func(t *testing.T) {
		ag := newHandoverAgent(t, "ok", nil)
		resume := ag.s.holdReleases(context.Background())
		resume(nil)
		ag.s.end()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := ag.a.MakeAgentReadyForDockerCompose(ctx); err != nil {
			t.Fatalf("the next run's release ended with %v; want it taken", err)
		}
	})
}
