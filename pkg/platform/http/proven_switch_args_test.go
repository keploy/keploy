package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The native agent is started through sudo, which drops KEPLOY_PROVEN, so the
// switch must reach it as an argv flag; with the switch off the argv must not
// change at all.
func TestNativeAgentArgs_CarryTheProvenSwitch(t *testing.T) {
	t.Cleanup(func() { _ = token.Cleanup() })
	flag := "--" + provenswitch.AgentFlag
	a := New(zap.NewNop(), nil, &config.Config{})
	opts := models.SetupOptions{AgentPort: 26795, ProxyPort: 26796, DnsPort: 26797, AgentURI: "http://localhost:26795/agent", Mode: models.MODE_TEST}

	t.Setenv(provenswitch.Env, "0")
	if args := a.nativeAgentArgs(opts); slices.Contains(args, flag) {
		t.Fatalf("switch off, yet the native agent argv carries %s: %v", flag, args)
	}

	// With DOCKER_HOST set the launcher does not ask the host's docker CLI
	// for its context (containerkey.AgentArgs).
	t.Setenv("DOCKER_HOST", "unix:///run/test-docker.sock")
	t.Setenv(provenswitch.Env, "1")
	if args := a.nativeAgentArgs(opts); !slices.Contains(args, flag) {
		t.Fatalf("KEPLOY_PROVEN=1, yet the native agent argv lacks %s, so the agent's parsers run the old paths: %v", flag, args)
	}
}

// sudo drops DOCKER_HOST too, so under the switch the native agent is told
// the client's Docker endpoint, where it resolves container keys; with the
// switch off its argv does not change.
func TestNativeAgentArgs_CarryTheDockerEndpointUnderTheSwitch(t *testing.T) {
	t.Cleanup(func() { _ = token.Cleanup() })
	flag := "--" + containerkey.AgentFlag + "=unix:///run/user/1000/docker.sock"
	a := New(zap.NewNop(), nil, &config.Config{})
	opts := models.SetupOptions{AgentPort: 26795, ProxyPort: 26796, DnsPort: 26797, AgentURI: "http://localhost:26795/agent", Mode: models.MODE_TEST}
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")

	t.Setenv(provenswitch.Env, "0")
	if args := a.nativeAgentArgs(opts); slices.ContainsFunc(args, func(s string) bool { return strings.HasPrefix(s, "--"+containerkey.AgentFlag) }) {
		t.Fatalf("switch off, yet the native agent argv carries the Docker endpoint: %v", args)
	}
	t.Setenv(provenswitch.Env, "1")
	if args := a.nativeAgentArgs(opts); !slices.Contains(args, flag) {
		t.Fatalf("KEPLOY_PROVEN=1 and DOCKER_HOST set, yet the native agent argv lacks %s: %v", flag, args)
	}
}

// healthAnswer is how a stand-in agent answers one read of its /health; n is
// the read's number, from 1.
type healthAnswer func(w http.ResponseWriter, n int32)

// healthSays is a health check that answers status, reporting the proven switch
// state ("" for an agent that predates the switch and sends no header).
func healthSays(state string, status int) healthAnswer {
	return func(w http.ResponseWriter, _ int32) {
		if state != "" {
			w.Header().Set(provenswitch.HealthHeader, state)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`"OK"`))
	}
}

// drops is a health check that drops the connection without an answer.
func drops(w http.ResponseWriter, _ int32) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// standInAgent is an agent whose /health answers as it is told, and which
// counts what it is asked: reads of /health, POSTs to /agent/ready (each one
// releases the app) and anything else.
type standInAgent struct {
	url         string
	healthReads atomic.Int32
	readies     atomic.Int32
	others      atomic.Int32
}

