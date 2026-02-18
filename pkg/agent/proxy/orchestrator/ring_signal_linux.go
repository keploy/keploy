//go:build linux

package orchestrator

import (
	"os"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// ringSignal uses Linux eventfd for goroutine wakeup with conditional
// notification.  The producer (forwarding goroutine) only issues the
// eventfd write syscall when the consumer (parser) is actually blocked,
// keeping the forwarding hot path syscall-free in the common case.
//
// Protocol (prevents missed wakeups):
//
//	Producer: rb.w.Store(w+n)  →  if sig.hasWaiter() { sig.notify() }
//	Consumer: check avail → setWaiting → re-check avail → wait → clearWaiting
type ringSignal struct {
	fd      int      // raw eventfd descriptor
	file    *os.File // wraps fd for Go netpoller integration
	once    sync.Once
	buf     [8]byte     // reused for eventfd reads
	waiting atomic.Bool // true when the consumer is about to park
}

func newRingSignal() *ringSignal {
	fd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		panic("orchestrator: eventfd creation failed: " + err.Error())
	}
	file := os.NewFile(uintptr(fd), "ring-eventfd")
	return &ringSignal{
		fd:   fd,
		file: file,
	}
}

// hasWaiter returns true if the consumer is blocked (or about to block).
// Called by the producer AFTER publishing new data.  Cost: ~1ns (atomic load).
func (s *ringSignal) hasWaiter() bool {
	return s.waiting.Load()
}

// setWaiting marks the consumer as about to block.  Must be followed by
// a re-check of ring buffer cursors before actually blocking.
func (s *ringSignal) setWaiting() {
	s.waiting.Store(true)
}

// clearWaiting marks the consumer as active (not blocked).
func (s *ringSignal) clearWaiting() {
	s.waiting.Store(false)
}

// notify wakes the consumer via eventfd write (~100-200ns).
// Only called when hasWaiter() is true, so the forwarding goroutine
// never pays this cost when the parser is keeping up.
func (s *ringSignal) notify() {
	var val [8]byte
	val[0] = 1 // uint64 little-endian = 1
	unix.Write(s.fd, val[:])
}

// wait blocks the consumer goroutine until signalled (or closed).
// The goroutine parks in Go's netpoller — no OS thread consumed.
// Returns true if woken normally, false if the eventfd was closed.
func (s *ringSignal) wait() bool {
	_, err := s.file.Read(s.buf[:])
	if err != nil {
		return false
	}
	return true
}

// close shuts down the eventfd, waking any parked consumer with an error.
func (s *ringSignal) close() {
	s.once.Do(func() {
		s.file.Close()
	})
}
