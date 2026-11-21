package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// clockRunStarter is a startup hook that hands each agent run as its run id
// (agent.ClockRunStarter).
type clockRunStarter struct {
	agent.StartupHooks
	run string
}

func (s *clockRunStarter) ClockRun(context.Context) string { return s.run }

func useStartupHook(t *testing.T, h agent.StartupHook) {
	t.Helper()
	prev := agent.StartupAgentHook
	agent.StartupAgentHook = h
	t.Cleanup(func() { agent.StartupAgentHook = prev })
}

// runIDAgent is a stand-in agent whose health check answers healthy with this
// keploy's switch, and whose /hooks/clock-run answers takes (404 for an agent
// older than the route, 501 for one whose hooks keep no timeline). It notes
// every other request's path, in order, and the run ids it took. Like a
// real agent, it takes none before it is up: until its health check has
// answered healthy, the route answers 503 and the call is counted early.
type runIDAgent struct {
	url   string
	takes int
	mu    sync.Mutex
	// sick is how many health checks are still answered 503, as an agent
	// not up yet answers them; up says one has answered healthy.
	sick  int
	up    bool
	early int
	paths []string
	runs  []string
}

// newRunIDAgent is a stand-in agent that is up: a client is past its health
// handshake with it.
func newRunIDAgent(t *testing.T, takes int) *runIDAgent {
	t.Helper()
	ra := startRunIDAgent(t, takes, 0)
	ra.up = true
	return ra
}

// startRunIDAgent is a stand-in agent that has just started: its first sick
// health checks answer 503, and its route takes no run id until a check
// answers healthy.
func startRunIDAgent(t *testing.T, takes, sick int) *runIDAgent {
	t.Helper()
	ra := &runIDAgent{takes: takes, sick: sick}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/health") {
			ra.mu.Lock()
			notUp := ra.sick > 0
			if notUp {
				ra.sick--
			} else {
				ra.up = true
			}
			ra.mu.Unlock()
			if notUp {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set(provenswitch.HealthHeader, provenswitch.State())
			w.WriteHeader(http.StatusOK)
			return
		}
		ra.mu.Lock()
		ra.paths = append(ra.paths, r.URL.Path)
		up := ra.up
		if !up && r.URL.Path == "/hooks/clock-run" {
			ra.early++
		}
		ra.mu.Unlock()
		if r.URL.Path != "/hooks/clock-run" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if !up {
			http.Error(w, "the agent is not up yet", http.StatusServiceUnavailable)
			return
		}
		var req models.ClockRunReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch ra.takes {
		case http.StatusOK:
			ra.mu.Lock()
			ra.runs = append(ra.runs, req.Run)
			ra.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.StatusNotFound:
			// What chi answers for a route the agent does not have.
			http.NotFound(w, r)
		case http.StatusNotImplemented:
			http.Error(w, models.NoTimelineHooks, http.StatusNotImplemented)
		default:
			http.Error(w, "keploy's agent already marks its clock's timeline with another keploy run's id", ra.takes)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { sessions.Delete(srv.URL) })
	ra.url = srv.URL
	return ra
}

func (ra *runIDAgent) asked() ([]string, []string) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	return append([]string(nil), ra.paths...), append([]string(nil), ra.runs...)
}

// handedEarly is how many times the agent was handed the run id before its
// health check answered healthy.
func (ra *runIDAgent) handedEarly() int {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	return ra.early
}

// observedClient is a client of the agent at uri whose log is observed.
func observedClient(uri string) (*AgentClient, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: uri}}), logs
}

// runIDLines are the lines the client said of a run id the agent did not take.
func runIDLines(logs *observer.ObservedLogs) []observer.LoggedEntry {
	return logs.FilterMessageSnippet("took no run id").AllUntimed()
}

