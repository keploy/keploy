package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// standInContainersAgent answers /agent/containers as an agent that takes a
// container list (takes) or reads Docker itself, and hands over each list it
// is given.
func standInContainersAgent(t *testing.T, takes bool) (url string, asked *atomic.Int32, got chan containerkey.Snapshot) {
	t.Helper()
	asked = &atomic.Int32{}
	got = make(chan containerkey.Snapshot, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/containers" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			asked.Add(1)
			if !takes {
				http.Error(w, "reads Docker itself", http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			var s containerkey.Snapshot
			if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			got <- s
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/agent", asked, got
}

// withNoDockerDaemon points the container lists this test's pushers give at a
// Docker daemon that is not there, for a test that counts the lists an agent is
// given. A pusher gives its agent every change to the daemon's containers as
// it happens (containerkey.Pusher), so on a host whose Docker starts and stops
// containers while the test runs (a CI runner's), the lists such a test counts
// would include changes it does not control: 3 or 4 lists where the session
// starts gave 2. With no daemon, a list says so, and nothing changes.
func withNoDockerDaemon(t *testing.T) {
	t.Helper()
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "docker.sock"))
}

// Under the proven switch a client gives an agent that takes container lists
// the list before its session starts: synchronously, so the list is there
// when the session request goes. It asks each agent once whether it takes
// one, and any client of this process gives the list to that agent.
func TestPushContainersGivesATakingAgentItsList(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	uri, asked, got := standInContainersAgent(t, true)
	t.Cleanup(func() { containerPushers.Delete(uri) })
	a := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	a.pushContainers(t.Context())
	var s containerkey.Snapshot
	select {
	case s = <-got:
	default:
		t.Fatal("pushContainers returned before the agent was given its list")
	}
	// Whatever this host's Docker is (none, or a daemon), the list names
	// the daemon it read, or why there is none, and its source.
	if s.Source == "" || (s.Endpoint == "" && s.Err == "") {
		t.Fatalf("the agent was given an empty list: %+v", s)
	}
	other := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	other.pushContainers(t.Context())
	select {
	case s2 := <-got:
		if s2.Source != s.Source {
			t.Fatalf("a second client of the agent gave it another source's list")
		}
	default:
		t.Fatal("a second client of the same agent gave it no list")
	}
	if asked.Load() != 1 {
		t.Fatalf("the agent was asked %d times whether it takes a list, want once", asked.Load())
	}
}

// A pusher gives its lists to the agent it was made for, even after its
// client is set up for another agent: that agent's own pusher, which leaves
// that agent's own container out, gives it its lists.
func TestAPusherGivesItsListsToItsOwnAgent(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	withNoDockerDaemon(t)
	uri, _, got := standInContainersAgent(t, true)
	t.Cleanup(func() { containerPushers.Delete(uri) })
	other, _, gotOther := standInContainersAgent(t, true)
	a := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	a.pushContainers(t.Context())
	<-got
	a.conf.Agent.AgentURI = other // set up for another agent
	v, ok := containerPushers.Load(uri)
	if !ok {
		t.Fatal("no pusher for the first agent")
	}
	if err := v.(*containerkey.Pusher).Push(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(gotOther) != 0 {
		t.Fatalf("the first agent's pusher gave %d lists to it and %d to the other agent", len(got), len(gotOther))
	}
}

// An agent that reads Docker itself is asked once and given nothing; without
// the switch nothing is asked at all.
func TestPushContainersLeavesOtherAgentsAlone(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	uri, asked, got := standInContainersAgent(t, false)
	t.Cleanup(func() { containerPushers.Delete(uri) })
	a := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	a.pushContainers(t.Context())
	a.pushContainers(t.Context())
	if asked.Load() != 1 || len(got) != 0 {
		t.Fatalf("an agent that reads Docker was asked %d times and given %d lists", asked.Load(), len(got))
	}

	t.Setenv(provenswitch.Env, "0")
	uri2, asked2, got2 := standInContainersAgent(t, true)
	b := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: uri2}})
	b.pushContainers(t.Context())
	if asked2.Load() != 0 || len(got2) != 0 {
		t.Fatal("without the switch an agent was asked about container lists")
	}
}

// postContainers says what the agent answered when it takes no list.
func TestPostContainersReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "this agent reads Docker itself", http.StatusConflict)
	}))
	t.Cleanup(srv.Close)
	a := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL + "/agent"}})
	if err := a.postContainers(t.Context(), a.conf.Agent.AgentURI, containerkey.Snapshot{}); err == nil {
		t.Fatal("a refused list was taken for given")
	}
}

