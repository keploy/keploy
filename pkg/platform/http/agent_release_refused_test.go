package http

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

var errRestartedSetup = errors.New("failed to set keploy's restarted agent up again (store-mocks): 500")

// awaitNoAsk fails the row when a release reaches the agent within d.
func awaitNoAsk(t *testing.T, ag *handoverAgent, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ag.asked:
		t.Fatalf("%s reached the agent", what)
	case <-time.After(d):
	}
}

// A HOLD LET GO ON A FAILED SETUP RELEASES NOTHING. compose's retry held the
// releases while its `up` started keploy's agent again, and the agent client
// could not set that agent up again: the hold is let go with the failure
// (holdReleases' resume). The release it held ends with that failure at once,
// sending nothing to the agent, which has none of the run's mocks or
// settings: so does every release this run asks for until it ends. The next
// run's release goes on. Both KEPLOY_PROVEN modes.
func TestAFailedSetupEndsEveryReleaseOfTheRunWithNoRequestInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			ag := newHandoverAgent(t, "ok", nil)
			resume := ag.s.holdReleases(context.Background())

			released := make(chan error, 1)
			go func() { released <- ag.a.MakeAgentReadyForDockerCompose(context.Background()) }()
			select {
			case <-ag.asked:
				t.Fatal("a release the run asked for reached the agent while the releases were held")
			case err := <-released:
				t.Fatalf("a held release ended (%v) before its hold was let go", err)
			case <-time.After(300 * time.Millisecond):
			}

			resume(errRestartedSetup)
			err := awaitErr(t, released, "the release held when the setup failed")
			if !errors.Is(err, errRestartedSetup) || errors.Is(err, errReleaseCut) {
				t.Fatalf("the held release ended with %v; want the failed setup, never a release owed to the agent", err)
			}
			if !strings.Contains(err.Error(), "could not be set up again") {
				t.Fatalf("the held release ended with %q; want it to say the agent could not be set up again", err)
			}

			// A release the run asks for later ends so too, at once.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := ag.a.MakeAgentReadyForDockerCompose(ctx); !errors.Is(err, errRestartedSetup) {
				t.Fatalf("a release asked after the failed setup ended with %v; want the failed setup", err)
			}
			awaitNoAsk(t, ag, 300*time.Millisecond, "a release of the run whose restarted agent's setup failed")
			if got := ag.took(); len(got) != 0 {
				t.Fatalf("the agent took %v; want no release of the run whose restarted agent's setup failed", got)
			}

			// The run's end lets the failure go: the next run's release
			// goes on.
			ag.s.end()
			if err := ag.a.MakeAgentReadyForDockerCompose(ctx); err != nil {
				t.Fatalf("the next run's release ended with %v; want it taken", err)
			}
			if got := ag.took(); strings.Join(got, ",") != "release" {
				t.Fatalf("the agent took %v; want the next run's release alone", got)
			}
		})
	}
}

// A failed setup ends a held release at once, even while another hold is
// still on: it is never held for good. Both KEPLOY_PROVEN modes.
func TestAFailedSetupEndsAReleaseAnotherHoldStillHoldsInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			ag := newHandoverAgent(t, "ok", nil)
			first := ag.s.holdReleases(context.Background())
			second := ag.s.holdReleases(context.Background())
			defer second(nil)

			released := make(chan error, 1)
			go func() { released <- ag.a.MakeAgentReadyForDockerCompose(context.Background()) }()
			awaitNoAsk(t, ag, 300*time.Millisecond, "a release held by two holds")

			first(errRestartedSetup)
			if err := awaitErr(t, released, "the release the failed setup ended"); !errors.Is(err, errRestartedSetup) {
				t.Fatalf("the held release ended with %v; want the failed setup", err)
			}
			if got := ag.took(); len(got) != 0 {
				t.Fatalf("the agent took %v; want nothing", got)
			}
		})
	}
}
