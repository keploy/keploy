package manager

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// resolveBench is a manager with ring windows resolved, one a millisecond for
// half a millisecond each from s0, and buffered mocks that none of them owns:
// one in the gap after each window, and the rest a nanosecond apart in the
// same gaps. All an hour from now, so the stale cutoff reaches none.
type resolveBench struct {
	mgr  *SyncMockManager
	out  chan *models.Mock
	s0   time.Time
	ring int
}

func newResolveBench(ring, buffered int) *resolveBench {
	rb := &resolveBench{out: make(chan *models.Mock, 1<<16), s0: time.Now().Add(time.Hour), ring: ring}
	rb.mgr = openWindowManager(rb.out, make(chan models.TestMockMapping, 1<<16))
	for i := range ring {
		at := rb.s0.Add(time.Duration(i) * time.Millisecond)
		rb.mgr.ResolveRange(at, at.Add(500*time.Microsecond), "r", true, false)
	}
	gaps := max(ring, 1)
	for i := range buffered {
		rb.mgr.AddMock(httpMockAt(rb.gap(i%gaps, i/gaps)))
	}
	return rb
}

// gap is the k-th instant in the gap after window i.
func (rb *resolveBench) gap(i, k int) time.Time {
	return rb.s0.Add(time.Duration(i)*time.Millisecond + 700*time.Microsecond + time.Duration(k))
}

// drain empties the output when it is near full.
func (rb *resolveBench) drain() {
	if len(rb.out) > cap(rb.out)/2 {
		sentMocks(rb.out)
	}
}

// BenchmarkResolveOverABufferAndAFullRing is a resolve while mocks no window
// owns wait buffered, the requests they were made for still in flight: each
// op adds a mock and resolves the window that holds it, which takes it, so the
// buffer stays as it is. The ring grows by one window an op until it is at its
// bound: run with a fixed count (-benchtime=20x) for the smaller rings.
func BenchmarkResolveOverABufferAndAFullRing(b *testing.B) {
	for _, buffered := range []int{1000, 10000, 100000} {
		for _, ring := range []int{0, 1000, maxRecentWindows} {
			b.Run(fmt.Sprintf("buffered=%d/ring=%d", buffered, ring), func(b *testing.B) {
				rb := newResolveBench(ring, buffered)
				// The first pass after the mocks were added decides each of
				// them; the ops time the passes after it.
				far := rb.s0.Add(24 * time.Hour)
				rb.mgr.ResolveRange(far, far, "warm", true, false)
				sentMocks(rb.out)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					at := rb.s0.Add(-time.Duration(i+1) * time.Millisecond)
					rb.mgr.AddMock(httpMockAt(at))
					rb.mgr.ResolveRange(at, at.Add(time.Microsecond), "t", true, false)
					rb.drain()
				}
				b.StopTimer()
				if n := len(rb.mgr.buffer); n != buffered {
					b.Fatalf("%d mocks buffered after the ops, want %d", n, buffered)
				}
			})
		}
	}
}

// BenchmarkAddMockBesideResolves is how long a parser's AddMock waits for the
// manager's lock while verdicts resolve on another goroutine, as fast as they
// come, with the ring full: the stall every parser of the app sees. Each add's
// mock is requested in a gap of the ring, so it stays buffered, as a request's
// mocks do while it is in flight: the buffer grows by one an add, and every
// resolve after it passes over the adds so far (on main, each of them against
// every window of the ring). It reports the slowest add and the 99th
// percentile. Run it with a fixed count (-benchtime=200x).
//
// It starts with the buffer empty. Rows with mocks buffered beforehand
// measured no wait even on main, a few microseconds at most: their adds ended
// before a resolve took the lock. So there are none.
func BenchmarkAddMockBesideResolves(b *testing.B) {
	b.Run(fmt.Sprintf("buffered=0/ring=%d", maxRecentWindows), func(b *testing.B) {
		rb := newResolveBench(maxRecentWindows, 0)
		far := rb.s0.Add(24 * time.Hour)
		rb.mgr.ResolveRange(far, far, "warm", true, false)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				at := far.Add(time.Duration(i+1) * time.Millisecond)
				rb.mgr.ResolveRange(at, at.Add(time.Microsecond), "v", true, false)
			}
		}()
		waits := make([]time.Duration, 0, b.N)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			// Requested in a gap of the ring: no window owns it, so it stays
			// buffered, as a request's mock still in flight does.
			mk := httpMockAt(rb.gap(i%maxRecentWindows, 1+i/maxRecentWindows))
			began := time.Now()
			rb.mgr.AddMock(mk)
			waits = append(waits, time.Since(began))
		}
		b.StopTimer()
		close(stop)
		wg.Wait()
		slices.Sort(waits)
		b.ReportMetric(float64(waits[len(waits)-1].Nanoseconds()), "max-ns/add")
		b.ReportMetric(float64(waits[len(waits)*99/100].Nanoseconds()), "p99-ns/add")
	})
}
