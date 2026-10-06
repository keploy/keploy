package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// countingInspect answers ContainerInspect with resp (or err) and counts calls.
type countingInspect struct {
	kdocker.Client
	resp  container.InspectResponse
	err   error
	calls int
}

func (f *countingInspect) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	f.calls++
	return f.resp, f.err
}

func agentContainer(ports nat.PortMap, running bool, networkMode string) container.InspectResponse {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			State:      &container.State{Running: running},
			HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(networkMode)},
		},
		NetworkSettings: &container.NetworkSettings{NetworkSettingsBase: container.NetworkSettingsBase{Ports: ports}},
	}
}

func dockerAgent(cmdType utils.CmdType, fake *countingInspect) *AgentClient {
	conf := &config.Config{CommandType: string(cmdType), KeployContainer: "keploy-v3-test"}
	conf.Agent.AgentPort = 16789
	conf.Agent.ProxyPort = 16790
	conf.ProxyPort = 16790
	return &AgentClient{logger: zap.NewNop(), dockerClient: fake, conf: conf}
}

// listeningAgent is the keploy agent's /app/listen-addrs, which answers from the
// network namespace it shares with the app: addrs[port] are the addresses the
// app listens on for port, and a port it has no entry for gets status.
type listeningAgent struct {
	addrs  map[string][]string
	status int
	mu     sync.Mutex
	asked  []string
}

