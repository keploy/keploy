package config

import (
	"runtime"
	"sync"
	"sync/atomic"
	"weak"
)

// agentAddresses holds, for each Config a Setup has given an agent address
// (SetAgentURI), the address it holds now. It is kept beside the Config, not
// in it: a field the agent's address moved in atomically would make every
// copy of a Config a copy of a lock (go vet's copylocks), and a pointer field
// would be shared by every copy, so a copy's agent set up later (the replay
// stack's) would move the address its original's readers read. A Config is
// held weakly, and its entry goes with it.
var agentAddresses sync.Map // weak.Pointer[Config] -> *atomic.Pointer[string]

// AgentURI is the address of this run's keploy agent: the one Setup chose
// for it, or the one a compose start moved it to when the engine refused to
// publish the one Setup chose (SetAgentURI); until a Setup has chosen one,
// Agent.AgentURI as the caller set it.
//
// It is the one way to read the agent's address. Agent.AgentURI is what the
// caller set, or what Setup last chose; a reader of the field does not see a
// move, and reads a string another goroutine may be writing. A reader that
// keeps the address (a health check's loop) reads it again at each use, so
// that a move is followed.
func (c *Config) AgentURI() string {
	if h, ok := agentAddresses.Load(weak.Make(c)); ok {
		if p := h.(*atomic.Pointer[string]).Load(); p != nil {
			return *p
		}
	}
	return c.Agent.AgentURI
}

// SetAgentURI makes uri the address AgentURI reads, safely while it is read.
// Only Setup, which chooses the agent's address, and the compose start that
// moves it off a port the engine refused to publish, set it.
func (c *Config) SetAgentURI(uri string) {
	h := new(atomic.Pointer[string])
	h.Store(&uri)
	key := weak.Make(c)
	held, loaded := agentAddresses.LoadOrStore(key, h)
	if loaded {
		held.(*atomic.Pointer[string]).Store(&uri)
		return
	}
	runtime.AddCleanup(c, func(key weak.Pointer[Config]) { agentAddresses.Delete(key) }, key)
}

// ResetAgentURI drops any address stored for c by SetAgentURI so AgentURI
// reads c.Agent.AgentURI again. Callers that zero and reuse a Config in place
// (such as a process-wide default singleton across tests) call this so a
// previous run's address does not survive the reset.
func (c *Config) ResetAgentURI() {
	if h, ok := agentAddresses.Load(weak.Make(c)); ok {
		h.(*atomic.Pointer[string]).Store(nil)
	}
}