// Under the proven switch, every session start gives a docker-mode agent its
// container list before the session request goes: the record stream
// (GetOutgoing), a replay's mock session (MockOutgoing), and the setting up of
// an agent compose started again, before its session is made again.
func TestEverySessionStartGivesTheListFirst(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	withNoDockerDaemon(t)
	client, agent := newRestartingClient(t, true)
	uri := client.conf.Agent.AgentURI
	t.Cleanup(func() { containerPushers.Delete(uri) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)
	order := func() []string {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return slices.Clone(agent.order)
	}
	const list = "POST /agent/containers"
	// listBefore: since the call at from, the agent was given a list before
	// call (the first of it from there).
	listBefore := func(from int, call string) int {
		t.Helper()
		got := order()[from:]
		at := slices.Index(got, call)
		if at < 0 {
			t.Fatalf("no %s after call %d: %v", call, from, order())
		}
		if !slices.Contains(got[:at], list) {
			t.Fatalf("the agent was not given its list before %s: %v", call, got)
		}
		return from + at + 1
	}

	outgoing, err := client.GetOutgoing(gctx, models.OutgoingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	recv(t, outgoing)
	next := listBefore(0, "POST /agent/outgoing")

	if err := client.MockOutgoing(gctx, models.OutgoingOptions{}); err != nil {
		t.Fatal(err)
	}
	next = listBefore(next, "POST /agent/mock")

	// The agent restarted: it holds no list. Its session is made again
	// (MockOutgoing, replayed) only after it is given one by the set-up
	// itself, and the replayed step gives it again.
	if err := client.setUpRestartedAgent(gctx); err != nil {
		t.Fatal(err)
	}
	got := order()[next:]
	first := slices.Index(got, list)
	for _, call := range []string{"POST /agent/outgoing", "POST /agent/mock"} { // the stream reconnected, the step replayed
		if at := slices.Index(got, call); at < 0 || first < 0 || first > at {
			t.Fatalf("the restarted agent was not given its list before %s: %v", call, got)
		}
	}
	at := slices.Index(got, "POST /agent/mock")
	if n := len(slices.DeleteFunc(slices.Clone(got[:at]), func(c string) bool { return c != list })); n != 2 {
		t.Fatalf("the restarted agent was given %d lists before its session was made again, want 2 (the set-up's and the replayed step's): %v", n, got)
	}
	client.session().end()
}

// A new agent set up at a URI is asked afresh whether it takes container
// lists: what was kept for the last agent there is dropped.
func TestASetUpAgentIsAskedAfresh(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	uri, asked, _ := standInContainersAgent(t, false)
	t.Cleanup(func() { containerPushers.Delete(uri) })
	a := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	a.pushContainers(t.Context())
	forgetContainers(uri)
	a.pushContainers(t.Context())
	if asked.Load() != 2 {
		t.Fatalf("the agent was asked %d times, want again after a new one was set up", asked.Load())
	}
}

// An agent older than container keys has no /agent/containers: the run says
// once, at Info, that it names container destinations by their address.
func TestAnAgentOlderThanContainerKeysIsSaidOnce(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	uri := srv.URL + "/agent"
	t.Cleanup(func() { containerPushers.Delete(uri) })
	core, logs := observer.New(zap.InfoLevel)
	a := New(zap.New(core), nil, &config.Config{Agent: config.Agent{AgentURI: uri}})
	a.pushContainers(t.Context())
	a.pushContainers(t.Context())
	if n := logs.FilterMessageSnippet("predates container keys").Len(); n != 1 {
		t.Fatalf("an older agent was said %d times, want once: %v", n, logs.All())
	}
}

// The CLI's ask for its agent's next settle reads the agent's answer; an
// agent with no such route (one that predates settles) or that reads Docker
// itself gives ErrNoSettles, so the pusher stops asking; anything else is
// an error, asked again.
func TestAskSettleReadsTheAgentsAnswer(t *testing.T) {
	var status atomic.Int32
	var query atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent/containers/settle" {
			http.NotFound(w, r)
			return
		}
		query.Store(r.URL.RawQuery)
		if code := int(status.Load()); code != http.StatusOK {
			http.Error(w, "no", code)
			return
		}
		_ = json.NewEncoder(w).Encode(containerkey.SettleAsk{Asked: 3, Boot: "b1"})
	}))
	t.Cleanup(srv.Close)
	a := New(zap.NewNop(), nil, &config.Config{})
	uri := srv.URL + "/agent"
	status.Store(http.StatusOK)
	got, err := a.askSettle(t.Context(), uri, "b0", 2)
	if err != nil || got != (containerkey.SettleAsk{Asked: 3, Boot: "b1"}) {
		t.Fatalf("askSettle = %+v, %v", got, err)
	}
	if q := query.Load().(string); q != "after=2&boot=b0" {
		t.Fatalf("asked with %q", q)
	}
	for _, code := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusConflict} {
		status.Store(int32(code))
		if _, err := a.askSettle(t.Context(), uri, "", 0); !errors.Is(err, containerkey.ErrNoSettles) {
			t.Fatalf("an agent answering %d gave %v, want ErrNoSettles", code, err)
		}
	}
	status.Store(http.StatusInternalServerError)
	if _, err := a.askSettle(t.Context(), uri, "", 0); err == nil || errors.Is(err, containerkey.ErrNoSettles) {
		t.Fatalf("an agent answering 500 gave %v, want an error to ask again on", err)
	}
	if _, err := a.askSettle(t.Context(), srv.URL+"/old", "", 0); !errors.Is(err, containerkey.ErrNoSettles) {
		t.Fatalf("an agent with no settle route gave %v", err)
	}
}