func (l *listeningAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	port := r.URL.Query().Get("port")
	l.mu.Lock()
	l.asked = append(l.asked, r.URL.Path+"?port="+port)
	l.mu.Unlock()
	addrs, ok := l.addrs[port]
	if !ok {
		w.WriteHeader(l.status)
		_, _ = w.Write([]byte(`{"error":"cannot tell"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(models.AppListenAddrs{Addrs: addrs})
}

// withAgent points a at an agent whose sockets are l.
func withAgent(t *testing.T, a *AgentClient, l *listeningAgent) *AgentClient {
	t.Helper()
	srv := httptest.NewServer(l)
	t.Cleanup(srv.Close)
	a.conf.Agent.AgentURI = srv.URL + "/agent"
	return a
}

// The advice has to be true for where the app listens, not only for what is
// published. docker forwards a published port to the container's own address,
// so a server that listens only on 127.0.0.1 inside the container (common for
// an internal one the app calls itself) is not reached from the host however
// the port is published: "Publish it: add -p 8097:8097" is wrong advice there,
// and once followed it used to silence the recording's warning while every
// replay of those tests still failed, on a reset instead of a refusal.
func TestUnreachableAppPortWhenTheAppListensOnlyOnLoopback(t *testing.T) {
	both := func(port string) []nat.PortBinding {
		return []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: port}, {HostIP: "::", HostPort: port}}
	}
	for _, tc := range []struct {
		name    string
		ports   nat.PortMap
		addrs   []string
		want    string
		notWant string
	}{
		{
			name:    "published as itself",
			ports:   nat.PortMap{"8095/tcp": both("8095"), "8097/tcp": both("8097")},
			addrs:   []string{"127.0.0.1"},
			want:    "the app listens on port 8097 only on 127.0.0.1 inside the container, which no published port reaches, and keploy sends each test from the host to port 8097, the port it was recorded on. keploy cannot replay these tests from the host while the app listens only there: the app would have to listen on 0.0.0.0:8097",
			notWant: "published as itself",
		},
		{
			name:  "not published",
			ports: nat.PortMap{"8095/tcp": both("8095")},
			addrs: []string{"127.0.0.1", "::1"},
			want:  "the app listens on port 8097 only on 127.0.0.1 and ::1 inside the container, which no published port reaches, and keploy sends each test from the host to port 8097, the port it was recorded on. keploy cannot replay these tests from the host while the app listens only there: the app would have to listen on 0.0.0.0:8097 and have port 8097 published as itself (add -p 8097:8097 to the docker command)",
		},
		{
			name:  "IPv4-mapped loopback is loopback",
			ports: nat.PortMap{"8097/tcp": both("8097")},
			addrs: []string{"::ffff:127.0.0.1"},
			want:  "the app listens on port 8097 only on 127.0.0.1 inside the container,",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &countingInspect{resp: agentContainer(tc.ports, true, "bridge")}
			l := &listeningAgent{addrs: map[string][]string{"8097": tc.addrs}, status: http.StatusNotFound}
			a := withAgent(t, dockerAgent(utils.DockerRun, fake), l)

			got := a.UnreachableAppPort(context.Background(), "localhost", 8097)
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("reason:\n got %q\nwant it to start %q", got, tc.want)
			}
			if strings.Contains(got, "Publish it") || (tc.notWant != "" && strings.Contains(got, tc.notWant)) {
				t.Errorf("reason %q advises what cannot help", got)
			}
			if want := []string{"/agent/app/listen-addrs?port=8097"}; !slices.Equal(l.asked, want) {
				t.Errorf("asked the agent %v, want %v", l.asked, want)
			}
		})
	}
}

// Where the app listens on a wildcard (or its container's own address), a
// published port reaches it: publishing is the whole fix, and a published
// port is reachable. When the agent cannot tell (nothing listens yet, or an
// agent without the question), the advice says what else publishing needs.
func TestUnreachableAppPortAdviceFollowsWhereTheAppListens(t *testing.T) {
	ports := nat.PortMap{"8095/tcp": {{HostIP: "0.0.0.0", HostPort: "8095"}}}
	const publish = "it is not published on the host (the docker command publishes 8095:8095), and keploy sends each test from the host to port 8096, the port it was recorded on. Publish it: add -p 8096:8096 to the docker command"
	const caveat = ", with the app listening on 0.0.0.0:8096 (a published port does not reach a socket on 127.0.0.1)"
	for _, tc := range []struct {
		name  string
		agent *listeningAgent
		want  string
	}{
		{name: "on a wildcard", agent: &listeningAgent{addrs: map[string][]string{"8096": {"0.0.0.0"}}}, want: publish},
		{name: "on the container's address and loopback", agent: &listeningAgent{addrs: map[string][]string{"8096": {"127.0.0.1", "172.17.0.2"}}}, want: publish},
		{name: "on ::", agent: &listeningAgent{addrs: map[string][]string{"8096": {"::"}}}, want: publish},
		{name: "not listening yet", agent: &listeningAgent{addrs: map[string][]string{"8096": {}}}, want: publish + caveat},
		{name: "an agent without the question", agent: &listeningAgent{status: http.StatusNotFound}, want: publish + caveat},
		{name: "an agent that cannot tell now", agent: &listeningAgent{status: http.StatusInternalServerError}, want: publish + caveat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &countingInspect{resp: agentContainer(ports, true, "bridge")}
			a := withAgent(t, dockerAgent(utils.DockerRun, fake), tc.agent)
			if got := a.UnreachableAppPort(context.Background(), "localhost", 8096); got != tc.want {
				t.Errorf("reason:\n got %q\nwant %q", got, tc.want)
			}
		})
	}

	published := nat.PortMap{"8096/tcp": {{HostIP: "0.0.0.0", HostPort: "8096"}}}
	for _, addrs := range [][]string{{"0.0.0.0"}, {}} {
		fake := &countingInspect{resp: agentContainer(published, true, "bridge")}
		a := withAgent(t, dockerAgent(utils.DockerRun, fake), &listeningAgent{addrs: map[string][]string{"8096": addrs}})
		if got := a.UnreachableAppPort(context.Background(), "localhost", 8096); got != "" {
			t.Errorf("published port with the app listening on %v reported unreachable: %q", addrs, got)
		}
	}
}

// blockingInspect is a docker daemon that never answers an inspect.
type blockingInspect struct{ kdocker.Client }

func (blockingInspect) ContainerInspect(ctx context.Context, _ string) (container.InspectResponse, error) {
	<-ctx.Done()
	return container.InspectResponse{}, ctx.Err()
}

// It is asked from a recording's insert loop, a replay's readiness gate and a
// refused test, often with a context that has no deadline. A docker daemon or
// agent that does not answer must cost each of them a bounded wait and an
// "I cannot tell", never the rest of the run.
func TestUnreachableAppPortBoundsItsOwnWait(t *testing.T) {
	defer func(d time.Duration) { appPortCheckTimeout = d }(appPortCheckTimeout)
	appPortCheckTimeout = 200 * time.Millisecond

	hungAgent := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hungAgent.Close()
	ports := nat.PortMap{"8095/tcp": {{HostIP: "0.0.0.0", HostPort: "8095"}}}

	for name, a := range map[string]*AgentClient{
		"docker daemon hangs": {logger: zap.NewNop(), dockerClient: blockingInspect{}, conf: dockerAgent(utils.DockerRun, nil).conf},
		"agent hangs":         dockerAgent(utils.DockerRun, &countingInspect{resp: agentContainer(ports, true, "bridge")}),
	} {
		t.Run(name, func(t *testing.T) {
			a.conf.Agent.AgentURI = hungAgent.URL + "/agent"
			done := make(chan string, 1)
			start := time.Now()
			go func() { done <- a.UnreachableAppPort(context.Background(), "localhost", 8096) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("UnreachableAppPort did not return while the docker daemon or agent hung")
			}
			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("took %s, want about appPortCheckTimeout", took)
			}
		})
	}
}

// `-p 8000-9000:8000-9000` publishes a thousand and one ports; the reason for
// one unpublished port is one log line and names a few of them.
func TestUnreachableAppPortListsAFewPublishes(t *testing.T) {
	ports := nat.PortMap{}
	for p := 8000; p <= 9000; p++ {
		ports[nat.Port(strconv.Itoa(p)+"/tcp")] = []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: strconv.Itoa(p)}}
	}
	fake := &countingInspect{resp: agentContainer(ports, true, "bridge")}
	a := withAgent(t, dockerAgent(utils.DockerRun, fake), &listeningAgent{addrs: map[string][]string{"9500": {"0.0.0.0"}}})
	got := a.UnreachableAppPort(context.Background(), "localhost", 9500)
	want := "it is not published on the host (the docker command publishes 8000:8000, 8001:8001, 8002:8002, 8003:8003, 8004:8004, 8005:8005, 8006:8006, 8007:8007, 8008:8008, 8009:8009 and 991 more), and"
	if !strings.HasPrefix(got, want) {
		t.Errorf("reason:\n got %q\nwant it to start %q", got, want)
	}
}

// The shape that found this: the app serves on 8095 and 50052,
// published, and calls its own :8096 inside the container. Tests recorded on
// 8096 are refused on every replay because nothing publishes it.
func TestUnreachableAppPortNamesAnUnpublishedPortAndTheFix(t *testing.T) {
	both := func(port string) []nat.PortBinding {
		// docker publishes on both families, so every port shows twice.
		return []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: port}, {HostIP: "::", HostPort: port}}
	}
	ports := nat.PortMap{
		"8095/tcp":  both("8095"),
		"50052/tcp": both("50052"),
		"16789/tcp": {{HostIP: "127.0.0.1", HostPort: "16789"}}, // keploy's agent port
		"16790/tcp": {{HostIP: "127.0.0.1", HostPort: "16790"}}, // keploy's proxy port
		"8096/udp":  both("8096"),                               // not a TCP publish
	}
	fake := &countingInspect{resp: agentContainer(ports, true, "bridge")}
	a := withAgent(t, dockerAgent(utils.DockerRun, fake), &listeningAgent{addrs: map[string][]string{
		"8096": {"0.0.0.0"}, "8095": {"0.0.0.0"}, "50052": {"::"},
	}})

	got := a.UnreachableAppPort(context.Background(), "localhost", 8096)
	want := "it is not published on the host (the docker command publishes 50052:50052, 8095:8095), and keploy sends each test from the host to port 8096, the port it was recorded on. Publish it: add -p 8096:8096 to the docker command"
	if got != want {
		t.Errorf("reason:\n got %q\nwant %q", got, want)
	}
	for _, port := range []uint16{8095, 50052} {
		if got := a.UnreachableAppPort(context.Background(), "localhost", port); got != "" {
			t.Errorf("published port %d reported unreachable: %q", port, got)
		}
	}
}

func TestUnreachableAppPortShapes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cmdType utils.CmdType
		host    string
		inspect container.InspectResponse
		err     error
		want    string // substring; "" means the answer must be ""
		asks    int    // ContainerInspect calls expected
	}{
		{
			name: "published on another host port", cmdType: utils.DockerRun, host: "localhost", asks: 1,
			inspect: agentContainer(nat.PortMap{"8096/tcp": {{HostPort: "18096"}}}, true, "bridge"),
			want:    "it is published on host port 18096 instead, and keploy sends each test from the host to port 8096, the port it was recorded on. Publish it on the same port: add -p 8096:8096 to the docker command",
		},
		{
			name: "the host port leads to another app port", cmdType: utils.DockerRun, host: "127.0.0.1", asks: 1,
			inspect: agentContainer(nat.PortMap{"9000/tcp": {{HostPort: "8096"}}}, true, "bridge"),
			want:    "host port 8096 is published to the app's port 9000 instead",
		},
		{
			name: "nothing published", cmdType: utils.DockerRun, host: "::1", asks: 1,
			inspect: agentContainer(nat.PortMap{}, true, "bridge"),
			want:    "(the docker command publishes no ports)",
		},
		{
			name: "compose names the service's ports", cmdType: utils.DockerCompose, host: "localhost", asks: 1,
			inspect: agentContainer(nat.PortMap{"8095/tcp": {{HostPort: "8095"}}}, true, "bridge"),
			want:    `(the docker compose service publishes 8095:8095), and keploy sends each test from the host to port 8096, the port it was recorded on. Publish it: add "8096:8096" to the app service's ports`,
		},
		{
			name: "--from-container names the container", cmdType: utils.FromContainer, host: "localhost", asks: 1,
			inspect: agentContainer(nat.PortMap{"8095/tcp": {{HostPort: "8095"}}}, true, "bridge"),
			want:    "(the container publishes 8095:8095), and keploy sends each test from the host to port 8096, the port it was recorded on. Publish it: re-create the container with -p 8096:8096",
		},
		{
			name: "published as itself", cmdType: utils.DockerRun, host: "localhost", asks: 1,
			inspect: agentContainer(nat.PortMap{"8096/tcp": {{HostIP: "127.0.0.1", HostPort: "8096"}}}, true, "bridge"),
		},
		{
			name: "host network: the app's ports are the host's", cmdType: utils.DockerRun, host: "localhost", asks: 1,
			inspect: agentContainer(nil, true, "host"),
		},
		{
			name: "a stopped container refuses for another reason", cmdType: utils.DockerRun, host: "localhost", asks: 1,
			inspect: agentContainer(nat.PortMap{}, false, "bridge"),
		},
		{
			name: "cannot inspect: cannot tell", cmdType: utils.DockerRun, host: "localhost", asks: 1,
			err: errors.New("Cannot connect to the Docker daemon"),
		},
		{
			name: "a test.host elsewhere is not the host's loopback", cmdType: utils.DockerRun, host: "10.0.0.7", asks: 0,
			inspect: agentContainer(nat.PortMap{}, true, "bridge"),
		},
		{
			name: "native mode has no container", cmdType: utils.Native, host: "localhost", asks: 0,
			inspect: agentContainer(nat.PortMap{}, true, "bridge"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &countingInspect{resp: tc.inspect, err: tc.err}
			got := dockerAgent(tc.cmdType, fake).UnreachableAppPort(context.Background(), tc.host, 8096)
			switch {
			case tc.want == "" && got != "":
				t.Errorf("want no reason, got %q", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("reason:\n got %q\nwant it to contain %q", got, tc.want)
			}
			if fake.calls != tc.asks {
				t.Errorf("ContainerInspect called %d times, want %d", fake.calls, tc.asks)
			}
		})
	}
}
