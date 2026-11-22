package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

// composeAgentOn is an agent client whose compose agent Setup put on port
// 16789, at uri, and whose next draw of the agent's port is 20001 (draws
// counts the draws, keep what each kept off).
func composeAgentOn(t *testing.T, uri string) (a *AgentClient, draws *int, keep *[]uint32) {
	t.Helper()
	was := containerPort
	t.Cleanup(func() { containerPort = was })
	draws, keep = new(int), new([]uint32)
	containerPort = func(k ...uint32) (uint32, error) {
		*draws++
		*keep = append([]uint32(nil), k...)
		return 20001, nil
	}
	conf := &config.Config{}
	conf.Agent.AgentURI = uri
	conf.Agent.AgentPort, conf.Agent.ProxyPort, conf.Agent.DnsPort = 16789, 16790, 16791
	a = &AgentClient{logger: zap.NewNop(), conf: conf}
	a.addressAgent(uri)
	return a, draws, keep
}

// Compose's refusal to publish keploy's agent's port, in each engine's
// words, moves the agent to the port drawn again: keploy addresses it there
// (its scheme, host, user and path kept), a mock run's tests are handed it,
// and its record of the port moves; the field Setup recorded the address in
// stays the caller's record. The draw keeps off the proxy's and the DNS
// server's ports and the one taken.
func TestComposeRefusingTheAgentsPortMovesTheAgent(t *testing.T) {
	refusals := map[string]string{
		"Docker, another container's port": "Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint keploy-agent (5e4edfc81cf3): Bind for 127.0.0.1:16789 failed: port is already allocated",
		"Docker 29, a host process's":      " ✘ Container keploy-agent  Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint keploy-agent (b7eadbc86d7b): failed to bind host port 127.0.0.1:16789/tcp: address already in use",
		"Docker before 29, no proxy":       "Error response from daemon: driver failed programming external connectivity on endpoint keploy-agent (abc): failed to bind host port for 127.0.0.1:16789:172.18.0.2:16789/tcp: address already in use",
		"Docker's userland proxy":          "Error response from daemon: driver failed programming external connectivity on endpoint keploy-agent (abc): Error starting userland proxy: listen tcp4 127.0.0.1:16789: bind: address already in use",
		"podman's rootlessport":            "Error: rootlessport listen tcp 127.0.0.1:16789: bind: address already in use",
		"Docker Desktop on Windows":        "Error response from daemon: Ports are not available: exposing port TCP 127.0.0.1:16789 -> 0.0.0.0:0: listen tcp 127.0.0.1:16789: bind: Only one usage of each socket address (protocol/network address/port) is normally permitted.",
	}
	for name, refusal := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Setenv("KEPLOY_MOCK_AGENT", "http://localhost:16789")
			a, draws, keep := composeAgentOn(t, "http://localhost:16789/agent")
			said := " ✔ Network app_default  Created\n" + refusal + "\n"
			to, err := a.composeAgentPortRefused(context.Background(), said)
			if err != nil || to != 20001 {
				t.Fatalf("composeAgentPortRefused = %d, %v; want the agent moved to 20001", to, err)
			}
			if got := a.conf.AgentURI(); got != "http://localhost:20001/agent" {
				t.Fatalf("the agent is addressed at %q, want http://localhost:20001/agent", got)
			}
			if a.conf.Agent.AgentPort != 20001 || a.conf.Agent.AgentURI != "http://localhost:16789/agent" {
				t.Fatalf("Agent.AgentPort %d, Agent.AgentURI %q; want 20001, and Setup's record kept", a.conf.Agent.AgentPort, a.conf.Agent.AgentURI)
			}
			if got := os.Getenv("KEPLOY_MOCK_AGENT"); got != "http://localhost:20001" {
				t.Fatalf("KEPLOY_MOCK_AGENT %q, want http://localhost:20001", got)
			}
			if *draws != 1 || !slices.Contains(*keep, 16789) || !slices.Contains(*keep, 16790) || !slices.Contains(*keep, 16791) {
				t.Fatalf("%d draws keeping off %v; want one, off 16789, 16790 and 16791", *draws, *keep)
			}
		})
	}

	// A pinned address keeps its host, its user and its path.
	a, _, _ := composeAgentOn(t, "http://u:p@10.0.0.5:16789/agent")
	if _, err := a.composeAgentPortRefused(context.Background(), refusals["Docker 29, a host process's"]); err != nil {
		t.Fatal(err)
	}
	if got := a.conf.AgentURI(); got != "http://u:p@10.0.0.5:20001/agent" {
		t.Fatalf("the pinned agent is addressed at %q, want http://u:p@10.0.0.5:20001/agent", got)
	}
	// A test's address that is not the agent's is not touched.
	t.Setenv("KEPLOY_MOCK_AGENT", "http://localhost:9999")
	a, _, _ = composeAgentOn(t, "http://localhost:16789/agent")
	if _, err := a.composeAgentPortRefused(context.Background(), refusals["Docker 29, a host process's"]); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("KEPLOY_MOCK_AGENT"); got != "http://localhost:9999" {
		t.Fatalf("KEPLOY_MOCK_AGENT %q, want the one that was not the agent's, http://localhost:9999", got)
	}
}

