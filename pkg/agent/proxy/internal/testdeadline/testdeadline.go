// Package testdeadline derives event-wait hang guards solely from *testing.T's
// test binary deadline. Waiting on socket or channel events with only
// t.Deadline() as the bound prevents false failures on CPU-starved hosts while
// still reporting a clean test diagnostic before testing's binary-wide timeout
// panic when a regression causes a hang.
//
// The package is for tests only: nothing outside a _test.go file imports it.
package testdeadline

import (
	"net"
	"testing"
	"time"
)

// Bound derives a hang-guard deadline solely from t.Deadline() — a quarter of
// the time remaining, so N failing waits in one test binary consume at most
// 1-(3/4)^N of the timeout and fail with their own diagnostic before testing's
// binary-wide timeout panic. When the test has no deadline (for example
// go test -timeout 0), ok is false and waits have no wall-clock bound.
func Bound(t *testing.T) (time.Time, bool) {
	t.Helper()
	dl, ok := t.Deadline()
	if !ok {
		return time.Time{}, false
	}
	rem := time.Until(dl) / 4
	if rem < 0 {
		rem = 0
	}
	return time.Now().Add(rem), true
}

// Before returns a timer channel that fires at Bound(t) along with a stop
// function, or (nil, noop) when the test has no deadline.
func Before(t *testing.T) (<-chan time.Time, func()) {
	t.Helper()
	bound, ok := Bound(t)
	if !ok {
		return nil, func() {}
	}
	timer := time.NewTimer(time.Until(bound))
	return timer.C, func() { timer.Stop() }
}

// SetRead sets conn's read deadline to Bound(t), or clears it when the test
// has no deadline.
func SetRead(t *testing.T, conn net.Conn) {
	t.Helper()
	if bound, ok := Bound(t); ok {
		if err := conn.SetReadDeadline(bound); err != nil {
			t.Fatalf("set read deadline: %v", err)
		}
		return
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}
}

// SetWrite sets conn's write deadline to Bound(t), or clears it when the test
// has no deadline.
func SetWrite(t *testing.T, conn net.Conn) {
	t.Helper()
	if bound, ok := Bound(t); ok {
		if err := conn.SetWriteDeadline(bound); err != nil {
			t.Fatalf("set write deadline: %v", err)
		}
		return
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("clear write deadline: %v", err)
	}
}

// Join waits for done to close, bounded by Before(t). If done does not close
// before the bound, it records a test error naming the stuck goroutine instead
// of hanging until testing's binary-wide timeout panic.
func Join(t *testing.T, name string, done <-chan struct{}) {
	t.Helper()
	expired, stop := Before(t)
	defer stop()
	select {
	case <-done:
	case <-expired:
		t.Errorf("%s did not return during cleanup", name)
	}
}
