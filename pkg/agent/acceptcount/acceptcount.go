// Package acceptcount keeps the sequence numbers of the connections a
// listener accepts, and lets a caller wait until every number up to a count
// has been accepted.
//
// The kernel numbers each connection completed to the proxy's marked listener
// as it counts it (the hooks' proxy_conns_established, and the number in the
// connection's own socket), and the proxy adds each accepted connection's
// number here. A connection can be queued before it is counted, so the
// numbers can arrive out of order; Accepted is the highest n such that every
// number from 1 to n has been accepted. A test's begin and end wait for it to
// reach the count they read (models.ProxyConnCounts): later connections cannot
// hold that wait back.
package acceptcount

import (
	"context"
	"fmt"
	"sync"
)

// Counter keeps accepted sequence numbers. The zero value is ready to use.
type Counter struct {
	mu sync.Mutex
	// upTo is the highest n with every number 1..n accepted; ahead holds
	// the numbers above upTo+1 accepted out of order.
	upTo  uint64
	ahead map[uint64]bool
	err   error
	// changed is closed, and replaced, when upTo moves and at Fail: a
	// waiter wakes on it.
	changed chan struct{}
}

// Add records one accepted connection's sequence number. A number of 0, or
// one already recorded, means the numbering is not what the kernel promised:
// the counter fails with that said.
func (c *Counter) Add(seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	switch {
	case seq == 0:
		c.failLocked(fmt.Errorf("an accepted connection had no sequence number"))
		return
	case seq <= c.upTo || c.ahead[seq]:
		c.failLocked(fmt.Errorf("sequence number %d was accepted twice", seq))
		return
	case seq > c.upTo+1:
		if c.ahead == nil {
			c.ahead = make(map[uint64]bool)
		}
		c.ahead[seq] = true
		return
	}
	c.upTo = seq
	for c.ahead[c.upTo+1] {
		delete(c.ahead, c.upTo+1)
		c.upTo++
	}
	c.wakeLocked()
}

// Fail says the numbers can no longer be kept (the listener closed, or a
// number could not be read), with err as why: every waiter returns it, now
// and from then on. The first call wins.
func (c *Counter) Fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failLocked(err)
}

func (c *Counter) failLocked(err error) {
	if c.err == nil {
		c.err = err
	}
	c.wakeLocked()
}

func (c *Counter) wakeLocked() {
	if c.changed != nil {
		close(c.changed)
		c.changed = nil
	}
}

// Accepted is the highest n such that every sequence number 1..n has been
// accepted, and Fail's error once it was called.
func (c *Counter) Accepted() (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.upTo, c.err
}

// WaitAccepted returns once every sequence number 1..n has been accepted, or
// with Fail's error, or with ctx's error when ctx ends first. It has no
// deadline of its own: every Add that moves Accepted, and Fail, wakes it.
func (c *Counter) WaitAccepted(ctx context.Context, n uint64) error {
	for {
		c.mu.Lock()
		if c.err != nil {
			err := c.err
			c.mu.Unlock()
			return err
		}
		if c.upTo >= n {
			c.mu.Unlock()
			return nil
		}
		if c.changed == nil {
			c.changed = make(chan struct{})
		}
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
