package http

import (
	"context"
	"encoding/json"
	"errors"
	"net"
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
	"go.keploy.io/server/v3/pkg"
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
// app listens on for port, raw[port] is a 200 body sent as it is, fail[port]
// is a status it fails with, and a port it has no entry for gets status.
type listeningAgent struct {
	addrs  map[string][]string
	raw    map[string]string
	fail   map[string]int
	status int
	mu     sync.Mutex
	asked  []string
}

func (l *listeningAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	port := r.URL.Query().Get("port")
	l.mu.Lock()
	l.asked = append(l.asked, r.URL.Path+"?port="+port)
	l.mu.Unlock()
	if body, ok := l.raw[port]; ok {
		_, _ = w.Write([]byte(body))
		return
	}
	addrs, ok := l.addrs[port]
	if status, failing := l.fail[port]; failing || !ok {
		if !failing {
			status = l.status
		}
		w.WriteHeader(status)
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

			got := a.UnreachableAppPort(context.Background(), "localhost", 8097, 8097)
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
			if got := a.UnreachableAppPort(context.Background(), "localhost", 8096, 8096); got != tc.want {
				t.Errorf("reason:\n got %q\nwant %q", got, tc.want)
			}
		})
	}

	published := nat.PortMap{"8096/tcp": {{HostIP: "0.0.0.0", HostPort: "8096"}}}
	for _, addrs := range [][]string{{"0.0.0.0"}, {}} {
		fake := &countingInspect{resp: agentContainer(published, true, "bridge")}
		a := withAgent(t, dockerAgent(utils.DockerRun, fake), &listeningAgent{addrs: map[string][]string{"8096": addrs}})
		if got := a.UnreachableAppPort(context.Background(), "localhost", 8096, 8096); got != "" {
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
			go func() { done <- a.UnreachableAppPort(context.Background(), "localhost", 8096, 8096) }()
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
	got := a.UnreachableAppPort(context.Background(), "localhost", 9500, 9500)
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

	got := a.UnreachableAppPort(context.Background(), "localhost", 8096, 8096)
	want := "it is not published on the host (the docker command publishes 50052:50052, 8095:8095), and keploy sends each test from the host to port 8096, the port it was recorded on. Publish it: add -p 8096:8096 to the docker command"
	if got != want {
		t.Errorf("reason:\n got %q\nwant %q", got, want)
	}
	for _, port := range []uint16{8095, 50052} {
		if got := a.UnreachableAppPort(context.Background(), "localhost", port, port); got != "" {
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
			// A port map sends the tests recorded on 8096 there and no
			// others; --port would send every test of every set there.
			name: "published on another host port", cmdType: utils.DockerRun, host: "localhost", asks: 1,
			inspect: agentContainer(nat.PortMap{"8096/tcp": {{HostPort: "18096"}}}, true, "bridge"),
			want:    "it is published on host port 18096 instead, and keploy sends each test from the host to port 8096, the port it was recorded on. Publish it on the same port (add -p 8096:8096 to the docker command), or send these tests to host port 18096 with a port map in keploy.yml (test.replaceWith.global.port: {8096: 18096})",
		},
		{
			// Where the app listens decides it (see
			// TestUnreachableAppPortForTestsSentToAnotherHostPort); no agent
			// says, so nothing can be said.
			name: "the host port leads to another app port: cannot tell without the agent", cmdType: utils.DockerRun, host: "127.0.0.1", asks: 1,
			inspect: agentContainer(nat.PortMap{"9000/tcp": {{HostPort: "8096"}}}, true, "bridge"),
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
			got := dockerAgent(tc.cmdType, fake).UnreachableAppPort(context.Background(), tc.host, 8096, 8096)
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

// A compose file that publishes "18067:8080", replayed with --port 18067 so the
// tests go to the host port that leads to the app's port 8080, where they were
// recorded. That reaches the app. The check used to compare host port 18067
// with itself, call the publish to 8080 a publish elsewhere, and so fail the
// first test of every replay that found the app still starting at once,
// advising "map host port 18067 to the app's port 18067", where re-sending, or
// the readiness gate it also switched off, would have waited for the app. The
// agent is asked where the app listens on the container port the host port
// leads to. A host port that leads to another container port than the recorded
// one is only wrong when the app listens there only on loopback, or when the
// agent sees nothing listening there and the app listening on the recorded
// port: an app recorded on 8080 can be run on 9000 and replayed with --port to
// it, and an agent that cannot tell where the app listens says nothing.
func TestUnreachableAppPortForTestsSentToAnotherHostPort(t *testing.T) {
	compose := func(hostPort, appPort string) nat.PortMap {
		return nat.PortMap{nat.Port(appPort + "/tcp"): {{HostIP: "0.0.0.0", HostPort: hostPort}, {HostIP: "::", HostPort: hostPort}}}
	}
	const sends = "keploy sends these tests from the host to port 18067, for the app's port 8080 they were recorded on"
	const caveat = ", with the app listening on 0.0.0.0:8080 (a published port does not reach a socket on 127.0.0.1)"
	// A port map keyed on the recorded port does not move a test that
	// test.port, a protocol port or a replaceWith rule already sends
	// elsewhere, and the reason cannot tell which of them did.
	const redirected = " in place of the setting that sends them to port 18067 now"
	// keep keeps the publish to 9000, which other tests may need.
	const keep = `, or, to keep "18067:9000", publish the app's port 8080 on another host port too and send these tests there with a port map in keploy.yml (test.replaceWith.global.port)` + redirected
	const wrongPort = `host port 18067 is published to the container's port 9000, where nothing listens, instead of the app's port 8080 these tests were recorded on, where the app listens, and ` + sends + `. Change "18067:9000" to "18067:8080" in the app service's ports`
	const loopbackThere = "host port 18067 is published to the container's port 9000, where the app listens only on 127.0.0.1, which no published port reaches, and " + sends +
		`. keploy cannot replay these tests from the host through it: the app would have to listen on 0.0.0.0:9000, or host port 18067 be published to the app's port 8080 these tests were recorded on instead (change "18067:9000" to "18067:8080" in the app service's ports), with the app listening on 0.0.0.0:8080`
	// A test that does not say the app's port it was recorded on has the
	// port it is sent to for one.
	const sends0 = "keploy sends these tests from the host to port 18067"
	for _, tc := range []struct {
		name    string
		ports   nat.PortMap
		appPort uint16
		addrs   map[string][]string // the agent's answer, by port; a port it has no entry for: cannot tell
		raw     map[string]string   // an answer it sends as it is
		fail    map[string]int      // a status it fails with
		want    string
		asked   []string // the ports the agent is asked about
	}{
		{
			name: "published to the app's port, the app still starting", ports: compose("18067", "8080"), appPort: 8080,
			addrs: map[string][]string{"8080": {}}, asked: []string{"8080"},
		},
		{
			name: "published to the app's port, the app listening", ports: compose("18067", "8080"), appPort: 8080,
			addrs: map[string][]string{"8080": {"0.0.0.0"}}, asked: []string{"8080"},
		},
		{
			name: "published to the app's port, an agent that cannot tell", ports: compose("18067", "8080"), appPort: 8080,
			asked: []string{"8080"},
		},
		{
			name: "published to the app's port, the app listening only on loopback", ports: compose("18067", "8080"), appPort: 8080,
			addrs: map[string][]string{"8080": {"127.0.0.1"}}, asked: []string{"8080"},
			want: "host port 18067 is published to the app's port 8080, where the app listens only on 127.0.0.1 inside the container, which no published port reaches, and " + sends + ". keploy cannot replay these tests from the host while the app listens only there: the app would have to listen on 0.0.0.0:8080",
		},
		{
			// A second publish of host port 18067 is refused ("port is
			// already allocated"): the one there has to change.
			name: "published to another container port, the app listening on its recorded port", ports: compose("18067", "9000"), appPort: 8080,
			addrs: map[string][]string{"9000": {}, "8080": {"0.0.0.0"}}, asked: []string{"9000", "8080"},
			want: wrongPort + keep,
		},
		{
			name: "published to another container port, which the recorded port is also published on", appPort: 8080,
			ports: nat.PortMap{"9000/tcp": {{HostPort: "18067"}}, "8080/tcp": {{HostPort: "18057"}}},
			addrs: map[string][]string{"9000": {}, "8080": {"0.0.0.0"}}, asked: []string{"9000", "8080"},
			want: wrongPort + `, or send these tests to host port 18057 with a port map in keploy.yml (test.replaceWith.global.port: {8080: 18057})` + redirected,
		},
		{
			name: "published to another container port, the app listening only on loopback on its recorded port", ports: compose("18067", "9000"), appPort: 8080,
			addrs: map[string][]string{"9000": {}, "8080": {"127.0.0.1"}}, asked: []string{"9000", "8080"},
			want: wrongPort + `, with the app listening on 0.0.0.0:8080 (it listens there only on 127.0.0.1, which no published port reaches)` + keep,
		},
		{
			// Recorded natively on 8080, run in a container on 9000 and
			// replayed with --port to the host port published to it.
			name: "published to another container port, where the app listens now", ports: compose("18067", "9000"), appPort: 8080,
			addrs: map[string][]string{"9000": {"0.0.0.0"}}, asked: []string{"9000"},
		},
		{
			name: "published to another container port, the app listening on neither yet", ports: compose("18067", "9000"), appPort: 8080,
			addrs: map[string][]string{"9000": {}, "8080": {}}, asked: []string{"9000", "8080"},
		},
		{
			// The app may be listening there: what it does on its recorded
			// port does not say.
			name: "published to another container port, an agent that cannot tell there", ports: compose("18067", "9000"), appPort: 8080,
			addrs: map[string][]string{"8080": {"0.0.0.0"}}, fail: map[string]int{"9000": http.StatusInternalServerError}, asked: []string{"9000"},
		},
		{
			name: "published to another container port, an address the agent's answer there does not read as", ports: compose("18067", "9000"), appPort: 8080,
			addrs: map[string][]string{"8080": {"0.0.0.0"}}, raw: map[string]string{"9000": `{"addrs":["not-an-address"]}`}, asked: []string{"9000"},
		},
		{
			name: "published to another container port, an agent answer that is not JSON there", ports: compose("18067", "9000"), appPort: 8080,
			addrs: map[string][]string{"8080": {"0.0.0.0"}}, raw: map[string]string{"9000": `{"addrs":`}, asked: []string{"9000"},
		},
		{
			name: "published to another container port where nothing listens, an agent that cannot tell on the recorded port", ports: compose("18067", "9000"), appPort: 8080,
			addrs: map[string][]string{"9000": {}}, fail: map[string]int{"8080": http.StatusInternalServerError}, asked: []string{"9000", "8080"},
		},
		{
			// The app listens on a third port: on 9000 it may yet listen, as
			// apps open their servers one after another, and nothing seen
			// now tells "not yet" from "never".
			name: "published to another container port where nothing listens, the app on neither that nor its recorded port", ports: compose("18067", "9000"), appPort: 8080,
			addrs: map[string][]string{"9000": {}, "8080": {}, "8081": {"0.0.0.0"}}, asked: []string{"9000", "8080"},
		},
		{
			name: "published to another container port, where the app listens only on loopback", ports: compose("18067", "9000"), appPort: 8080,
			addrs: map[string][]string{"9000": {"127.0.0.1"}}, asked: []string{"9000", "8080"},
			want: loopbackThere,
		},
		{
			name: "published to another container port, where the app listens only on loopback, as on its recorded port", ports: compose("18067", "9000"), appPort: 8080,
			addrs: map[string][]string{"9000": {"127.0.0.1"}, "8080": {"127.0.0.1"}}, asked: []string{"9000", "8080"},
			want: loopbackThere + " (it listens there only on 127.0.0.1)",
		},
		{
			// Listening there on 0.0.0.0 would send the tests to a server
			// they were not recorded on: the publish is what is wrong.
			name: "published to another container port, where the app listens only on loopback, and on its recorded port where a publish reaches", ports: compose("18067", "9000"), appPort: 8080,
			addrs: map[string][]string{"9000": {"127.0.0.1"}, "8080": {"0.0.0.0"}}, asked: []string{"9000", "8080"},
			want: `host port 18067 is published to the container's port 9000, where the app listens only on 127.0.0.1, which no published port reaches, instead of the app's port 8080 these tests were recorded on, where it listens on an address one does, and ` + sends +
				`. Change "18067:9000" to "18067:8080" in the app service's ports` + keep,
		},
		{
			name: "not published, the app's port published on another host port", ports: compose("18057", "8080"), appPort: 8080,
			addrs: map[string][]string{"8080": {}}, asked: []string{"8080"},
			want: `host port 18067 is not published, while the app's port 8080 is published on host port 18057, and ` + sends + `. Publish host port 18067 to it (add "18067:8080" to the app service's ports), or send these tests to host port 18057 with a port map in keploy.yml (test.replaceWith.global.port: {8080: 18057})` +
				redirected + caveat,
		},
		{
			name: "not published at all", ports: compose("18057", "9000"), appPort: 8080,
			addrs: map[string][]string{"8080": {"0.0.0.0"}}, asked: []string{"8080"},
			want: `host port 18067 is not published (the docker compose service publishes 18057:9000), and ` + sends + `. Publish it: add "18067:8080" to the app service's ports`,
		},
		{
			name: "a test that does not say its port, published to its own number", ports: compose("18067", "18067"),
			addrs: map[string][]string{"18067": {}}, asked: []string{"18067"},
		},
		{
			name: "a test that does not say its port, published to another container port, where the app listens", ports: compose("18067", "8080"),
			addrs: map[string][]string{"8080": {"0.0.0.0"}}, asked: []string{"8080"},
		},
		{
			name: "a test that does not say its port, published to another container port, the app on neither yet", ports: compose("18067", "8080"),
			addrs: map[string][]string{"8080": {}, "18067": {}}, asked: []string{"8080", "18067"},
		},
		{
			// The rule for a known app port, with the port the test is sent
			// to for one.
			name: "a test that does not say its port, published to another container port where nothing listens, the app on its own number", ports: compose("18067", "8080"),
			addrs: map[string][]string{"8080": {}, "18067": {"0.0.0.0"}}, asked: []string{"8080", "18067"},
			want: `host port 18067 is published to the container's port 8080, where nothing listens, instead of the container's port 18067, where the app listens, and ` + sends0 +
				`. Change "18067:8080" to "18067:18067" in the app service's ports, or, to keep "18067:8080", publish the container's port 18067 on another host port too and send these tests there with a port map in keploy.yml (test.replaceWith.global.port)`,
		},
		{
			// Nothing says the tests belong anywhere else.
			name: "a test that does not say its port, published where the app listens only on loopback", ports: compose("18067", "8080"),
			addrs: map[string][]string{"8080": {"127.0.0.1"}}, asked: []string{"8080", "18067"},
			want: "host port 18067 is published to the container's port 8080, where the app listens only on 127.0.0.1, which no published port reaches, and " + sends0 + ". keploy cannot replay these tests from the host through it: the app would have to listen on 0.0.0.0:8080",
		},
		{
			name: "a test that does not say its port, published where the app listens only on loopback, as on its own number", ports: compose("18067", "8080"),
			addrs: map[string][]string{"8080": {"127.0.0.1"}, "18067": {"::1"}}, asked: []string{"8080", "18067"},
			want: "host port 18067 is published to the container's port 8080, where the app listens only on 127.0.0.1, which no published port reaches, and " + sends0 +
				`. keploy cannot replay these tests from the host through it: the app would have to listen on 0.0.0.0:8080, or host port 18067 be published to the container's port 18067 instead (change "18067:8080" to "18067:18067" in the app service's ports), with the app listening on 0.0.0.0:18067 (it listens there only on ::1)`,
		},
		{
			name: "a test that does not say its port, its own number published on another host port", ports: compose("28067", "18067"),
			addrs: map[string][]string{"18067": {"0.0.0.0"}}, asked: []string{"18067"},
			want: `host port 18067 is not published, while the container's port 18067 is published on host port 28067, and ` + sends0 + `. Publish host port 18067 to it (add "18067:18067" to the app service's ports), or send these tests to host port 28067 with a port map in keploy.yml (test.replaceWith.global.port: {18067: 28067})`,
		},
		{
			// Not "it is not published": the prefix names the app, which
			// is not what is published.
			name: "a test that does not say its port, not published", ports: compose("18057", "8080"),
			addrs: map[string][]string{"18067": {}}, asked: []string{"18067"},
			want: `host port 18067 is not published (the docker compose service publishes 18057:8080), and ` + sends0 + `. Publish it: add "18067:18067" to the app service's ports` +
				", with the app listening on 0.0.0.0:18067 (a published port does not reach a socket on 127.0.0.1)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &countingInspect{resp: agentContainer(tc.ports, true, "bridge")}
			l := &listeningAgent{addrs: tc.addrs, raw: tc.raw, fail: tc.fail, status: http.StatusNotFound}
			a := withAgent(t, dockerAgent(utils.DockerCompose, fake), l)

			if got := a.UnreachableAppPort(context.Background(), "localhost", 18067, tc.appPort); got != tc.want {
				t.Errorf("reason:\n got %q\nwant %q", got, tc.want)
			}
			var want []string
			for _, port := range tc.asked {
				want = append(want, "/agent/app/listen-addrs?port="+port)
			}
			if !slices.Equal(l.asked, want) {
				t.Errorf("asked the agent %v, want %v", l.asked, want)
			}
		})
	}
}

// A host port takes one publish on each host address: docker refuses a second
// one with "port is already allocated". So where the host port the tests go to
// is published to another container port, the fix changes that publish, in the
// words of how the app was started and keeping its host address; it never adds
// one next to it. Where it is published only on addresses the tests do not
// reach, the fix publishes it on every address, which none of those may be
// left next to: it replaces every one of them, not only the first.
func TestUnreachableAppPortChangesTheHostPortsPublish(t *testing.T) {
	both := []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "18067"}, {HostIP: "::", HostPort: "18067"}}
	on := func(ip string) nat.PortBinding { return nat.PortBinding{HostIP: ip, HostPort: "18067"} }
	otherAddrs := nat.PortMap{"9000/tcp": {on("10.0.0.1")}, "8080/tcp": {on("192.168.1.5")}}
	threeAddrs := nat.PortMap{"9000/tcp": {on("10.0.0.1"), on("10.0.0.2")}, "8080/tcp": {on("192.168.1.5")}}
	for _, tc := range []struct {
		name    string
		cmdType utils.CmdType
		ports   nat.PortMap
		want    string
	}{
		{name: "docker run", cmdType: utils.DockerRun, ports: nat.PortMap{"9000/tcp": both},
			want: "Change -p 18067:9000 to -p 18067:8080 in the docker command, or, to keep -p 18067:9000, publish"},
		{name: "docker compose", cmdType: utils.DockerCompose, ports: nat.PortMap{"9000/tcp": both},
			want: `Change "18067:9000" to "18067:8080" in the app service's ports, or, to keep "18067:9000", publish`},
		{name: "--from-container", cmdType: utils.FromContainer, ports: nat.PortMap{"9000/tcp": both},
			want: "Re-create the container with -p 18067:8080 in place of -p 18067:9000, or, to keep -p 18067:9000, publish"},
		{name: "a publish on one host address keeps it", cmdType: utils.DockerRun, ports: nat.PortMap{"9000/tcp": {{HostIP: "127.0.0.1", HostPort: "18067"}}},
			want: "Change -p 127.0.0.1:18067:9000 to -p 127.0.0.1:18067:8080 in the docker command, or, to keep -p 127.0.0.1:18067:9000, publish"},
		{name: "an IPv6 host address", cmdType: utils.DockerRun, ports: nat.PortMap{"9000/tcp": {{HostIP: "::1", HostPort: "18067"}}},
			want: "Change -p [::1]:18067:9000 to -p [::1]:18067:8080 in the docker command"},
		{name: "docker run, published only on two other addresses", cmdType: utils.DockerRun, ports: otherAddrs,
			want: "Publish it on every address: replace -p 10.0.0.1:18067:9000 and -p 192.168.1.5:18067:8080 with -p 18067:8080 in the docker command"},
		{name: "docker compose, published only on two other addresses", cmdType: utils.DockerCompose, ports: otherAddrs,
			want: `Publish it on every address: replace "10.0.0.1:18067:9000" and "192.168.1.5:18067:8080" with "18067:8080" in the app service's ports`},
		{name: "--from-container, published only on two other addresses", cmdType: utils.FromContainer, ports: otherAddrs,
			want: "Publish it on every address: re-create the container with -p 18067:8080 in place of -p 10.0.0.1:18067:9000 and -p 192.168.1.5:18067:8080"},
		{name: "published only on three other addresses", cmdType: utils.DockerRun, ports: threeAddrs,
			want: "Publish it on every address: replace -p 10.0.0.1:18067:9000, -p 10.0.0.2:18067:9000 and -p 192.168.1.5:18067:8080 with -p 18067:8080 in the docker command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &countingInspect{resp: agentContainer(tc.ports, true, "bridge")}
			a := withAgent(t, dockerAgent(tc.cmdType, fake), &listeningAgent{addrs: map[string][]string{"9000": {}, "8080": {"0.0.0.0"}}})
			got := a.UnreachableAppPort(context.Background(), "localhost", 18067, 8080)
			if !strings.Contains(got, tc.want) {
				t.Errorf("reason:\n got %q\nwant it to contain %q", got, tc.want)
			}
			if strings.Contains(got, "add ") {
				t.Errorf("reason %q advises adding a publish of a host port that has one", got)
			}
		})
	}
}