// An up that failed otherwise, or refused another port (an application's,
// which keploy did not pick), does not move the agent; one that named the
// agent's address in words keploy does not know for a taken port fails
// closed: its error shows the line, and the agent is not moved. Nor is it
// moved once the run is cut.
func TestComposeFailingOtherwiseLeavesTheAgentWhereItIs(t *testing.T) {
	for name, said := range map[string]string{
		"a dependency's crash":   "dependency failed to start: container app-db-1 exited (1)",
		"an application's port":  "Error response from daemon: failed to set up container networking: failed to bind host port 127.0.0.1:8080/tcp: address already in use",
		"a longer port's prefix": "Error response from daemon: failed to set up container networking: failed to bind host port 127.0.0.1:167890/tcp: address already in use",
		"nothing":                "",
	} {
		t.Run(name, func(t *testing.T) {
			a, draws, _ := composeAgentOn(t, "http://localhost:16789/agent")
			if to, err := a.composeAgentPortRefused(context.Background(), said); to != 0 || err != nil {
				t.Fatalf("composeAgentPortRefused = %d, %v; want 0 and no error", to, err)
			}
			if *draws != 0 || a.conf.AgentURI() != "http://localhost:16789/agent" || a.conf.Agent.AgentPort != 16789 {
				t.Fatalf("%d draws, agent at %q port %d; want it left where it was", *draws, a.conf.AgentURI(), a.conf.Agent.AgentPort)
			}
		})
	}

	const unknown = "Error response from daemon: something new happened to 127.0.0.1:16789 today"
	a, draws, _ := composeAgentOn(t, "http://localhost:16789/agent")
	to, err := a.composeAgentPortRefused(context.Background(), "x\n"+unknown+"\n")
	if to != 0 || err == nil || !strings.Contains(err.Error(), unknown) || !strings.Contains(err.Error(), "does not know") {
		t.Fatalf("composeAgentPortRefused = %d, %v; want an error showing %q", to, err, unknown)
	}
	if *draws != 0 || a.conf.AgentURI() != "http://localhost:16789/agent" {
		t.Fatalf("%d draws, agent at %q; want it left where it was", *draws, a.conf.AgentURI())
	}

	// No other port to draw, or an address with no host to move: the
	// agent is left where it was, and the error says why.
	a, _, _ = composeAgentOn(t, "http://localhost:16789/agent")
	containerPort = func(...uint32) (uint32, error) { return 0, errors.New("every port is taken") }
	if to, err := a.composeAgentPortRefused(context.Background(), "failed to bind host port 127.0.0.1:16789/tcp: address already in use"); to != 0 || err == nil || !strings.Contains(err.Error(), "every port is taken") {
		t.Fatalf("composeAgentPortRefused with no port to draw = %d, %v; want the draw's error", to, err)
	}
	if a.conf.AgentURI() != "http://localhost:16789/agent" || a.conf.Agent.AgentPort != 16789 {
		t.Fatalf("with no port to draw the agent is at %q port %d; want it left where it was", a.conf.AgentURI(), a.conf.Agent.AgentPort)
	}
	for _, uri := range []string{"", "localhost:16789"} {
		a, _, _ = composeAgentOn(t, uri)
		if to, err := a.composeAgentPortRefused(context.Background(), "failed to bind host port 127.0.0.1:16789/tcp: address already in use"); to != 0 || err == nil {
			t.Fatalf("composeAgentPortRefused at %q = %d, %v; want an error", uri, to, err)
		}
		if a.conf.AgentURI() != uri || a.conf.Agent.AgentPort != 16789 {
			t.Fatalf("an agent at %q was moved to %q port %d", uri, a.conf.AgentURI(), a.conf.Agent.AgentPort)
		}
	}

	cut, cancel := context.WithCancel(context.Background())
	cancel()
	a, draws, _ = composeAgentOn(t, "http://localhost:16789/agent")
	if to, err := a.composeAgentPortRefused(cut, "failed to bind host port 127.0.0.1:16789/tcp: address already in use"); to != 0 || err == nil {
		t.Fatalf("a cut run's composeAgentPortRefused = %d, %v; want its error", to, err)
	}
	if *draws != 0 || a.conf.AgentURI() != "http://localhost:16789/agent" {
		t.Fatalf("a cut run drew %d ports and addressed the agent at %q; want it left where it was", *draws, a.conf.AgentURI())
	}
}

