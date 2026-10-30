package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/netnames"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// liveNameDocker is a Docker client whose containers can change while it is
// read: a container started after the last list.
type liveNameDocker struct {
	kdocker.Client
	mu sync.Mutex
	f  fakeNameInspector
}

func (d *liveNameDocker) set(f fakeNameInspector) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.f = f
}

func (d *liveNameDocker) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	d.mu.Lock()
	f := d.f
	d.mu.Unlock()
	return f.ContainerInspect(ctx, id)
}

func (d *liveNameDocker) NetworkInspect(ctx context.Context, id string, o network.InspectOptions) (network.Inspect, error) {
	d.mu.Lock()
	f := d.f
	d.mu.Unlock()
	return f.NetworkInspect(ctx, id, o)
}

// stackNames is a compose stack whose network holds the given containers,
// each by its id, under its own name and the service name its id names.
func stackNames(ids ...string) fakeNameInspector {
	f := fakeNameInspector{
		containers: map[string]container.InspectResponse{
			"keploy-v3-agent": onNetworks(map[string]*network.EndpointSettings{"stack_default": {}}),
		},
		networks: map[string]network.Inspect{"stack_default": {Containers: map[string]network.EndpointResource{}}},
	}
	for _, id := range ids {
		f.containers[id] = onNetworks(map[string]*network.EndpointSettings{"stack_default": {DNSNames: []string{"stack-" + id + "-1", id}}})
		f.networks["stack_default"].Containers[id] = network.EndpointResource{Name: "stack-" + id + "-1"}
	}
	return f
}

// nameAgent is a docker-mode agent's name routes over a Store of its own, as
// the agent's are over netnames.Default (routes/netnames.go). restart is the
// agent's process replaced: the new one holds no list and no CLI's stream of
// asks, and the old one's streams end with it.
// opened has each stream of asks' after, as the CLI opens the stream.
type nameAgent struct {
	mu     sync.Mutex
	store  *netnames.Store
	gone   context.Context
	kill   context.CancelFunc
	opened chan uint64
}

func newNameAgent(t *testing.T) (*nameAgent, string) {
	t.Helper()
	ag := &nameAgent{opened: make(chan uint64, 64)}
	ag.restart()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store, gone := ag.current()
		switch {
		case strings.HasSuffix(r.URL.Path, "/health"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/mock"):
			_ = json.NewEncoder(w).Encode(models.AgentResp{IsSuccess: true})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, models.NetworkNamesPath):
			var l netnames.List
			if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			store.Apply(l)
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, models.NetworkNamesAskPath):
			after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
			select {
			case ag.opened <- after:
			default:
			}
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			go func() {
				select {
				case <-gone.Done():
				case <-ctx.Done():
				}
				cancel()
			}()
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			enc := json.NewEncoder(w)
			store.Serve(ctx, after, func(a netnames.Ask) error {
				if err := enc.Encode(a); err != nil {
					return err
				}
				w.(http.Flusher).Flush()
				return nil
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() { ag.mu.Lock(); ag.kill(); ag.mu.Unlock(); srv.Close() })
	return ag, srv.URL + "/agent"
}

func (ag *nameAgent) current() (*netnames.Store, context.Context) {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	return ag.store, ag.gone
}

func (ag *nameAgent) restart() {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.kill != nil {
		ag.kill()
	}
	ag.store = netnames.New()
	ag.gone, ag.kill = context.WithCancel(context.Background())
}

// answered waits for a CLI's stream of asks on store, then asks it for a
// fresh list, as a DNS miss for name does, and reports whether name is the
// network's own after it.
func answered(store *netnames.Store, name string) bool {
	for {
		present, changed := store.Present()
		if present {
			break
		}
		<-changed
	}
	store.Refresh(context.Background())
	return store.Owns(name)
}

// A docker-mode agent is given the names its replay's networks answer at a
// test set's start (MockOutgoing), and again when docker compose started it
// again mid-session; each time something answers its asks for a fresh list
// from then on. Without either, the agent answers a compose service's name
// itself, not docker's resolver: a false red, after a restart above all, the
// stressed-infra case.
func TestADockerModeAgentHoldsItsNetworkNamesAcrossARestart(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	ag, uri := newNameAgent(t)
	t.Cleanup(func() { forgetNetworkNames(uri) })
	docker := &liveNameDocker{}
	docker.set(stackNames("db"))
	a := New(zap.NewNop(), docker, &config.Config{
		KeployContainer: "keploy-v3-agent",
		CommandType:     string(utils.DockerCompose),
		Agent:           config.Agent{AgentURI: uri},
	})
	// Not a restartable session: the set-up of a restarted agent is then all
	// that gives the new agent its list, not a replayed MockOutgoing.
	storeSession(t, uri, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := a.MockOutgoing(ctx, models.OutgoingOptions{}); err != nil {
		t.Fatalf("MockOutgoing = %v", err)
	}
	store, _ := ag.current()
	if !store.Given() || !store.Owns("db") {
		t.Fatal("a test set's start gave the agent no list holding db")
	}
	docker.set(stackNames("db", "cache"))
	if !answered(store, "cache") {
		t.Fatal("cache, started after the test set's start, is not found on a miss: nothing answers the agent's asks")
	}

	ag.restart()
	docker.set(stackNames("db", "cache", "queue"))
	if err := a.setUpRestartedAgent(ctx); err != nil {
		t.Fatalf("setUpRestartedAgent = %v", err)
	}
	store, _ = ag.current()
	if !store.Given() || !store.Owns("queue") {
		t.Fatal("the restarted agent was given no list holding queue")
	}
	docker.set(stackNames("db", "cache", "queue", "late"))
	if !answered(store, "late") {
		t.Fatal("late, started after the restart, is not found on a miss: nothing answers the restarted agent's asks")
	}
}