// A new keploy beside a new agent: the agent is handed the run id once it
// answers its health check healthy (an agent starting up answers it 503
// first, and takes no id then), before anything else is asked of it, and
// nothing is said.
func TestANewAgentIsHandedTheRunIDOnceItAnswers(t *testing.T) {
	for _, st := range switchStates {
		t.Run(st.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, st.env)
			useStartupHook(t, &clockRunStarter{run: "run-1"})
			ra := startRunIDAgent(t, http.StatusOK, 2)
			a, logs := observedClient(ra.url)
			if err := a.agentReady(context.Background(), nil, false, models.SetupOptions{}); err != nil {
				t.Fatalf("agentReady = %v", err)
			}
			if n := ra.handedEarly(); n != 0 {
				t.Fatalf("the agent was handed the run id %d time(s) before it answered its health check", n)
			}
			paths, runs := ra.asked()
			if len(paths) != 1 || paths[0] != "/hooks/clock-run" || len(runs) != 1 || runs[0] != "run-1" {
				t.Fatalf("the agent was asked %v with run ids %v, want only /hooks/clock-run with run-1", paths, runs)
			}
			if l := runIDLines(logs); len(l) != 0 {
				t.Fatalf("said %q, want nothing for an agent that took the run id", l[0].Message)
			}
		})
	}
}

// A new keploy beside an agent older than the route (a user-pinned agent
// image): the agent answers 404, the run goes on, and the client says so
// once, at Info, with the upgrade that fixes it, however many agents of the
// run it hands the id (a docker compose run's agent per test set).
func TestAnAgentOlderThanTheRouteRunsOnWithoutARunID(t *testing.T) {
	for _, st := range switchStates {
		t.Run(st.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, st.env)
			useStartupHook(t, &clockRunStarter{run: "run-1"})
			ra := startRunIDAgent(t, http.StatusNotFound, 2)
			a, logs := observedClient(ra.url)
			if err := a.agentReady(context.Background(), nil, false, models.SetupOptions{}); err != nil {
				t.Fatalf("agentReady = %v; an agent without the route must not fail the run", err)
			}
			for _, set := range []string{"test-set-0", "test-set-1"} {
				if err := a.BeforeTestSetCompose(context.Background(), "test-run-1", set, false); err != nil {
					t.Fatalf("BeforeTestSetCompose(%s) = %v; an agent without the route must not fail the set", set, err)
				}
			}
			lines := runIDLines(logs)
			if len(lines) != 1 {
				t.Fatalf("said the run id was not taken %d times, want once", len(lines))
			}
			l := lines[0]
			if l.Level != zapcore.InfoLevel || !strings.Contains(l.Message, "older than this keploy") || !strings.Contains(l.Message, "agent image of this keploy's version") {
				t.Fatalf("said %s %q, want an Info line naming an older agent and its upgrade", l.Level, l.Message)
			}
		})
	}
}

// A new keploy beside an agent of its own version whose hooks keep no clock
// timeline (OSS's own, a native macOS or Windows agent): the route answers
// 501 naming that. The agent is not older than this keploy and has no use
// for the id, so the run goes on, nothing is said above Debug, and no agent
// image is advised, however many agents of the run it hands the id.
func TestAnAgentWithoutTimelineHooksIsNotCalledOlder(t *testing.T) {
	for _, st := range switchStates {
		t.Run(st.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, st.env)
			useStartupHook(t, &clockRunStarter{run: "run-1"})
			ra := startRunIDAgent(t, http.StatusNotImplemented, 2)
			a, logs := observedClient(ra.url)
			if err := a.agentReady(context.Background(), nil, false, models.SetupOptions{}); err != nil {
				t.Fatalf("agentReady = %v; an agent without timeline hooks must not fail the run", err)
			}
			for _, set := range []string{"test-set-0", "test-set-1"} {
				if err := a.BeforeTestSetCompose(context.Background(), "test-run-1", set, false); err != nil {
					t.Fatalf("BeforeTestSetCompose(%s) = %v; an agent without timeline hooks must not fail the set", set, err)
				}
			}
			if paths, _ := ra.asked(); len(paths) != 5 || paths[0] != "/hooks/clock-run" {
				t.Fatalf("the agent was asked %v, want the run id handed once it answers and before each set's hook", paths)
			}
			for _, l := range logs.AllUntimed() {
				if l.Level > zapcore.DebugLevel {
					t.Fatalf("said %s %q, want nothing above Debug for an agent that keeps no timeline", l.Level, l.Message)
				}
			}
			if l := logs.FilterMessageSnippet("keeps no clock timeline").AllUntimed(); len(l) == 0 {
				t.Fatalf("said nothing of the agent that keeps no timeline, want a Debug line")
			}
		})
	}
}

