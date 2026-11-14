//go:build unix

package http

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/docker/go-connections/nat"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The failure this was found by, end to end through the replay request: the
// compose app publishes host port P to its port 8080, the tests were recorded
// on 8080 and are replayed with --port P, and the first one is sent while the
// app is still starting, so the connection is refused. It must be re-sent as
// an app still starting, and get the app's answer once it listens, not fail at
// once as a port the host can never reach.
func TestARefusalAtAHostPortPublishedToTheAppsPortIsReSent(t *testing.T) {
	// The app's port, refused until the app listens on it, and held all along
	// (testnet.SharedPort): released and bound again, it was another's to take
	// in between on a busy host, and the app could not listen on it.
	hostPort := strconv.Itoa(testnet.SharedPort(t))

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
			ln, err := testnet.OpenShared("tcp", net.JoinHostPort("127.0.0.1", hostPort))
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