// The agent's answer is one of three: the app listens there (on an address a
// published port reaches, or only on loopback), nothing does, or it cannot
// tell. An address it cannot read leaves the answer unknown unless another one
// it can read already settles it.
func TestClassifyAppListening(t *testing.T) {
	for _, tc := range []struct {
		addrs []string
		want  appListening
	}{
		{addrs: nil, want: appListening{state: listenNothing}},
		{addrs: []string{}, want: appListening{state: listenNothing}},
		{addrs: []string{"0.0.0.0"}, want: appListening{state: listenReachable}},
		{addrs: []string{"127.0.0.1", "172.17.0.2"}, want: appListening{state: listenReachable}},
		{addrs: []string{"::1", "127.0.0.1", "::ffff:127.0.0.1"}, want: appListening{state: listenLoopbackOnly, loopback: []string{"127.0.0.1", "::1"}}},
		{addrs: []string{"not-an-address"}, want: appListening{state: listenUnknown}},
		{addrs: []string{"127.0.0.1", "not-an-address"}, want: appListening{state: listenUnknown}},
		{addrs: []string{"not-an-address", "127.0.0.1"}, want: appListening{state: listenUnknown}},
		{addrs: []string{"not-an-address", "0.0.0.0"}, want: appListening{state: listenReachable}},
		{addrs: []string{"0.0.0.0", "not-an-address"}, want: appListening{state: listenReachable}},
	} {
		if got := classifyAppListening(tc.addrs); got.state != tc.want.state || !slices.Equal(got.loopback, tc.want.loopback) {
			t.Errorf("classifyAppListening(%q) = %+v, want %+v", tc.addrs, got, tc.want)
		}
	}
}

