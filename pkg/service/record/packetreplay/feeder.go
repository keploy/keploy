//go:build linux

package packetreplay

import "sync"

// responseFeeder feeds proxy→app payloads to the :16790 server in order.
type responseFeeder struct {
	mu    sync.Mutex
	queue [][]byte
	cond  *sync.Cond
	done  chan struct{}
}

func newResponseFeeder() *responseFeeder {
	r := &responseFeeder{done: make(chan struct{})}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *responseFeeder) push(p []byte) {
	r.mu.Lock()
	r.queue = append(r.queue, append([]byte(nil), p...))
	r.mu.Unlock()
	r.cond.Broadcast()
}

func (r *responseFeeder) pop(ctxDone <-chan struct{}) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for {
		if len(r.queue) > 0 {
			p := r.queue[0]
			r.queue = r.queue[1:]
			return p, true
		}
		// If either the server or the stream is shutting down, exit.
		select {
		case <-ctxDone:
			return nil, false
		case <-r.done:
			return nil, false
		default:
		}
		r.cond.Wait() // will be woken by push() or close()
	}
}

func (r *responseFeeder) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.done:
		// already closed
	default:
		close(r.done)
	}
	// Wake up any goroutines blocked in cond.Wait()
	r.cond.Broadcast()
}