func newStandIn(t *testing.T, health healthAnswer) *standInAgent {
	t.Helper()
	sa := &standInAgent{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/agent/ready"):
			sa.readies.Add(1)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/health"):
			health(w, sa.healthReads.Add(1))
		default:
			sa.others.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	sa.url = srv.URL
	return sa
}

// newStandInAgent is a stand-in whose every health check answers status,
// reporting the proven switch state.
func newStandInAgent(t *testing.T, state string, status int) *standInAgent {
	t.Helper()
	return newStandIn(t, healthSays(state, status))
}

func clientFor(uri string) *AgentClient {
	return New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
}

// clientRequests counts the requests an AgentClient sends itself, apart from
// the health checks of its readiness ticker (pkg.AgentHealthTickerWithAnswer),
// which has an HTTP client of its own and asks again whenever an answer is
// slower than a check waits for it, so what the stand-in counts depends on
// the clock and this count does not. With fail set, every request fails with
// what fail returns, as one to an agent that is gone would.
type clientRequests struct {
	base http.RoundTripper
	n    atomic.Int32
	fail func() error
}

func (c *clientRequests) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	if c.fail != nil {
		return nil, c.fail()
	}
	return c.base.RoundTrip(r)
}

// countClientRequests puts a clientRequests in front of a's own transport.
func countClientRequests(a *AgentClient) *clientRequests {
	base := a.client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c := &clientRequests{base: base}
	a.client.Transport = c
	return c
}

// switchStates is the switch off and on, as KEPLOY_PROVEN values.
var switchStates = []struct{ name, env string }{{"switch off", "0"}, {"switch on", "1"}}

// The CLI refuses an agent whose switch is not its own, in both directions,
// and with the switch on an agent that predates the switch; and it takes the
// agent's switch from the health answer that found the agent ready. It sends
// the agent nothing of its own, so with the switch off the bring-up asks
// exactly what it did before the switch existed: the health checks. A refused
// agent is stopped rather than left running. Each case has a definite answer,
// so none has a deadline, and none depends on how many health checks the
// ticker needed.
func TestWaitForAgentChecksTheSwitchOfTheAnswerThatFoundItReady(t *testing.T) {
	for _, tc := range []struct {
		name, local, agent string
		ok                 bool
	}{
		{"both off", "0", "off", true},
		{"both on", "1", "on", true},
		{"off, agent predates the switch", "0", "", true},
		{"on, agent off", "1", "off", false},
		{"off, agent on", "0", "on", false},
		{"on, agent predates the switch", "1", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.local)
			agent := newStandInAgent(t, tc.agent, http.StatusOK)
			a := clientFor(agent.url)
			sent := countClientRequests(a)
			stopped := false
			a.agentCancel = func() { stopped = true }
			err := a.waitForAgent(context.Background(), nil, false, models.SetupOptions{})
			if (err == nil) != tc.ok {
				t.Fatalf("waitForAgent = %v, want ok=%v", err, tc.ok)
			}
			if err != nil {
				if !errors.Is(err, provenswitch.ErrAgentSwitch) {
					t.Fatalf("a refusal must wrap provenswitch.ErrAgentSwitch: %v", err)
				}
				if !stopped {
					t.Fatal("the refused agent was left running")
				}
			} else if stopped {
				t.Fatal("an agent that was run against was stopped")
			}
			if n, o := sent.n.Load(), agent.others.Load(); n != 0 || o != 0 {
				t.Fatalf("the client sent the agent %d request(s) of its own, and the agent was asked %d thing(s) besides its health; want only the health checks", n, o)
			}
		})
	}
}

// An agent that has answered the health check that found it ready is not
// asked again, so it is run against even if, a moment later, it has exited or
// stops answering: the bring-up does not wait on it a second time, whichever
// the switch. Here every request the client sends itself fails, as one to an
// agent that is gone would, and ends the test at once, on its count.
func TestWaitForAgentDoesNotAskAgainAfterTheAnswerThatFoundItReady(t *testing.T) {
	for _, st := range switchStates {
		t.Run(st.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, st.env)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			askedAgain := errors.New("the client asked the agent again after it was found ready")
			agent := newStandInAgent(t, provenswitch.State(), http.StatusOK)
			a := clientFor(agent.url)
			gone := countClientRequests(a)
			gone.fail = func() error {
				cancel(askedAgain)
				return askedAgain
			}
			if err := a.waitForAgent(ctx, nil, false, models.SetupOptions{}); err != nil {
				t.Fatalf("waitForAgent = %v; the agent had answered the health check that found it ready", err)
			}
			if n := gone.n.Load(); n != 0 {
				t.Fatalf("the client asked the agent %d more time(s) after it was found ready, want 0", n)
			}
		})
	}
}

