package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// processAgent is an agent at one address whose process can be started again
// in place: every answer names the process that gives it, as an agent with
// the proven switch on does (models.AgentInstanceHeader); "" names none, as
// an agent with the switch off, or one older than the header.
type processAgent struct {
	mu       sync.Mutex
	instance string
}

func (p *processAgent) set(instance string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.instance = instance
}

func (p *processAgent) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	instance := p.instance
	p.mu.Unlock()
	if instance != "" {
		w.Header().Set(models.AgentInstanceHeader, instance)
	}
	w.WriteHeader(http.StatusOK)
}

// newProcessAgentClient is a client of a processAgent; what was seen of its
// address goes with the test.
func newProcessAgentClient(t *testing.T, instance string) (*AgentClient, *processAgent) {
	t.Helper()
	agent := &processAgent{instance: instance}
	srv := httptest.NewServer(agent)
	t.Cleanup(srv.Close)
	cfg := &config.Config{}
	cfg.Agent.AgentURI = srv.URL + "/agent"
	t.Cleanup(func() { instances.Delete(agentHost(cfg.Agent.AgentURI)) })
	return New(zap.NewNop(), nil, cfg), agent
}

// answer has the agent answer one call of client's, through its client (and
// so through every client AgentClient builds).
func answer(t *testing.T, c http.Client, uri string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, uri+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
}

// A watch keeps to the first agent process that answers after it begins, and
// says so when another answers after it; the answers to any client of that
// address count, its hooks' client and another AgentClient's too.
func TestAWatchSaysWhenAnotherAgentProcessAnswers(t *testing.T) {
	client, agent := newProcessAgentClient(t, "a")
	other := New(zap.NewNop(), nil, client.conf)
	uri := client.conf.Agent.AgentURI

	answer(t, client.client, uri) // before the watch: says nothing of it
	agent.set("b")
	w := client.WatchAgentInstance()
	defer w.Stop()
	answer(t, client.client, uri)
	if rep, replaced := w.Replacement(); replaced {
		t.Fatalf("replaced %+v; one process answered since the watch began", rep)
	}
	agent.set("c")
	answer(t, other.client, uri)
	agent.set("d")
	answer(t, client.client, uri)
	rep, replaced := w.Replacement()
	if !replaced || rep != (models.AgentReplacement{First: "b", Next: "c"}) {
		t.Fatalf("replacement %+v (%v), want b, then c: the first other process to answer after b", rep, replaced)
	}
}

// An answer that names no process (an agent with the switch off, or one older
// than the header) neither keeps a watch to a process nor ends one.
func TestAnAnswerNamingNoProcessSaysNothing(t *testing.T) {
	client, agent := newProcessAgentClient(t, "")
	uri := client.conf.Agent.AgentURI
	w := client.WatchAgentInstance()
	defer w.Stop()
	answer(t, client.client, uri)
	agent.set("a")
	answer(t, client.client, uri)
	agent.set("")
	answer(t, client.client, uri)
	agent.set("a")
	answer(t, client.client, uri)
	if rep, replaced := w.Replacement(); replaced {
		t.Fatalf("replaced %+v; only process a named itself", rep)
	}
	// Nor is it the process that answers now, for an Accept.
	agent.set("")
	answer(t, client.client, uri)
	w.Accept()
	agent.set("b")
	answer(t, client.client, uri)
	if rep, replaced := w.Replacement(); !replaced || rep != (models.AgentReplacement{First: "a", Next: "b"}) {
		t.Fatalf("replacement %+v (%v), want a, then b: a was the last process to name itself when the watch accepted", rep, replaced)
	}
}

