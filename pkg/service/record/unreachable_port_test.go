package record

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	agenthttp "go.keploy.io/server/v3/pkg/platform/http"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A keploy.yml that sends replay somewhere other than test.host and each test's
// recorded port makes "can the host reach the recorded port?" the wrong
// question: replay will not dial it, so a warning about it is noise, or worse,
// advice to publish a port nobody uses. Each redirect setting on its own keeps
// the recording quiet and asks the instrumentation nothing; with none of them,
// it asks and warns.
func TestUnreachablePortWarnerAsksNothingWhenReplayIsRedirected(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*config.Test)
	}{
		{"test.port", func(t *config.Test) { t.Port = 8080 }},
		{"test.grpcPort", func(t *config.Test) { t.GRPCPort = 50051 }},
		{"test.ssePort", func(t *config.Test) { t.SSEPort = 8047 }},
		{"a protocol port", func(t *config.Test) { t.Protocol = config.ProtocolConfig{"http": {Port: 8080}} }},
		{"replaceWith.global.url", func(t *config.Test) {
			t.ReplaceWith.Global.URL = map[string]string{"localhost:8096": "app.internal:8096"}
		}},
		{"replaceWith.global.port", func(t *config.Test) { t.ReplaceWith.Global.Port = map[uint32]uint32{8096: 18096} }},
		{"replaceWith.test-sets", func(t *config.Test) {
			t.ReplaceWith.TestSets = map[string]config.ReplaceWithMap{"test-set-0": {Port: map[uint32]uint32{8096: 18096}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			tc.set(&cfg.Test)
			instr := &unpublishedInstr{unpublished: map[uint16]string{8096: "it is not published on the host"}}
			core, logs := observer.New(zapcore.DebugLevel)

			w := newUnreachablePortWarner(zap.New(core), instr, cfg)
			w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8096})

			if len(instr.asked) != 0 {
				t.Errorf("asked %v; replay does not dial the recorded port", instr.asked)
			}
			if logs.Len() != 0 {
				t.Errorf("logged %v", logs.All())
			}
		})
	}

	// The control: nothing redirected, so it asks, and warns.
	instr := &unpublishedInstr{unpublished: map[uint16]string{8096: "it is not published on the host"}}
	core, logs := observer.New(zapcore.WarnLevel)
	w := newUnreachablePortWarner(zap.New(core), instr, &config.Config{})
	w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8096})
	if len(instr.asked) != 1 || logs.FilterMessageSnippet("port 8096 cannot be replayed").Len() != 1 {
		t.Errorf("not redirected: asked %v, logged %v; want one question and one warning", instr.asked, logs.All())
	}
}

// inspectOnly is a docker daemon that answers every ContainerInspect with resp.
type inspectOnly struct {
	kdocker.Client
	resp container.InspectResponse
}

func (f inspectOnly) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	return f.resp, nil
}

// The record-time warning from the Docker-mode instrumentation itself, which
// asks about the recorded port at that same host port. Where host port 8095
// already has a publish, to the container's port 9000 where nothing listens,
// a second one is refused ("port is already allocated"): the warning says to
// change that one, or to send these tests where 8095 is published with a port
// map, never to add -p 8095:8095. Where host port 8095 has none, adding it is
// the fix.
func TestUnreachablePortWarnerSaysHowToPublishTheRecordedPort(t *testing.T) {
	both := func(port string) []nat.PortBinding {
		return []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: port}, {HostIP: "::", HostPort: port}}
	}
	const prefix = "test cases recorded on the app's port 8095 cannot be replayed at that port from the host: "
	const sendThere = ", or send these tests to host port 18095 with a port map in keploy.yml (test.replaceWith.global.port: {8095: 18095})"
	for _, tc := range []struct {
		name  string
		ports nat.PortMap
		want  string
	}{
		{
			name:  "host port 8095 published to another container port",
			ports: nat.PortMap{"9000/tcp": both("8095"), "8095/tcp": both("18095")},
			want: prefix + "host port 8095 is published to the container's port 9000, where nothing listens, instead of the app's port 8095 these tests were recorded on, where the app listens, and keploy sends each test from the host to port 8095, the port it was recorded on. " +
				"Change -p 8095:9000 to -p 8095:8095 in the docker command" + sendThere,
		},
		{
			name:  "host port 8095 not published",
			ports: nat.PortMap{"8095/tcp": both("18095")},
			want: prefix + "it is published on host port 18095 instead, and keploy sends each test from the host to port 8095, the port it was recorded on. " +
				"Publish it on the same port (add -p 8095:8095 to the docker command)" + sendThere,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The app listens on 8095, on every address; nothing on 9000.
				addrs := map[string][]string{"9000": {}, "8095": {"0.0.0.0"}}[r.URL.Query().Get("port")]
				_ = json.NewEncoder(w).Encode(models.AppListenAddrs{Addrs: addrs})
			}))
			defer agent.Close()
			cfg := &config.Config{CommandType: string(utils.DockerRun), KeployContainer: "keploy-v3-test"}
			cfg.Agent.AgentURI = agent.URL + "/agent"
			inspect := container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					State:      &container.State{Running: true},
					HostConfig: &container.HostConfig{NetworkMode: "bridge"},
				},
				NetworkSettings: &container.NetworkSettings{NetworkSettingsBase: container.NetworkSettingsBase{Ports: tc.ports}},
			}
			core, logs := observer.New(zapcore.WarnLevel)

			w := newUnreachablePortWarner(zap.New(core), agenthttp.New(zap.NewNop(), inspectOnly{resp: inspect}, cfg), cfg)
			w.check(context.Background(), &models.TestCase{Name: "test-1", AppPort: 8095})

			if logs.Len() != 1 || logs.All()[0].Message != tc.want {
				t.Fatalf("warned %v\nwant one warning:\n%q", logs.All(), tc.want)
			}
			if strings.Contains(tc.name, "another container port") && strings.Contains(logs.All()[0].Message, "add -p") {
				t.Errorf("the warning advises a second publish of host port 8095: %q", logs.All()[0].Message)
			}
		})
	}
}
