//go:build linux

package linux

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// errReportClosed is a report's end when keploy closed its reader.
var errReportClosed = errors.New("keploy closed the report's reader")

// reportDrain lets a read of a watch's count wait until the watch has
// handled every report the kernel made before the read began. The read
// flushes the ring's reader (ringbuf.Reader.Flush); the reader then returns
// every report up to the ring's end, one it is still writing included, and
// then ringbuf.ErrFlushed; the watch, which handles one report at a time,
// calls flushed when it gets that. A report the kernel made before the flush
// has its room in the ring from then, so it has been handled. No clock: the
// reports' own order.
type reportDrain struct {
	// serial: one drain at a time, so a flush the reader answers once is
	// one drain's.
	serial sync.Mutex

	mu sync.Mutex
	// gen counts the flushes the watch got through.
	gen uint64
	// changed is closed, and replaced, at every flushed and at stop.
	changed chan struct{}
	// stopped is why the watch ended; nil while it runs.
	stopped error
	// waiting counts the drains waiting on the watch.
	waiting int
}

func newReportDrain() *reportDrain {
	return &reportDrain{changed: make(chan struct{})}
}

// flushed is the watch's: its reader returned a flush, after every report
// before it.
func (d *reportDrain) flushed() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gen++
	close(d.changed)
	d.changed = make(chan struct{})
}

// stop is the watch's, as it ends: why is errReportClosed when keploy closed
// its reader.
func (d *reportDrain) stop(why error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped == nil {
		d.stopped = why
		close(d.changed)
		d.changed = make(chan struct{})
	}
}

// failure is why the watch ended other than by keploy closing its reader:
// what it counts cannot be told from then on.
func (d *reportDrain) failure() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped == nil || errors.Is(d.stopped, errReportClosed) {
		return nil
	}
	return d.stopped
}

// wait flushes the reader and waits until the watch got through the flush.
// It ends early with ctx, or when the watch has ended: what it would have
// counted cannot be told.
func (d *reportDrain) wait(ctx context.Context, flush func() error) error {
	d.serial.Lock()
	defer d.serial.Unlock()
	d.mu.Lock()
	start, stopped := d.gen, d.stopped
	d.mu.Unlock()
	if stopped != nil {
		return fmt.Errorf("the report has ended: %w", stopped)
	}
	if err := flush(); err != nil {
		return fmt.Errorf("flush the report's ring: %w", err)
	}
	d.mu.Lock()
	d.waiting++
	defer func() {
		d.mu.Lock()
		d.waiting--
		d.mu.Unlock()
	}()
	for {
		gen, stopped, ch := d.gen, d.stopped, d.changed
		d.mu.Unlock()
		if gen != start {
			return nil
		}
		if stopped != nil {
			return fmt.Errorf("the report ended before it handled what the kernel had made: %w", stopped)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		d.mu.Lock()
	}
}
