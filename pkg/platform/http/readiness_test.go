package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The agent's readiness answer: a 404 (an agent without the route) or a 501
// (one without the watch) is an agent that cannot watch, for the first
// test's own connect to decide; a 500 is a read that failed, never that; a
// 200 is the answer.
func TestAppReadinessTellsAnAgentThatCannotWatchFromAFailedRead(t *testing.T) {
	for _, c := range []struct {
		code        int
		body        string
		unsupported bool
	}{
		{http.StatusNotFound, `404 page not found`, true},
		{http.StatusNotImplemented, `{"error":"this agent cannot tell whether the application listens"}`, true},
		{http.StatusInternalServerError, `{"error":"read the application's progress: bad fd"}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/app/readiness" {
				t.Errorf("path %q, want /app/readiness", r.URL.Path)
			}
			w.WriteHeader(c.code)
			_, _ = w.Write([]byte(c.body))
		}))
		a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
		_, err := a.AppReadiness(context.Background(), 0)
		srv.Close()
		if err == nil || errors.Is(err, models.ErrReadinessUnsupported) != c.unsupported {
			t.Errorf("%d: err %v, want unsupported %v", c.code, err, c.unsupported)
		}
	}
	var since string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		since = r.URL.Query().Get("since")
		_, _ = w.Write([]byte(`{"listens":[{"port":8080,"addr":"0.0.0.0","here":true,"cookie":7,"seq":3,"live":true}],"listen_count":4,"progress":{"execs":2}}`))
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	got, err := a.AppReadiness(context.Background(), 3)
	if err != nil || since != "3" || len(got.Listens) != 1 || !got.Listens[0].Live || got.Listens[0].Cookie != 7 || got.ListenCount != 4 || got.Progress.Execs != 2 {
		t.Fatalf("AppReadiness = %+v, %v (asked since %q); want, from 3, the live listen on :8080, a count of 4 and two execs", got, err, since)
	}
}

// The container port a host port is published to is the keploy agent
// container's (it publishes the application's ports): the lowest TCP one,
// when several are; nothing for a port it does not publish, or outside
// Docker mode.
func TestThePublishedContainerPortIsTheAgentContainersMapping(t *testing.T) {
	resp := container.InspectResponse{NetworkSettings: &container.NetworkSettings{NetworkSettingsBase: container.NetworkSettingsBase{Ports: nat.PortMap{
		"3001/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "8098"}},
		"3000/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "8098"}},
		"53/udp":   []nat.PortBinding{{HostPort: "8098"}},
		"4000/tcp": []nat.PortBinding{{HostPort: "9000"}},
	}}}}
	a := &AgentClient{logger: zap.NewNop(), dockerClient: inspectOnly{resp: resp}, conf: &config.Config{KeployContainer: "keploy-v3", CommandType: string(utils.DockerCompose)}}
	for host, want := range map[uint16]uint16{8098: 3000, 9000: 4000, 7000: 0} {
		if got, ok := a.PublishedContainerPort(context.Background(), host); got != want || ok != (want != 0) {
			t.Errorf("PublishedContainerPort(%d) = %d, %v; want %d", host, got, ok, want)
		}
	}
	a.conf.CommandType = string(utils.Native)
	if got, ok := a.PublishedContainerPort(context.Background(), 8098); ok {
		t.Errorf("a native application: PublishedContainerPort = %d, want none", got)
	}
}

// The set's mark is a POST to /app/readiness/mark; a 404 or 501 is an agent
// that cannot watch.
func TestMarkReadinessPostsTheMark(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{"listen_count":4,"listens_lost":1}`))
	}))
	defer srv.Close()
	a := &AgentClient{logger: zap.NewNop(), client: http.Client{}, conf: &config.Config{Agent: config.Agent{AgentURI: srv.URL}}}
	got, err := a.MarkReadiness(context.Background())
	if err != nil || method != http.MethodPost || path != "/app/readiness/mark" || got.ListenCount != 4 || got.ListensLost != 1 {
		t.Fatalf("MarkReadiness = %+v, %v (%s %s); want POST /app/readiness/mark answering 4 and 1", got, err, method, path)
	}
	nf := httptest.NewServer(http.NotFoundHandler())
	defer nf.Close()
	a.conf.Agent.AgentURI = nf.URL
	if _, err := a.MarkReadiness(context.Background()); !errors.Is(err, models.ErrReadinessUnsupported) {
		t.Fatalf("an agent without the route: %v, want one that cannot watch", err)
	}
}