// Under docker compose the run id is handed to each test set's agent before
// the set's hook, the first call made to it, and again before that hook when
// the session makes it again on an agent compose started again.
func TestAComposeSetsAgentIsHandedTheRunIDBeforeItsHook(t *testing.T) {
	for _, st := range switchStates {
		t.Run(st.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, st.env)
			useStartupHook(t, &clockRunStarter{run: "run-1"})
			ra := newRunIDAgent(t, http.StatusOK)
			a, _ := observedClient(ra.url)
			a.session().configure(true)
			if err := a.BeforeTestSetCompose(context.Background(), "test-run-1", "test-set-0", true); err != nil {
				t.Fatalf("BeforeTestSetCompose = %v", err)
			}
			s := a.session()
			s.mu.Lock()
			steps := append([]sessionStep(nil), s.steps...)
			s.mu.Unlock()
			if len(steps) != 1 || steps[0].kind != stepTestSetHook {
				t.Fatalf("the session kept %d step(s), want the set's hook", len(steps))
			}
			if err := steps[0].apply(context.Background()); err != nil {
				t.Fatalf("making the set's hook again = %v", err)
			}
			paths, runs := ra.asked()
			want := []string{"/hooks/clock-run", "/hooks/before-test-set-compose", "/hooks/clock-run", "/hooks/before-test-set-compose"}
			if strings.Join(paths, " ") != strings.Join(want, " ") {
				t.Fatalf("the agent was asked %v, want %v", paths, want)
			}
			if len(runs) != 2 || runs[0] != "run-1" || runs[1] != "run-1" {
				t.Fatalf("run ids %v, want run-1 twice", runs)
			}
		})
	}
}

// An agent that answers the route but refuses the id, or that cannot be
// reached, does not fail the run either: the client warns once, with the
// agent's reason. A call the run itself stopped says nothing.
func TestARunIDTheAgentDoesNotTakeIsSaidOnceAndTheRunGoesOn(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		useStartupHook(t, &clockRunStarter{run: "run-1"})
		ra := newRunIDAgent(t, http.StatusConflict)
		a, logs := observedClient(ra.url)
		a.handClockRun(context.Background())
		a.handClockRun(context.Background())
		lines := runIDLines(logs)
		if len(lines) != 1 || lines[0].Level != zapcore.WarnLevel {
			t.Fatalf("said %v, want one warning", lines)
		}
		for _, want := range []string{
			"the agent already holds another run's id: an earlier run's agent is still running",
			"already marks its clock's timeline with another keploy run's id",
			"Stop the earlier run's keploy agent",
		} {
			if !strings.Contains(lines[0].Message, want) {
				t.Fatalf("said %q, want it to name %q", lines[0].Message, want)
			}
		}
		if strings.Contains(lines[0].Message, "cannot tell this run's") {
			t.Fatalf("said %q: an agent that holds another run's id is not one that cannot tell the runs' timelines apart", lines[0].Message)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		useStartupHook(t, &clockRunStarter{run: "run-1"})
		ra := newRunIDAgent(t, http.StatusOK)
		a, logs := observedClient(ra.url)
		gone := countClientRequests(a)
		gone.fail = func() error { return context.DeadlineExceeded }
		a.handClockRun(context.Background())
		if lines := runIDLines(logs); len(lines) != 1 || lines[0].Level != zapcore.WarnLevel || !strings.Contains(lines[0].Message, "did not answer") {
			t.Fatalf("said %v, want one warning that the agent did not answer", lines)
		}
	})
	t.Run("stopped", func(t *testing.T) {
		useStartupHook(t, &clockRunStarter{run: "run-1"})
		ra := newRunIDAgent(t, http.StatusOK)
		a, logs := observedClient(ra.url)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		a.handClockRun(ctx)
		if lines := runIDLines(logs); len(lines) != 0 {
			t.Fatalf("said %q for a call the run stopped, want nothing", lines[0].Message)
		}
	})
}

