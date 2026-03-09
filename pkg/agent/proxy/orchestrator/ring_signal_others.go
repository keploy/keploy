//go:build !linux

package orchestrator

import (
	"sync"
	"sync/atomic"
)

// ringSignal is the non-Linux fallback using a buffered channel.
// On Linux we use eventfd; on other platforms this provides efficient
// wakeup via Go's channel mechanism with conditional notification.
type ringSignal struct {
	ch      chan struct{}
	closeCh chan struct{}
	once    sync.Once
	waiting atomic.Bool
}

func newRingSignal() *ringSignal {
	return &ringSignal{
		ch:      make(chan struct{}, 1),
		closeCh: make(chan struct{}),
	}
}

// hasWaiter returns true if the consumer is blocked (or about to block).
func (s *ringSignal) hasWaiter() bool {
	return s.waiting.Load()
}

// setWaiting marks the consumer as about to block.
func (s *ringSignal) setWaiting() {
	s.waiting.Store(true)
}

// clearWaiting marks the consumer as active.
func (s *ringSignal) clearWaiting() {
	s.waiting.Store(false)
}

// notify wakes the consumer via a non-blocking channel send.
func (s *ringSignal) notify() {
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// wait blocks the consumer until signalled or closed.
func (s *ringSignal) wait() bool {
	select {
	case <-s.ch:
		return true
	case <-s.closeCh:
		return false
	}
}

// close shuts down the signal, waking any blocked consumer.
func (s *ringSignal) close() {
	s.once.Do(func() {
		close(s.closeCh)
	})
}