// The switch is taken only from an answer that found the agent ready: what an
// answer other than 200 says is not the agent's switch. With the switch on, an
// agent whose first health check answers 503 reporting the switch off, and
// whose later ones answer 200 reporting it on, is run against.
func TestWaitForAgentTakesTheSwitchOnlyFromAHealthyAnswer(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	agent := newStandIn(t, func(w http.ResponseWriter, n int32) {
		if n == 1 {
			healthSays("off", http.StatusServiceUnavailable)(w, n)
			return
		}
		healthSays("on", http.StatusOK)(w, n)
	})
	a := clientFor(agent.url)
	sent := countClientRequests(a)
	if err := a.waitForAgent(context.Background(), nil, false, models.SetupOptions{}); err != nil {
		t.Fatalf("waitForAgent = %v; the switch of a 503 is not the agent's", err)
	}
	if n := sent.n.Load(); n != 0 {
		t.Fatalf("the client sent the agent %d request(s) of its own, want 0", n)
	}
}

// An agent whose health check never answers is not ready, whichever the
// switch: the wait ends the way it did before the switch existed (here, the
// caller giving up), never with a switch refusal. The stand-in drops every
// connection and the caller gives up at its second read, on the count, so a
// wait that ends with the caller's cause read the agent more than once.
func TestWaitForAgentWithAnUnreadableHealthIsNotASwitchRefusal(t *testing.T) {
	for _, st := range switchStates {
		t.Run(st.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, st.env)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			gaveUp := errors.New("the caller gave up after two reads")
			agent := newStandIn(t, func(w http.ResponseWriter, n int32) {
				drops(w, n)
				if n >= 2 {
					cancel(gaveUp)
				}
			})
			err := clientFor(agent.url).waitForAgent(ctx, nil, false, models.SetupOptions{})
			if !errors.Is(err, gaveUp) || errors.Is(err, provenswitch.ErrAgentSwitch) {
				t.Fatalf("waitForAgent = %v; want the caller's cause, not a switch refusal", err)
			}
		})
	}
}

// A start that stops making progress is what ends the wait for an agent that
// never answers its health check, in either switch state: that is an agent
// that did not become ready (pkg.ErrAgentNotReady, which the caller retries
// with a fresh agent), never a switch refusal, and it is stopped rather than
// left running. This one never reports its start, and its silence is given up
// on at the first look (bootSilence, shortened).
func TestWaitForAgentWhoseStartStopsIsNotReady(t *testing.T) {
	look, silence := bootLook, bootSilence
	bootLook, bootSilence = time.Millisecond, 0
	t.Cleanup(func() { bootLook, bootSilence = look, silence })
	for _, st := range switchStates {
		t.Run(st.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, st.env)
			a := clientFor(newStandIn(t, drops).url)
			stopped := false
			a.agentCancel = func() { stopped = true }
			err := a.waitForAgent(context.Background(), nil, false, models.SetupOptions{})
			if !errors.Is(err, pkg.ErrAgentNotReady) || errors.Is(err, provenswitch.ErrAgentSwitch) {
				t.Fatalf("waitForAgent = %v; want pkg.ErrAgentNotReady, not a switch refusal", err)
			}
			if !stopped {
				t.Fatal("the agent that never became ready was left running")
			}
		})
	}
}

// With the switch off, the docker compose readiness step is exactly what it
// was before the switch existed: the agent is told to release the app, and
// nothing else is asked of it. v3.6.117 instead read /health first and would
// not release an agent whose health check did not answer 200, however ready
// its /agent/ready said it was. Nothing is read with the switch off, so not
// even an agent that would report the switch on is asked: the agent compose
// starts is the one in the file keploy wrote, without --proven. Each case has
// a definite answer; a second read of /health gives up at once, on its count,
// rather than leaving a regression to the readiness budget.
func TestMakeAgentReadyForDockerComposeWithTheSwitchOffIsAsBefore(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	for _, tc := range []struct {
		name   string
		health healthAnswer
	}{
		{"health answers 503", healthSays("", http.StatusServiceUnavailable)},
		{"health drops the connection", drops},
		{"health answers 200 off", healthSays("off", http.StatusOK)},
		{"health would report the switch on", healthSays("on", http.StatusOK)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			agent := newStandIn(t, func(w http.ResponseWriter, n int32) {
				tc.health(w, n)
				if n >= 2 {
					cancel()
				}
			})
			err := clientFor(agent.url).MakeAgentReadyForDockerCompose(ctx)
			if err != nil || agent.readies.Load() != 1 {
				t.Fatalf("MakeAgentReadyForDockerCompose = %v after %d release(s); with the switch off the agent is released as before", err, agent.readies.Load())
			}
			if n := agent.healthReads.Load(); n != 0 {
				t.Fatalf("with the switch off the agent's health was read %d time(s); before the switch existed it was not read at all", n)
			}
		})
	}
}