// Accept keeps the watch to the process that answers now, and a later
// replacement of that one is told.
func TestAcceptKeepsTheWatchToTheProcessAnsweringNow(t *testing.T) {
	client, agent := newProcessAgentClient(t, "a")
	uri := client.conf.Agent.AgentURI
	w := client.WatchAgentInstance()
	defer w.Stop()
	answer(t, client.client, uri)
	agent.set("b")
	answer(t, client.client, uri)
	if _, replaced := w.Replacement(); !replaced {
		t.Fatal("b answered after a, and the watch did not say so")
	}
	w.Accept()
	answer(t, client.client, uri)
	if rep, replaced := w.Replacement(); replaced {
		t.Fatalf("replaced %+v after Accept; b answered, and the watch keeps to it", rep)
	}
	agent.set("c")
	answer(t, client.client, uri)
	if rep, replaced := w.Replacement(); !replaced || rep != (models.AgentReplacement{First: "b", Next: "c"}) {
		t.Fatalf("replacement %+v (%v), want b, then c", rep, replaced)
	}
}

// A watch that stopped is gone from its address.
func TestAStoppedWatchIsGone(t *testing.T) {
	client, _ := newProcessAgentClient(t, "a")
	at := instancesAt(agentHost(client.conf.Agent.AgentURI))
	w := client.WatchAgentInstance()
	w.Stop()
	at.mu.Lock()
	n := len(at.watches)
	at.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d watch(es) left at the address after Stop", n)
	}
}

// The stop's own connection (ownConnections) still notes which process
// answered, and still opens a connection of its own.
func TestOwnConnectionsKeepsNotingTheAgentProcess(t *testing.T) {
	for _, tok := range []string{"", "tok"} {
		client, agent := newProcessAgentClient(t, "a")
		uri := client.conf.Agent.AgentURI
		client.client = agentHTTPClient(tok)
		c := client.client
		c.Transport = ownConnections(c.Transport)
		it, ok := c.Transport.(*instanceTransport)
		if !ok {
			t.Fatalf("token %q: ownConnections gave %T, which notes no agent process", tok, c.Transport)
		}
		base := it.base
		if bt, ok := base.(*bearerTransport); ok {
			base = bt.base
		}
		if ht, ok := base.(*http.Transport); !ok || !ht.DisableKeepAlives {
			t.Fatalf("token %q: the stop's transport %T keeps connections", tok, base)
		}
		w := client.WatchAgentInstance()
		answer(t, c, uri)
		agent.set("b")
		answer(t, c, uri)
		if _, replaced := w.Replacement(); !replaced {
			t.Fatalf("token %q: a process answering on the stop's connection was not noted", tok)
		}
		w.Stop()
	}
}

// The agent docker compose started again in its bring-up's retry, set up
// again before the app ran, holds all a watch keeps to: the watch keeps to
// it, and is told of a replacement after.
func TestARestartedAgentSetUpAgainIsTheOneAWatchKeepsTo(t *testing.T) {
	client, agent := newRestartingClient(t, true)
	t.Cleanup(func() { instances.Delete(agentHost(client.conf.Agent.AgentURI)) })
	agent.mu.Lock()
	agent.instance = "a"
	agent.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	grp, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, grp)

	w := client.WatchAgentInstance()
	defer w.Stop()
	outgoing, err := client.GetOutgoing(gctx, models.OutgoingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.MakeAgentReadyForDockerCompose(gctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil || m.Name != "mock-1" {
		t.Fatalf("first mock: %+v", m)
	}

	agent.mu.Lock()
	agent.instance = "b" // compose started the agent again
	agent.mu.Unlock()
	if err := client.setUpRestartedAgent(gctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := recv(t, outgoing); m == nil || m.Name != "mock-2" {
		t.Fatalf("mock after the restart: %+v", m)
	}
	if rep, replaced := w.Replacement(); replaced {
		t.Fatalf("replaced %+v; the agent compose started again was set up again before the app ran", rep)
	}
	agent.mu.Lock()
	agent.instance = "c"
	agent.mu.Unlock()
	answer(t, client.client, client.conf.Agent.AgentURI)
	if rep, replaced := w.Replacement(); !replaced || rep != (models.AgentReplacement{First: "b", Next: "c"}) {
		t.Fatalf("replacement %+v (%v), want b, then c", rep, replaced)
	}
	client.session().end()
}
