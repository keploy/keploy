package models

import (
	"context"
	"sync"
)

// MockQueue implements an unbounded queue for mocks to prevent dropping during correlation.
// This queue is used during re-recording to ensure all mocks are properly correlated with tests,
// even under high traffic scenarios where mocks arrive faster than they can be processed.
type MockQueue struct {
	items  []*Mock
	mu     sync.Mutex
	cond   *sync.Cond
	closed bool
}

// NewMockQueue creates a new unbounded mock queue
func NewMockQueue() *MockQueue {
	q := &MockQueue{
		items: make([]*Mock, 0),
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Push adds a mock to the queue (never blocks, never drops)
func (q *MockQueue) Push(mock *Mock) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return // Don't accept new items after close
	}

	q.items = append(q.items, mock)
	q.cond.Signal() // Wake up any waiting consumer
}

// PopWithContext removes and returns a mock from the queue
// Blocks if empty until item available or context cancelled
func (q *MockQueue) PopWithContext(ctx context.Context) *Mock {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.items) == 0 && !q.closed {
		// Create done channel to handle context cancellation
		done := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				q.cond.Broadcast()
			case <-done:
			}
		}()

		q.cond.Wait()
		close(done)

		// Check if context was cancelled
		if ctx.Err() != nil {
			return nil
		}
	}

	// If closed and empty, return nil
	if len(q.items) == 0 {
		return nil
	}

	// Get first item (FIFO)
	mock := q.items[0]
	q.items = q.items[1:]

	return mock
}

// Len returns the current queue size
func (q *MockQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.items)
}

// Close signals that no more items will be added
func (q *MockQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.closed = true
	q.cond.Broadcast() // Wake up all waiting consumers
}