// An agent of the run that answers the route with any other status took no
// id either: 401 from its auth, a 5xx, 503 from an agent not up yet, or a
// 501 whose body does not name models.NoTimelineHooks (a proxy in front of a
// remote agent). The client warns once, naming the status and the agent's
// own reason, never calls it an older agent, and never takes the answer for
// an agent that keeps no timeline.
func TestAnyOtherAnswerIsAWarningWithTheAgentsReason(t *testing.T) {
	for _, st := range switchStates {
		t.Run(st.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, st.env)
			useStartupHook(t, &clockRunStarter{run: "run-1"})
			for _, code := range []int{
				http.StatusUnauthorized, http.StatusInternalServerError,
				http.StatusServiceUnavailable, http.StatusNotImplemented,
			} {
				t.Run(strconv.Itoa(code), func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						http.Error(w, "the agent's own reason", code)
					}))
					t.Cleanup(srv.Close)
					t.Cleanup(func() { sessions.Delete(srv.URL) })
					a, logs := observedClient(srv.URL)
					a.handClockRun(context.Background())
					a.handClockRun(context.Background())
					lines := runIDLines(logs)
					want := fmt.Sprintf("it answered %d: the agent's own reason", code)
					if len(lines) != 1 || lines[0].Level != zapcore.WarnLevel || !strings.Contains(lines[0].Message, want) {
						t.Fatalf("said %v, want one warning naming %q", lines, want)
					}
					if strings.Contains(lines[0].Message, "older than this keploy") {
						t.Fatalf("said %q: an agent that answered %d is not an older one", lines[0].Message, code)
					}
					if l := logs.FilterMessageSnippet("keeps no clock timeline").AllUntimed(); len(l) != 0 {
						t.Fatalf("said %q: an agent that answered %d is not one that keeps no timeline", l[0].Message, code)
					}
				})
			}
		})
	}
}

// A keploy whose startup hook hands no run id (OSS's own, or one with none
// for this run) asks the agent nothing, and a client with no agent address
// to ask hands nothing: neither says anything.
func TestNoRunIDIsHandedWithoutOne(t *testing.T) {
	t.Run("no agent address", func(t *testing.T) {
		useStartupHook(t, &clockRunStarter{run: "run-1"})
		a, logs := observedClient("")
		a.handClockRun(context.Background())
		if l := runIDLines(logs); len(l) != 0 {
			t.Fatalf("said %q with no agent to hand the run id to, want nothing", l[0].Message)
		}
	})
	for name, h := range map[string]agent.StartupHook{
		"no capability": &agent.StartupHooks{},
		"no id":         &clockRunStarter{},
	} {
		t.Run(name, func(t *testing.T) {
			useStartupHook(t, h)
			ra := newRunIDAgent(t, http.StatusOK)
			a, logs := observedClient(ra.url)
			if err := a.agentReady(context.Background(), nil, false, models.SetupOptions{}); err != nil {
				t.Fatalf("agentReady = %v", err)
			}
			if paths, _ := ra.asked(); len(paths) != 0 {
				t.Fatalf("the agent was asked %v, want nothing besides its health", paths)
			}
			if l := runIDLines(logs); len(l) != 0 {
				t.Fatalf("said %q, want nothing", l[0].Message)
			}
		})
	}
}