// Readers of the agent's address while compose's refusal moves it, as the
// services' compose waits and the application client read it, each read the
// address before the move or after it, never another, with no race (-race).
// The application client is handed that one address (SetAgentAddress).
func TestReadersOfTheAgentsAddressSeeItMoveWhole(t *testing.T) {
	a, _, _ := composeAgentOn(t, "http://localhost:16789/agent")
	app := &recordingApp{}
	a.handRestartHooks(app)
	if app.address == nil || app.refused == nil {
		t.Fatalf("hooks handed: address %v, port refusal %v; want both", app.address != nil, app.refused != nil)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	bad := make(chan string, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got := app.address(); got != "http://localhost:16789/agent" && got != "http://localhost:20001/agent" {
					bad <- got
					return
				}
			}
		}()
	}
	to, err := app.refused(context.Background(), "failed to bind host port 127.0.0.1:16789/tcp: address already in use")
	close(stop)
	wg.Wait()
	close(bad)
	for got := range bad {
		t.Errorf("a reader read %q while the agent moved", got)
	}
	if err != nil || to != 20001 || app.address() != "http://localhost:20001/agent" {
		t.Fatalf("the hook moved the agent to %d (%v), the app reads %q; want 20001, http://localhost:20001/agent", to, err, app.address())
	}
}

// Moving a compose agent to another port carries every address-keyed registry
// populated before compose up: the restartable *agentSession configured by
// registerApp, containerAt / agentContainerAt populated by nameAgentContainer,
// and instancesAt (any WatchAgentInstance opened before compose up). The old
// address keys are cleaned up so nothing is orphaned or leaked there.
func TestComposeMoveKeepsTheAgentsSessionAndRegistries(t *testing.T) {
	const from, to = "http://localhost:16789/agent", "http://localhost:20001/agent"
	fromHost, toHost := agentHost(from), agentHost(to)
	t.Cleanup(func() {
		sessions.Delete(from)
		sessions.Delete(to)
		containerAt.Delete(fromHost)
		containerAt.Delete(toHost)
		instances.Delete(fromHost)
		instances.Delete(toHost)
	})

	a, _, _ := composeAgentOn(t, from)
	a.session().configure(true)
	if !a.session().recording() {
		t.Fatal("precondition: the compose agent's session records")
	}
	a.nameAgentContainer("keploy-agent-move-test")
	watch := a.WatchAgentInstance()
	defer watch.Stop()

	refusal := "Error response from daemon: failed to set up container networking: driver failed programming external connectivity on endpoint keploy-agent (abc): Bind for 127.0.0.1:16789 failed: port is already allocated"
	if got, err := a.composeAgentPortRefused(context.Background(), refusal); err != nil || got != 20001 {
		t.Fatalf("composeAgentPortRefused: %d, %v", got, err)
	}
	if a.conf.AgentURI() != to {
		t.Fatalf("moved to %q, want %q", a.conf.AgentURI(), to)
	}

	// 1. Session moved and kept restartable.
	if !a.session().recording() {
		t.Fatalf("after moving to %s the agent's session is not restartable: the session configured at Setup was left at %s", a.conf.AgentURI(), from)
	}
	if _, stale := sessions.Load(from); stale {
		t.Fatalf("old session key %q remained in sessions after move", from)
	}

	// 2. containerAt and agentContainerAt moved to the new host:port.
	if _, stale := containerAt.Load(fromHost); stale {
		t.Fatalf("old host %q remained in containerAt after move", fromHost)
	}
	answeredAt(toHost)
	if !a.containerNamed("keploy-agent-move-test").isKnown() {
		t.Fatalf("answeredAt(%q) did not mark the container Setup named as known", toHost)
	}
	a.forgetAgentContainer()
	if _, left := containerAt.Load(toHost); left {
		t.Fatalf("forgetAgentContainer did not drop moved entry at %q", toHost)
	}

	// 3. instancesAt moved so watches opened before compose up follow answers at toHost.
	if _, stale := instances.Load(fromHost); stale {
		t.Fatalf("old host %q remained in instances after move", fromHost)
	}
	instancesAt(toHost).answered("inst-1")
	instancesAt(toHost).answered("inst-2")
	rep, replaced := watch.Replacement()
	if !replaced || rep.First != "inst-1" || rep.Next != "inst-2" {
		t.Fatalf("watch opened before move got replacement=(%+v, %v); want (inst-1 -> inst-2, true)", rep, replaced)
	}
}

