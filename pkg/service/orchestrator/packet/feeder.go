//go:build linux

package packet

import (
	"errors"
	"sync"
)

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

// func (r *responseFeeder) isEmpty() bool {
// 	r.mu.Lock()
// 	defer r.mu.Unlock()
// 	return len(r.queue) == 0
// }

// func (r *responseFeeder) waitUntilEmpty(ctx context.Context) error {
// 	r.mu.Lock()
// 	defer r.mu.Unlock()

// 	for !r.isEmpty() {
// 		select {
// 		case <-ctx.Done():
// 			return ctx.Err()
// 		case <-r.done:
// 			return nil
// 		default:
// 			r.cond.Wait()
// 		}
// 	}
// 	return nil
// }

// FeederManager manages multiple feeders identified by srcPort.
type FeederManager struct {
	mu       sync.Mutex
	cond     *sync.Cond
	feeders  map[uint16]*responseFeeder
	vacant   map[uint16]bool
	occupied map[uint16]bool
	closed   bool
}

func NewFeederManager() *FeederManager {
	m := &FeederManager{
		feeders:  make(map[uint16]*responseFeeder),
		vacant:   make(map[uint16]bool),
		occupied: make(map[uint16]bool),
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// GetOrCreate retrieves an existing feeder for srcPort, or creates a new one.
func (m *FeederManager) GetOrCreate(srcPort uint16) *responseFeeder {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}

	// If feeder exists, return it
	if f, ok := m.feeders[srcPort]; ok {
		return f
	}

	// Otherwise, create a new feeder
	f := newResponseFeeder()
	m.feeders[srcPort] = f
	m.vacant[srcPort] = true
	m.cond.Broadcast() // Notify waiting goroutines that a feeder is available
	return f
}

// Acquire blocks until an available feeder exists and returns it.
func (m *FeederManager) Acquire() (uint16, *responseFeeder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for {
		if m.closed {
			return 0, nil, errors.New("feeder manager closed")
		}

		for sp := range m.vacant {
			delete(m.vacant, sp)
			m.occupied[sp] = true
			return sp, m.feeders[sp], nil
		}

		m.cond.Wait() // Wait until a feeder becomes available
	}
}

// Release frees the feeder associated with srcPort and marks it as available.
func (m *FeederManager) Release(srcPort uint16) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if f, ok := m.feeders[srcPort]; ok {
		f.close() // Close the feeder and clean up.
		delete(m.feeders, srcPort)
	}
	delete(m.vacant, srcPort)
	delete(m.occupied, srcPort)
}

// Close shuts down the manager and closes all feeders.
func (m *FeederManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return
	}
	m.closed = true
	for _, f := range m.feeders {
		f.close() // Close all feeders
	}
	m.cond.Broadcast()
}
