package http

import (
	"context"
	"errors"
	"net/http"
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

// A FAILED SETUP WITH NO HOLD ON CUTS A RELEASE WHOSE REQUEST IS OUT. No hold
// was on (docker could not say which agent ran before compose's retry, or
// compose recreated one that ran), and a release of the run has its request
// out to the agent, which has not answered it. The agent client cannot set
// the agent compose started again up (refuseRun): the release ends at once
// with that failure, its request cut, not waiting on an answer that would
// release the app against an agent with none of the run's mocks or settings.
// A release the run asks for later ends at once too, asking the agent
// nothing at all. Both KEPLOY_PROVEN modes.
func TestAFailedSetupWithNoHoldCutsAReleaseWhoseRequestIsOutInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			ag := newHandoverAgent(t, "ok", make(chan struct{}))

			released := make(chan error, 1)
			go func() { released <- ag.a.MakeAgentReadyForDockerCompose(context.Background()) }()
			ag.awaitAsked(t)

			ag.s.refuseRun(errRestartedSetup)
			select {
			case err := <-released:
				if !errors.Is(err, errRestartedSetup) || errors.Is(err, errReleaseCut) {
					t.Fatalf("the release whose request was out ended with %v; want the failed setup, never a release owed to the agent", err)
				}
				if !strings.Contains(err.Error(), "could not be set up again") {
					t.Fatalf("the release ended with %q; want it to say the agent could not be set up again", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the release whose request was out still waited on the agent's answer 10 s after the restarted agent's setup failed")
			}
			if got := ag.took(); len(got) != 0 {
				t.Fatalf("the agent answered %v; want the release ended before any answer", got)
			}

			sent := countClientRequests(ag.a)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := ag.a.MakeAgentReadyForDockerCompose(ctx); !errors.Is(err, errRestartedSetup) {
				t.Fatalf("a release asked after the failed setup ended with %v; want the failed setup", err)
			}
			if n := sent.n.Load(); n != 0 {
				t.Fatalf("a release asked after the failed setup sent the agent %d requests; want none", n)
			}
		})
	}
}

// answeredThenRefused is the client's transport, which refuses the run's
// releases (refuseRun) once the agent's answer to a release is in hand and
// before the release has read it: the failed setup comes while the request
// is out, after the agent took it.
type answeredThenRefused struct {
	base http.RoundTripper
	s    *agentSession
}

func (r answeredThenRefused) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err == nil && strings.HasSuffix(req.URL.Path, "/agent/ready") {
		r.s.refuseRun(errRestartedSetup)
	}
	return resp, err
}

// A release the agent answered while its setup's failure refused it ends with
// that failure, not as released: refuseRun comes once the agent's answer is
// in hand and before the release has read it. Both KEPLOY_PROVEN modes.
func TestAFailedSetupWithNoHoldEndsAReleaseTheAgentAnsweredInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			ag := newHandoverAgent(t, "ok", nil)
			base := ag.a.client.Transport
			if base == nil {
				base = http.DefaultTransport
			}
			ag.a.client.Transport = answeredThenRefused{base: base, s: ag.s}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := ag.a.MakeAgentReadyForDockerCompose(ctx)
			if !errors.Is(err, errRestartedSetup) || errors.Is(err, errReleaseCut) {
				t.Fatalf("the release refused while the agent answered it ended with %v; want the failed setup", err)
			}
			if got := ag.took(); strings.Join(got, ",") != "release" {
				t.Fatalf("the agent took %v; want the one release whose answer was out", got)
			}
		})
	}
}

// A release the failed setup refused before its request went out is never let
// post, with no hold on: awaitPost refuses it under the session's lock,
// whatever net/http does with a request whose context is cut, and the
// release's context carries the failure. Both KEPLOY_PROVEN modes.
func TestARefusedReleaseIsNotLetPostWithNoHoldInEveryMode(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("switch="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			ag := newHandoverAgent(t, "ok", nil)
			ctx, cut := context.WithCancelCause(context.Background())
			defer cut(nil)
			rc, refused := ag.s.beginRelease(false, cut, func(context.Context) error { return nil })
			if refused != nil {
				t.Fatalf("a release asked before any failed setup was refused with %v", refused)
			}
			defer ag.s.endRelease(rc, false)

			ag.s.refuseRun(errRestartedSetup)
			if ag.s.awaitPost(ctx.Done(), rc) {
				t.Fatal("awaitPost let the refused release post; want no post")
			}
			if cause := context.Cause(ctx); !errors.Is(cause, errRestartedSetup) {
				t.Fatalf("the refused release's context was cut with %v; want the failed setup", cause)
			}
			ag.s.mu.Lock()
			out := ag.s.releaseOut()
			ag.s.mu.Unlock()
			if out {
				t.Fatal("the refused release has its request out; want none")
			}
		})
	}
}
