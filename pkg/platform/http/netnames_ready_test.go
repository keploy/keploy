package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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

// nameDocker is a Docker client that answers only the inspects the name list
// is read from.
type nameDocker struct {
	kdocker.Client
	f fakeNameInspector
}

func (d nameDocker) ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error) {
	return d.f.ContainerInspect(ctx, id)
}

func (d nameDocker) NetworkInspect(ctx context.Context, id string, o network.InspectOptions) (network.Inspect, error) {
	return d.f.NetworkInspect(ctx, id, o)
}

// A launcher that never calls MockOutgoing still gives a docker-mode agent the
// names its replay's networks answer, once the agent takes its release:
// enterprise's in-Pod replay stack brings compose up through this client and
// marks the agent ready, while k8s-proxy, not this client, drives the agent's
// mocking. Without the list such an agent answers a compose service's name
// (here "db") itself instead of forwarding it to docker's resolver. A
// native-mode client gives none: it has no network of its own to list.
func TestAgentReadyGivesADockerModeAgentItsNetworkNames(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	inspect := fakeNameInspector{
		containers: map[string]container.InspectResponse{
			"keploy-v3-agent": onNetworks(map[string]*network.EndpointSettings{"stack_default": {}}),
			"dbid":            onNetworks(map[string]*network.EndpointSettings{"stack_default": {DNSNames: []string{"stack-db-1", "db"}}}),
		},
		networks: map[string]network.Inspect{
			"stack_default": {Containers: map[string]network.EndpointResource{"dbid": {Name: "stack-db-1"}}},
		},
	}
	for _, tc := range []struct {
		name      string
		cmdType   utils.CmdType
		wantNames bool
	}{
		{"docker compose", utils.DockerCompose, true},
		{"native", utils.Native, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu     sync.Mutex
				lists  []netnames.List
				served = make(chan struct{})
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/agent/ready"):
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, models.NetworkNamesPath):
					var l netnames.List
					if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					mu.Lock()
					lists = append(lists, l)
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
				case strings.HasSuffix(r.URL.Path, models.NetworkNamesAskPath):
					// Held open, as the agent holds it until a name misses.
					select {
					case <-r.Context().Done():
					case <-served:
					}
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			uri := srv.URL + "/agent"
			t.Cleanup(func() { forgetNetworkNames(uri); close(served) })

			a := New(zap.NewNop(), nameDocker{f: inspect}, &config.Config{
				KeployContainer: "keploy-v3-agent",
				CommandType:     string(tc.cmdType),
				Agent:           config.Agent{AgentURI: uri},
			})
			if err := a.MakeAgentReadyForDockerCompose(context.Background()); err != nil {
				t.Fatalf("MakeAgentReadyForDockerCompose = %v", err)
			}

			mu.Lock()
			got := slices.Clone(lists)
			mu.Unlock()
			if !tc.wantNames {
				if len(got) != 0 {
					t.Fatalf("a %s agent was given %d name list(s); it has no network of its own", tc.name, len(got))
				}
				return
			}
			if len(got) != 1 || !slices.Contains(got[0].Names, "db") || !slices.Contains(got[0].Names, "stack-db-1") {
				t.Fatalf("lists given on the agent's release = %+v, want one holding db and stack-db-1", got)
			}
			// The agent's asks for a fresh list are answered from then on: a
			// container started after the release is found on a miss.
			if v, ok := namePushers.Load(uri); !ok || v == noNamePusher {
				t.Fatalf("nothing answers the agent's asks for a fresh list after its release (pusher %v)", v)
			}
		})
	}
}
