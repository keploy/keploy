package http

import (
	"net/http"
	"net/url"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// An agent with the proven switch on names, in every answer, the process that
// gives it (models.AgentInstanceHeader). What a test set is judged on (the
// calls no mock answered, what its tests consumed, what keploy could not
// capture) is held in that process's memory, and an agent started again in
// its place answers at the same address holding none of it: kubelet restarts
// a crashed or OOM-killed sidecar in place, and docker a container. Read
// from the agent started again, a test's misses are none, and the set would
// pass on evidence that was lost. So the client notes which process each
// answer came from, and a watch (WatchAgentInstance) says whether every
// answer since it began came from one.

// instanceTransport notes the agent process every answer it carries names.
// Every client AgentClient builds goes through one (agentHTTPClient), so no
// call site can leave an answer out.
type instanceTransport struct {
	base http.RoundTripper
}

func (t *instanceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	res, err := base.RoundTrip(req)
	if err == nil && res != nil {
		// Any answer is the agent's: its container is there (known, on
		// the entry of the Setup that set it up at this address), whether
		// or not the answer names its process.
		answeredAt(req.URL.Host)
		// An answer that names no process says nothing about which one
		// answered: an agent with the switch off, or one older than the
		// header. It neither keeps a watch to a process nor ends one.
		if instance := res.Header.Get(models.AgentInstanceHeader); instance != "" {
			instancesAt(req.URL.Host).answered(instance)
		}
	}
	return res, err
}

// agentInstances is what the answers from one agent address (host:port)
// said of the process that gave them, and the watches open on it. It is
// kept per address, not per AgentClient, because one run can talk to its
// agent through more than one (enterprise's headless test starts the app
// through one and stores mocks through another): an answer to any of them
// is an answer of the agent.
type agentInstances struct {
	mu sync.Mutex
	// latest is the process that gave the latest answer naming one.
	latest  string
	watches map[*instanceWatch]struct{}
}

var instances sync.Map // host:port -> *agentInstances

// instancesAt is what was seen of the agent at host (host:port).
func instancesAt(host string) *agentInstances {
	if v, ok := instances.Load(host); ok {
		return v.(*agentInstances)
	}
	v, _ := instances.LoadOrStore(host, &agentInstances{watches: map[*instanceWatch]struct{}{}})
	return v.(*agentInstances)
}

// answered notes an answer from the process instance.
func (s *agentInstances) answered(instance string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest = instance
	for w := range s.watches {
		switch {
		case w.first == "":
			w.first = instance
		case instance != w.first && w.next == "":
			w.next = instance
		}
	}
}

// watch begins a watch: the first process to answer from now on is the one
// it keeps to. Not the latest one seen: a watch begins where what it keeps to
// begins (a test set, before its mocks are staged), and an agent replaced
// before that holds all of it.
func (s *agentInstances) watch() *instanceWatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := &instanceWatch{at: s}
	s.watches[w] = struct{}{}
	return w
}

// accept has every watch on the address keep to the process that answers now
// (instanceWatch.Accept).
func (s *agentInstances) accept() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for w := range s.watches {
		w.first, w.next = s.latest, ""
	}
}

// instanceWatch is a models.AgentInstanceWatch on one agent address.
type instanceWatch struct {
	at *agentInstances
	// first is the process the watch keeps to: the first to answer since
	// the watch began, or the one an Accept found answering; "" while none
	// has. next is the first other process to answer after it; "" while
	// none has. Both guarded by at.mu.
	first, next string
}

func (w *instanceWatch) Replacement() (models.AgentReplacement, bool) {
	w.at.mu.Lock()
	defer w.at.mu.Unlock()
	return models.AgentReplacement{First: w.first, Next: w.next}, w.next != ""
}

func (w *instanceWatch) Accept() {
	w.at.mu.Lock()
	defer w.at.mu.Unlock()
	w.first, w.next = w.at.latest, ""
}

func (w *instanceWatch) Stop() {
	w.at.mu.Lock()
	defer w.at.mu.Unlock()
	delete(w.at.watches, w)
}

// WatchAgentInstance begins a watch of which agent process answers at this
// client's agent address (models.AgentInstanceWatch).
func (a *AgentClient) WatchAgentInstance() models.AgentInstanceWatch {
	return instancesAt(agentHost(a.conf.Agent.AgentURI)).watch()
}

// agentHost is the host:port of the agent URI uri, as its requests carry it.
func agentHost(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return uri
	}
	return u.Host
}