// The decision itself, whatever is asked: an agent that cannot tell where the
// app listens on the container port host port 18067 leads to says nothing
// about that publish, however the app listens on the port the tests were
// recorded on.
func TestUnreachableAppPortReasonSaysNothingWhereTheAgentCannotTell(t *testing.T) {
	pub := portPublish{host: "localhost", port: "18067", appPort: "8080", leadsTo: "9000"}
	for _, atTarget := range []appListening{
		{state: listenReachable},
		{state: listenLoopbackOnly, loopback: []string{"127.0.0.1"}},
		{state: listenNothing},
		{},
	} {
		if got := unreachableAppPortReason(pub, utils.DockerCompose, appListening{}, atTarget); got != "" {
			t.Errorf("cannot tell on 9000, %+v on 8080: reason %q", atTarget, got)
		}
	}
}

// A publish on one host address only takes connections to that address. The
// tests go to a loopback address, so a publish of their host port on another
// address does not lead them anywhere, and one on a loopback address of the
// other family does not either. A wildcard publish may or may not be
// dual-stack, so it is taken to reach every loopback address: "cannot tell"
// must never read as "unreachable".
func TestUnreachableAppPortOnlyCountsPublishesTheTestsReach(t *testing.T) {
	const sends = "keploy sends these tests from the host to port 18067, for the app's port 8080 they were recorded on"
	on := func(ip string) []nat.PortBinding { return []nat.PortBinding{{HostIP: ip, HostPort: "18067"}} }
	for _, tc := range []struct {
		name  string
		host  string
		ports nat.PortMap
		want  string
	}{
		{
			name: "the publish to the app's port is on another address", host: "localhost",
			ports: nat.PortMap{"9000/tcp": on("127.0.0.1"), "8080/tcp": on("192.168.1.5")},
			want: "host port 18067 is published to the container's port 9000, where nothing listens, instead of the app's port 8080 these tests were recorded on, where the app listens, and " + sends +
				". Change -p 127.0.0.1:18067:9000 to -p 127.0.0.1:18067:8080 in the docker command, or, to keep -p 127.0.0.1:18067:9000, publish the app's port 8080 on another host port too and send these tests there with a port map in keploy.yml (test.replaceWith.global.port) in place of the setting that sends them to port 18067 now",
		},
		{
			name: "published only on another address", host: "localhost",
			ports: nat.PortMap{"8080/tcp": on("192.168.1.5")},
			want:  "host port 18067 is published only on 192.168.1.5 (-p 192.168.1.5:18067:8080), which keploy's connections to localhost do not reach, and " + sends + ". Publish it on every address: change -p 192.168.1.5:18067:8080 to -p 18067:8080 in the docker command",
		},
		{
			name: "published only on the IPv4 loopback, the tests sent to the IPv6 one", host: "::1",
			ports: nat.PortMap{"8080/tcp": on("127.0.0.1")},
			want:  "host port 18067 is published only on 127.0.0.1 (-p 127.0.0.1:18067:8080), which keploy's connections to ::1 do not reach, and " + sends + ". Publish it on every address: change -p 127.0.0.1:18067:8080 to -p 18067:8080 in the docker command",
		},
		{
			// A port map to 18057 would send the tests to an address their
			// connection to localhost does not reach either.
			name: "the app's port published on another host port, on another address", host: "localhost",
			ports: nat.PortMap{"8080/tcp": {{HostIP: "192.168.1.5", HostPort: "18057"}}},
			want:  "host port 18067 is not published (the docker command publishes 192.168.1.5:18057:8080), and " + sends + ". Publish it: add -p 18067:8080 to the docker command",
		},
		{name: "published on the loopback localhost names", host: "localhost", ports: nat.PortMap{"8080/tcp": on("127.0.0.1")}},
		{name: "published on the loopback a connection to the unspecified address goes to", host: "0.0.0.0", ports: nat.PortMap{"8080/tcp": on("127.0.0.1")}},
		{name: "published on the address the tests go to", host: "127.0.0.1", ports: nat.PortMap{"8080/tcp": on("127.0.0.1")}},
		{name: "published on the IPv6 wildcard, which may be dual-stack", host: "127.0.0.1", ports: nat.PortMap{"8080/tcp": on("::")}},
		{name: "published with no address", host: "127.0.0.1", ports: nat.PortMap{"8080/tcp": on("")}},
		{
			// localhost is both loopback addresses: one of them reaches it.
			name: "published on one loopback address to the app's port, on the other to another", host: "localhost",
			ports: nat.PortMap{"9000/tcp": on("127.0.0.1"), "8080/tcp": on("::1")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &countingInspect{resp: agentContainer(tc.ports, true, "bridge")}
			a := withAgent(t, dockerAgent(utils.DockerRun, fake), &listeningAgent{addrs: map[string][]string{"9000": {}, "8080": {"0.0.0.0"}}})
			if got := a.UnreachableAppPort(context.Background(), tc.host, 18067, 8080); got != tc.want {
				t.Errorf("reason:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// One host port published on several host addresses, to several container
// ports: the ports come in random order (a map), and the answer must not
// depend on it. The lowest container port is the one named.
func TestUnreachableAppPortAnswersAHostPortPublishedSeveralTimesTheSameWay(t *testing.T) {
	ports := nat.PortMap{
		"10000/tcp": {{HostIP: "127.0.0.1", HostPort: "18067"}},
		"9500/tcp":  {{HostIP: "127.0.0.2", HostPort: "18067"}},
		"9000/tcp":  {{HostIP: "::1", HostPort: "18067"}},
	}
	fake := &countingInspect{resp: agentContainer(ports, true, "bridge")}
	a := withAgent(t, dockerAgent(utils.DockerRun, fake), &listeningAgent{addrs: map[string][]string{
		"9000": {}, "9500": {}, "10000": {}, "8080": {"0.0.0.0"},
	}})
	first := a.UnreachableAppPort(context.Background(), "localhost", 18067, 8080)
	if want := "Change -p [::1]:18067:9000 to -p [::1]:18067:8080 in the docker command"; !strings.Contains(first, want) {
		t.Fatalf("reason:\n got %q\nwant it to contain %q", first, want)
	}
	for i := 0; i < 50; i++ {
		if got := a.UnreachableAppPort(context.Background(), "localhost", 18067, 8080); got != first {
			t.Fatalf("ask %d answered differently:\n got %q\nwant %q", i+2, got, first)
		}
	}

	// To one container port on two host addresses: the order docker lists
	// them in does not decide which is named either.
	for _, bindings := range [][]nat.PortBinding{
		{{HostIP: "::1", HostPort: "18067"}, {HostIP: "127.0.0.1", HostPort: "18067"}},
		{{HostIP: "127.0.0.1", HostPort: "18067"}, {HostIP: "::1", HostPort: "18067"}},
	} {
		fake := &countingInspect{resp: agentContainer(nat.PortMap{"9000/tcp": bindings}, true, "bridge")}
		a := withAgent(t, dockerAgent(utils.DockerRun, fake), &listeningAgent{addrs: map[string][]string{"9000": {}, "8080": {"0.0.0.0"}}})
		got := a.UnreachableAppPort(context.Background(), "localhost", 18067, 8080)
		if want := "Change -p 127.0.0.1:18067:9000 to -p 127.0.0.1:18067:8080 in the docker command"; !strings.Contains(got, want) {
			t.Errorf("publishes listed as %v: reason\n got %q\nwant it to contain %q", bindings, got, want)
		}
	}
}

// The failure this was found by, end to end through the replay request: the
// compose app publishes host port P to its port 8080, the tests were recorded
// on 8080 and are replayed with --port P, and the first one is sent while the
// app is still starting, so the connection is refused. It must be re-sent as
// an app still starting, and get the app's answer once it listens, not fail at
// once as a port the host can never reach.
func TestARefusalAtAHostPortPublishedToTheAppsPortIsReSent(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// The app finishes starting while keploy asks the agent where it listens:
	// from then on the host port answers, as docker forwards it to the app.
	var app *http.Server
	var startOnce sync.Once
	t.Cleanup(func() {
		if app != nil {
			_ = app.Close()
		}
	})
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() {
			ln, err := net.Listen("tcp", "127.0.0.1:"+hostPort)
			if err != nil {
				t.Errorf("the app could not listen on %s: %v", hostPort, err)
				return
			}
			app = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("pong"))
			})}
			go func() { _ = app.Serve(ln) }()
		})
		// Asked before the app listened: nothing listens on its port yet.
		_ = json.NewEncoder(w).Encode(models.AppListenAddrs{Addrs: nil})
	})
	srv := httptest.NewServer(agent)
	t.Cleanup(srv.Close)

	fake := &countingInspect{resp: agentContainer(nat.PortMap{"8080/tcp": {{HostIP: "0.0.0.0", HostPort: hostPort}}}, true, "bridge")}
	a := dockerAgent(utils.DockerCompose, fake)
	a.conf.Agent.AgentURI = srv.URL + "/agent"

	port, _ := strconv.ParseUint(hostPort, 10, 32)
	tc := &models.TestCase{
		Name: "get-ping-1", Kind: models.HTTP, AppPort: 8080,
		HTTPReq: models.HTTPReq{Method: "GET", URL: "http://127.0.0.1:18057/ping", Header: map[string]string{}},
	}
	resp, err := pkg.SimulateHTTP(context.Background(), tc, "test-set-0", zap.NewNop(), pkg.SimulationConfig{
		APITimeout:          5,
		ConfigHost:          "127.0.0.1",
		ConfigPort:          uint32(port),
		AppPortReachability: a,
	})
	if err != nil {
		t.Fatalf("the test failed instead of waiting for the app: %v", err)
	}
	if resp.StatusCode != http.StatusOK || resp.Body != "pong" {
		t.Errorf("got %d %q, want the app's 200 pong", resp.StatusCode, resp.Body)
	}
}
