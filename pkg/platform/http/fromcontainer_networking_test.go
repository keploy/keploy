package http

import (
	"context"
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// inspectOnly answers ContainerInspect and nothing else. The embedded interface
// is nil, so any other call panics rather than returning a zero value that
// quietly changes what the test means.
type inspectOnly struct {
	kdocker.Client
	resp container.InspectResponse
}

func (f inspectOnly) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return f.resp, nil
}

func agentFor(resp container.InspectResponse) *AgentClient {
	return &AgentClient{logger: zap.NewNop(), dockerClient: inspectOnly{resp: resp}}
}

// The agent publishes on the app's behalf, so these specs are spliced into the
// agent's own `docker run`. An unparseable one takes the whole session down
// before anything starts.
func TestAppNetworkingFromContainerPortSpecs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bindings nat.PortMap
		publish  bool
		want     []string
	}{
		{
			name:     "a plain published port",
			bindings: nat.PortMap{"9410/tcp": []nat.PortBinding{{HostPort: "19410"}}},
			want:     []string{"-p 19410:9410"},
		},
		{
			name:     "an interface-scoped binding keeps its host address",
			bindings: nat.PortMap{"9410/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "19410"}}},
			want:     []string{"-p 127.0.0.1:19410:9410"},
		},
		{
			// Docker stores an IPv6 host binding as the bare address, and
			// "::1:5353:53" is rejected by the port parser as having too many
			// colons - the agent's docker run then fails to start at all.
			name:     "an IPv6 host address is bracketed",
			bindings: nat.PortMap{"53/udp": []nat.PortBinding{{HostIP: "::1", HostPort: "5353"}}},
			want:     []string{"-p [::1]:5353:53/udp"},
		},
		{
			name:     "a non-tcp protocol is carried",
			bindings: nat.PortMap{"53/udp": []nat.PortBinding{{HostPort: "5353"}}},
			want:     []string{"-p 5353:53/udp"},
		},
		{
			// `docker run -P` leaves PortBindings empty, so reading bindings
			// alone republishes nothing and the app is unreachable all session.
			name:    "publish-all is carried even with no explicit bindings",
			publish: true,
			want:    []string{"-P"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := agentFor(container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					HostConfig: &container.HostConfig{PortBindings: tc.bindings, PublishAllPorts: tc.publish},
				},
			})
			ports, _, err := a.appNetworkingFromContainer(context.Background(), "app")
			if err != nil {
				t.Fatalf("appNetworkingFromContainer: %v", err)
			}
			if !reflect.DeepEqual(ports, tc.want) {
				t.Errorf("ports = %v, want %v", ports, tc.want)
			}
		})
	}
}

// Every spec produced above has to be one docker itself accepts.
func TestAppNetworkingPortSpecsAreParseable(t *testing.T) {
	a := agentFor(container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			HostConfig: &container.HostConfig{PortBindings: nat.PortMap{
				"9410/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "19410"}},
				"53/udp":   []nat.PortBinding{{HostIP: "::1", HostPort: "5353"}},
			}},
		},
	})
	ports, _, err := a.appNetworkingFromContainer(context.Background(), "app")
	if err != nil {
		t.Fatalf("appNetworkingFromContainer: %v", err)
	}
	for _, p := range ports {
		spec := p[len("-p "):]
		if _, _, err := nat.ParsePortSpecs([]string{spec}); err != nil {
			t.Errorf("docker cannot parse %q: %v", spec, err)
		}
	}
}

// The predefined networks are not joinable the way a user network is: bridge is
// the default anyway, and host/none are incompatible with the namespace sharing
// keploy needs. Passing them to the agent's `docker run` fails the run.
func TestAppNetworkingFromContainerSkipsPredefinedNetworks(t *testing.T) {
	a := agentFor(container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{}},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"bridge": {}, "host": {}, "none": {}, "backend": {}, "frontend": {},
		}},
	})
	_, networks, err := a.appNetworkingFromContainer(context.Background(), "app")
	if err != nil {
		t.Fatalf("appNetworkingFromContainer: %v", err)
	}
	// Sorted so a re-run produces the same agent command.
	if !reflect.DeepEqual(networks, []string{"backend", "frontend"}) {
		t.Errorf("networks = %v, want the user networks only, sorted", networks)
	}
}
