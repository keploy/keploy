package mock

import (
	"testing"
	"time"
)

// A case capture still storing cases after the stop is waited for past the
// bound: the bound is on its progress, not on the total. Before, it was a
// fixed wait, and the cases stored after it were left out of the recording.
func TestACaseCaptureStillStoringIsWaitedFor(t *testing.T) {
	const bound = 100 * time.Millisecond
	c := &caseCapture{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for i := 0; i < 15; i++ { // 15 x 20 ms: three bounds in all
			time.Sleep(20 * time.Millisecond)
			c.handled.Add(1)
		}
	}()
	c.wait(bound)
	select {
	case <-c.done:
	default:
		t.Fatal("gave up on a case capture that was still storing cases")
	}
}
