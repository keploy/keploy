package proxy

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOrphanSession records the open/close calls trackOrphanWhileActive makes.
type fakeOrphanSession struct {
	mu     sync.Mutex
	opens  int
	closes int
	// starts records the instant each window claims to begin. Discarding
	// this argument would hide a window that opens LATER than the traffic
	// it is meant to cover, which is a silent hole in the suppression.
	starts []time.Time
}

func (f *fakeOrphanSession) OpenOrphanWindow(start time.Time) func() {
	f.mu.Lock()
	f.opens++
	f.starts = append(f.starts, start)
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.closes++
			f.mu.Unlock()
		})
	}
}

func (f *fakeOrphanSession) counts() (opens, closes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens, f.closes
}

func (f *fakeOrphanSession) lastStart() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) == 0 {
		return time.Time{}
	}
	return f.starts[len(f.starts)-1]
}

// bumped stamps "the connection just moved a byte".
func bumped() *atomic.Int64 {
	var n atomic.Int64
	n.Store(time.Now().UnixNano())
	return &n
}

// TestTrackOrphanWhileActiveFollowsTraffic pins the scoping that keeps a
// retired connection from suppressing the whole recording.
//
// A connection whose parser was retired can no longer be captured, but that
// only costs a test case if the app USED it while serving that test case.
// Holding one window from the fallthrough to end-of-session throws away every
// later test — on an early fallthrough, the entire run — even though the dead
// connection may have sat idle throughout. These tests pin that the window
// tracks activity instead.
func TestTrackOrphanWhileActiveFollowsTraffic(t *testing.T) {
	t.Parallel()

	const (
		grace = 40 * time.Millisecond
		check = 10 * time.Millisecond
	)

	t.Run("suppresses immediately, then stops once the connection goes idle", func(t *testing.T) {
		t.Parallel()
		sess := &fakeOrphanSession{}
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() { trackOrphanWhileActive(stop, sess, bumped(), grace, check); close(done) }()

		// A window must exist from the outset: the connection is broken now.
		waitFor(t, time.Second, func() bool { o, _ := sess.counts(); return o == 1 })

		// No traffic from here, so the hole must stop growing.
		waitFor(t, time.Second, func() bool { _, c := sess.counts(); return c == 1 })

		close(stop)
		<-done
		if o, c := sess.counts(); o != c {
			t.Fatalf("opens=%d closes=%d: every window must be closed on exit, or the "+
				"leftover one suppresses the rest of the session", o, c)
		}
	})

	t.Run("re-suppresses when traffic resumes on the dead connection", func(t *testing.T) {
		t.Parallel()
		sess := &fakeOrphanSession{}
		last := bumped()
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() { trackOrphanWhileActive(stop, sess, last, grace, check); close(done) }()

		// Let it go idle and close the first window.
		waitFor(t, time.Second, func() bool { _, c := sess.counts(); return c == 1 })

		// The app uses the connection again. It still cannot be captured, so
		// suppression must resume — otherwise these test cases ship mock-less.
		resume := time.Now()
		last.Store(resume.UnixNano())
		waitFor(t, time.Second, func() bool { o, _ := sess.counts(); return o == 2 })

		// The reopened window must cover the traffic that triggered it. The
		// tick only observes the resume up to checkEvery late, so opening at
		// tick time would leave that span uncovered — and a request served
		// entirely inside it is precisely the mock-less test case this is
		// meant to suppress.
		if start := sess.lastStart(); start.After(resume) {
			t.Fatalf("reopened at %v, after the resume at %v (gap %v): traffic in that gap is "+
				"left unsuppressed", start, resume, start.Sub(resume))
		}

		close(stop)
		<-done
		if o, c := sess.counts(); o != c {
			t.Fatalf("opens=%d closes=%d, want equal", o, c)
		}
	})

	t.Run("a connection that stays busy is suppressed without interruption", func(t *testing.T) {
		t.Parallel()
		sess := &fakeOrphanSession{}
		last := bumped()
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() { trackOrphanWhileActive(stop, sess, last, grace, check); close(done) }()

		// Keep it hot for several grace periods.
		deadline := time.Now().Add(6 * grace)
		for time.Now().Before(deadline) {
			last.Store(time.Now().UnixNano())
			time.Sleep(check / 2)
		}

		if o, c := sess.counts(); o != 1 || c != 0 {
			t.Fatalf("opens=%d closes=%d, want 1/0: a continuously busy connection must stay "+
				"covered by ONE window — churning it would leave gaps that let mock-less "+
				"test cases through", o, c)
		}

		close(stop)
		<-done
		if _, c := sess.counts(); c != 1 {
			t.Fatalf("closes=%d, want 1 after stop", c)
		}
	})
}

// waitFor polls cond until it holds or the timeout expires. Polling rather than
// sleeping a fixed span keeps these tests fast when the machine is idle and
// tolerant when CI is loaded.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
