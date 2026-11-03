package starts

import (
	"sync"
	"time"
)

// liveInstances are the keploy instances of the test workers live now
// (agent.InstanceValue). The agent makes one live before the kernel can
// stamp a connection with it (InstanceBegan) and ends it as its worker ends
// (InstanceEnded). A value is given to one claim only, so a stamp that is
// not live is an ended worker's (InstanceGone): its process is in no live
// worker, and its call is charged as one from a process in no worker is,
// never to a later claim of the same id. Unlike the rest of the registry it
// is not cleared by Reset: only the agent ends a worker, and it ends each
// one at its session's reset.
type liveInstances struct {
	mu   sync.Mutex
	live map[uint32]struct{}
}

// InstanceBegan makes keploy instance v a live worker's, before any of its
// processes can be in it.
func (r *Registry) InstanceBegan(v uint32) {
	l := &r.instances
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.live == nil {
		l.live = map[uint32]struct{}{}
	}
	l.live[v] = struct{}{}
}

// InstanceEnded ends keploy instance v: its worker ended, or its claim was
// not given. A call stamped with it whose request keploy reads from now on
// is from no live worker: each connection noted in it is marked with the
// end (connOrigin.endedAt), under the origins' lock, which NoteOriginIn
// holds as it reads whether v lives.
func (r *Registry) InstanceEnded(v uint32) {
	o := &r.origin
	o.mu.Lock()
	defer o.mu.Unlock()
	l := &r.instances
	l.mu.Lock()
	delete(l.live, v)
	l.mu.Unlock()
	now := time.Now()
	for _, c := range o.conns {
		if c.instance == v && c.endedAt.IsZero() {
			c.endedAt = now
		}
	}
}

// InstanceGone reports whether a connection stamped with keploy instance v
// is from no live worker although it names one: v is not 0 (no instance)
// and its worker ended, or v was never a live worker's here.
func (r *Registry) InstanceGone(v uint32) bool {
	if v == 0 {
		return false
	}
	l := &r.instances
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.live[v]
	return !ok
}