// Moving a compose agent off a refused port and then having compose restart
// the agent drives setUpRestartedAgent against the moved address: configuring
// calls and readiness releases recorded before the move are reapplied on the
// moved address, and the restarted instance is tracked on the moved host.
func TestComposeMoveThenRestartReappliesSessionOnMovedAddress(t *testing.T) {
	var healthHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(models.AgentInstanceHeader, "restarted-moved-agent")
		if r.URL.Path == "/agent/health" {
			healthHits.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	movedPort64, err := strconv.ParseUint(u.Port(), 10, 32)
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}
	movedPort := uint32(movedPort64)

	from := fmt.Sprintf("http://%s:16789/agent", u.Hostname())
	to := fmt.Sprintf("http://%s:%d/agent", u.Hostname(), movedPort)
	fromHost, toHost := agentHost(from), agentHost(to)
	t.Cleanup(func() {
		sessions.Delete(from)
		sessions.Delete(to)
		instances.Delete(fromHost)
		instances.Delete(toHost)
	})

	a, _, _ := composeAgentOn(t, from)
	containerPort = func(...uint32) (uint32, error) { return movedPort, nil }
	a.client = http.Client{Transport: &instanceTransport{base: srv.Client().Transport}}

	// Simulate Setup's registerApp and a recorded configuring step + release.
	a.session().configure(true)
	watch := a.WatchAgentInstance()
	defer watch.Stop()

	var reappliedURI string
	var releaseURI string
	a.session().record(stepMockOutgoing, func(context.Context) error {
		reappliedURI = a.conf.AgentURI()
		return nil
	})
	rc, refused := a.session().beginRelease(false, func(error) {}, func(context.Context) error {
		releaseURI = a.conf.AgentURI()
		return nil
	})
	if refused != nil {
		t.Fatalf("beginRelease refused: %v", refused)
	}
	a.session().endRelease(rc, true)

	// Move the agent off the refused port to the server's port.
	refusal := "Error response from daemon: failed to set up container networking: failed to bind host port 127.0.0.1:16789/tcp: address already in use"
	gotPort, err := a.composeAgentPortRefused(context.Background(), refusal)
	if err != nil || gotPort != movedPort {
		t.Fatalf("composeAgentPortRefused = %d, %v; want %d", gotPort, err, movedPort)
	}
	if a.conf.AgentURI() != to {
		t.Fatalf("agent URI = %q, want %q", a.conf.AgentURI(), to)
	}

	// Drive compose's agent restart hook on the moved agent.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.setUpRestartedAgent(ctx); err != nil {
		t.Fatalf("setUpRestartedAgent after move: %v", err)
	}
	if healthHits.Load() == 0 {
		t.Fatal("setUpRestartedAgent did not probe /health on the moved agent")
	}
	if reappliedURI != to {
		t.Fatalf("configuring step reapplied at %q; want moved address %q", reappliedURI, to)
	}
	if releaseURI != to {
		t.Fatalf("agent-ready release reapplied at %q; want moved address %q", releaseURI, to)
	}
	if !a.AgentStartedBeforeApp("restarted-moved-agent") {
		t.Fatal("setUpRestartedAgent did not record the restarted instance at the moved address")
	}
	if rep, replaced := watch.Replacement(); replaced {
		t.Fatalf("watch saw replacement %+v after setUpRestartedAgent accepted the moved agent", rep)
	}
}

