package http

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
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
