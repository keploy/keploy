// Package leakcheck finds what a handler leaves behind a connection it is
// done with: goroutines parked for good, and registrations on a context that
// outlives the connection (the ingress's, which lives as long as the
// recording).
//
// Parked counts goroutines parked in a wait, which is what a leaked goroutine
// is. A goroutine that is runnable, running or in a syscall when the stacks
// are read is on its way out: one that has signalled it is done (closed a
// channel, sent on one, called a WaitGroup's Done) can still be exiting when
// the test reads them, more often under -race on a loaded box. Counting those
// would make a row fail on scheduling, not on a leak. Nothing here reads a
// clock.
//
// Context counts the registrations made on it by context.AfterFunc and by the
// contexts derived from it. A handler that registers a close-on-cancel on the
// ingress's context for its own life leaves none behind once it returns; one
// that forgets to stop it leaves one per connection in the context's children,
// with a closure holding the connection, and no goroutine to show for it.
//
// The package is for tests only: nothing outside a _test.go file imports it.
package leakcheck

import (
	"context"
	"runtime"
	"strings"
	"sync"
)

// parkedForGood are the waits a goroutine can sit in for the rest of the
// process when nothing will wake it: a channel operation or select nobody
// will complete, a read on a socket nobody closes, a WaitGroup or Cond nobody
// signals. A goroutine is in a lock or the runtime's own waits (semacquire,
// GC assist) only on its way somewhere, so they are not counted.
var parkedForGood = []string{
	"chan receive",
	"chan send",
	"select",
	"IO wait",
	"sync.WaitGroup.Wait",
	"sync.Cond.Wait",
}

// Parked counts the goroutines parked in one of parkedForGood's waits whose
// stack, its frames or its "created by" line, names any of fns, and returns
// the first one's stack.
func Parked(fns ...string) (int, string) {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count, first := 0, ""
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !parked(g) || !names(g, fns) {
			continue
		}
		if count == 0 {
			first = g
		}
		count++
	}
	return count, first
}

// parked reads the state in a goroutine's header, "goroutine 7 [chan
// receive, 2 minutes]:", and says whether it is one of parkedForGood's.
func parked(g string) bool {
	header, _, _ := strings.Cut(g, "\n")
	_, state, ok := strings.Cut(header, "[")
	if !ok {
		return false
	}
	for _, w := range parkedForGood {
		if strings.HasPrefix(state, w) {
			return true
		}
	}
	return false
}

func names(g string, fns []string) bool {
	for _, fn := range fns {
		if strings.Contains(g, fn) {
			return true
		}
	}
	return false
}

// Context is a live context that counts what is registered on it.
// context.AfterFunc and context.WithCancel register on a parent that has an
// AfterFunc method through that method, where on a context of the context
// package they would add to its children; Live is the number of those not
// yet stopped. Cancel closes Done and runs the live registrations, each on
// its own goroutine, as the context package does.
type Context struct {
	context.Context
	done chan struct{}

	mu     sync.Mutex
	next   int
	live   map[int]func()
	cancel bool
}

// NewContext returns a live Context over parent's values. parent's own Done
// is not watched: the Context ends only when Cancel is called.
func NewContext(parent context.Context) *Context {
	return &Context{Context: parent, done: make(chan struct{}), live: map[int]func(){}}
}

func (c *Context) Done() <-chan struct{} { return c.done }

func (c *Context) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

// AfterFunc registers f to run when c is cancelled; stop unregisters it.
func (c *Context) AfterFunc(f func()) (stop func() bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel {
		go f()
		return func() bool { return false }
	}
	id := c.next
	c.next++
	c.live[id] = f
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, ok := c.live[id]; !ok {
			return false
		}
		delete(c.live, id)
		return true
	}
}

// Live is the number of registrations not yet stopped or run.
func (c *Context) Live() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.live)
}

// Cancel ends c and runs what is still registered on it.
func (c *Context) Cancel() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel {
		return
	}
	c.cancel = true
	close(c.done)
	for id, f := range c.live {
		delete(c.live, id)
		go f()
	}
}