// Both Setup's initial draw and composeAgentPortRefused's move draw exclude
// host ports statically published by other services in the compose document,
// whether the compose document is in memory or on disk.
func TestComposeAgentPortExcludesDocumentPublishedHostPorts(t *testing.T) {
	composeYAML := []byte("services:\n  api:\n    image: alpine:3.21\n    ports:\n      - \"61550:8080\"\n      - \"127.0.0.1:61551:8081/tcp\"\n")
	stop := errors.New("stop at the agent's port")

	t.Run("in-memory compose excluded on Setup draw", func(t *testing.T) {
		was := containerPort
		t.Cleanup(func() { containerPort = was })
		var gotKeep []uint32
		containerPort = func(k ...uint32) (uint32, error) {
			gotKeep = append([]uint32(nil), k...)
			return 0, stop
		}
		conf := &config.Config{}
		conf.ProxyPort, conf.DNSPort = 16790, 16791
		a := &AgentClient{logger: zap.NewNop(), conf: conf}
		opts := models.SetupOptions{
			CommandType:     "docker-compose",
			InMemoryCompose: composeYAML,
		}
		if err := a.Setup(context.Background(), "docker compose up", opts); !errors.Is(err, stop) {
			t.Fatalf("Setup = %v; want %v", err, stop)
		}
		if !slices.Contains(gotKeep, 61550) || !slices.Contains(gotKeep, 61551) {
			t.Fatalf("Setup's agentPortFor excluded %v; want document published ports 61550 and 61551 included", gotKeep)
		}
	})

	t.Run("on-disk compose file excluded on Setup draw and move draw", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "compose.yaml")
		if err := os.WriteFile(path, composeYAML, 0o600); err != nil {
			t.Fatal(err)
		}
		was := containerPort
		t.Cleanup(func() { containerPort = was })
		var setupKeep []uint32
		containerPort = func(k ...uint32) (uint32, error) {
			setupKeep = append([]uint32(nil), k...)
			return 0, stop
		}
		conf := &config.Config{}
		conf.Agent.AgentURI = "http://localhost:16789/agent"
		conf.Agent.AgentPort, conf.Agent.ProxyPort, conf.Agent.DnsPort = 16789, 16790, 16791
		conf.ProxyPort, conf.DNSPort = 16790, 16791
		a := &AgentClient{logger: zap.NewNop(), conf: conf}
		opts := models.SetupOptions{CommandType: "docker-compose"}
		if err := a.Setup(context.Background(), "docker compose -f "+path+" up", opts); !errors.Is(err, stop) {
			t.Fatalf("Setup = %v; want %v", err, stop)
		}
		if !slices.Contains(setupKeep, 61550) || !slices.Contains(setupKeep, 61551) {
			t.Fatalf("Setup's agentPortFor excluded %v; want document published ports 61550 and 61551 included", setupKeep)
		}

		// Now verify composeAgentPortRefused also excludes the stored compose static ports.
		a.addressAgent("http://localhost:16789/agent")
		var moveKeep []uint32
		containerPort = func(k ...uint32) (uint32, error) {
			moveKeep = append([]uint32(nil), k...)
			return 20001, nil
		}
		refusal := "Error response from daemon: failed to set up container networking: failed to bind host port 127.0.0.1:16789/tcp: address already in use"
		if to, err := a.composeAgentPortRefused(context.Background(), refusal); err != nil || to != 20001 {
			t.Fatalf("composeAgentPortRefused = %d, %v; want 20001", to, err)
		}
		if !slices.Contains(moveKeep, 61550) || !slices.Contains(moveKeep, 61551) {
			t.Fatalf("move draw excluded %v; want document published ports 61550 and 61551 included", moveKeep)
		}
	})
}