// With the switch on, an agent is told to release the app only once its
// switch has been read and agrees: one whose health check keeps answering 503
// is never released, however ready its /agent/ready says it is, and the wait
// goes on reading (here, until the caller gives up after the second read, on
// the count); one whose health check answers 503 once and then reports the
// switch on is released after that read.
func TestMakeAgentReadyForDockerComposeWithTheSwitchOnWaitsForAReadableSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	never := newStandIn(t, func(w http.ResponseWriter, n int32) {
		healthSays("on", http.StatusServiceUnavailable)(w, n)
		if n >= 2 {
			cancel()
		}
	})
	err := clientFor(never.url).MakeAgentReadyForDockerCompose(ctx)
	if !errors.Is(err, context.Canceled) || never.readies.Load() != 0 {
		t.Fatalf("MakeAgentReadyForDockerCompose = %v after %d release(s); an agent whose switch was never read must not be released", err, never.readies.Load())
	}

	later := newStandIn(t, func(w http.ResponseWriter, n int32) {
		if n == 1 {
			healthSays("on", http.StatusServiceUnavailable)(w, n)
			return
		}
		healthSays("on", http.StatusOK)(w, n)
	})
	if err := clientFor(later.url).MakeAgentReadyForDockerCompose(context.Background()); err != nil || later.readies.Load() != 1 {
		t.Fatalf("MakeAgentReadyForDockerCompose = %v after %d release(s); an agent whose switch became readable and agrees must be released once", err, later.readies.Load())
	}
	if n := later.healthReads.Load(); n != 2 {
		t.Fatalf("the agent's switch was read %d times, want 2", n)
	}
}

// With the switch on, under docker compose the check comes before the agent is
// told to release the app: a disagreeing agent is refused and never released;
// an agreeing one is released as before.
func TestMakeAgentReadyForDockerComposeChecksTheSwitchFirst(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	bad := newStandInAgent(t, "off", http.StatusOK)
	err := clientFor(bad.url).MakeAgentReadyForDockerCompose(context.Background())
	if !errors.Is(err, provenswitch.ErrAgentSwitch) {
		t.Fatalf("MakeAgentReadyForDockerCompose = %v; want a refusal wrapping provenswitch.ErrAgentSwitch", err)
	}
	if n := bad.readies.Load(); n != 0 {
		t.Fatalf("the disagreeing agent was told to release the app %d time(s)", n)
	}

	good := newStandInAgent(t, "on", http.StatusOK)
	if err := clientFor(good.url).MakeAgentReadyForDockerCompose(context.Background()); err != nil {
		t.Fatalf("an agreeing agent was refused: %v", err)
	}
	if n := good.readies.Load(); n != 1 {
		t.Fatalf("the agreeing agent was released %d times, want 1", n)
	}
}

// A run that learns noise starts its native agent settling reads
// (--settle-reads), which sudo would otherwise have no way to tell it; any
// other run's argv does not change.
func TestNativeAgentArgs_CarrySettleReads(t *testing.T) {
	t.Cleanup(func() { _ = token.Cleanup() })
	a := New(zap.NewNop(), nil, &config.Config{})
	opts := models.SetupOptions{AgentPort: 26795, ProxyPort: 26796, DnsPort: 26797, AgentURI: "http://localhost:26795/agent", Mode: models.MODE_TEST}
	if args := a.nativeAgentArgs(opts); slices.Contains(args, "--settle-reads") {
		t.Fatalf("a run that learns no noise, yet the native agent argv carries --settle-reads: %v", args)
	}
	opts.SettleReads = true
	if args := a.nativeAgentArgs(opts); !slices.Contains(args, "--settle-reads") {
		t.Fatalf("a run that learns noise, yet the native agent argv lacks --settle-reads: %v", args)
	}
}
