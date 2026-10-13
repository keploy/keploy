package routes

import (
	"net/http"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// recording_clients.go counts the frame streams (test cases, mocks, mappings)
// a recording's client holds open on the agent and drains after its stop
// (holdForDrain), so a stop signal can wait for the client to finish with them
// (WaitForRecordingClients).
//
// A recording's client (the CLI) owns the agent's lifetime: when it stops it
// drains the agent's streams until the agent has handed over what it captured
// before the stop, and then stops the agent. The same signal often reaches the
// agent too: a `pkill -INT keploy`, a container stop. An agent that cancelled
// at once closed the streams the client was still draining, and what its
// parsers still held from before the stop was lost with it.

var recordingClients struct {
	mu sync.Mutex
	n  int
	// gone is closed when n falls to zero, and replaced when it rises from it.
	gone chan struct{}
}

func init() {
	recordingClients.gone = make(chan struct{})
	close(recordingClients.gone)
}

// holdForDrain notes the frame stream r opened, when its client drains it
// after the stop, closes it before it stops the agent, and is the only thing
// that stops the agent (models.DrainsAfterStopHeader), and returns the func
// that notes it closed. Any other stream (an agent docker compose or kubelet
// stops) is not held for: the agent stops on a signal as it always has.
func holdForDrain(r *http.Request) (detach func()) {
	if r.Header.Get(models.DrainsAfterStopHeader) != "true" {
		return func() {}
	}
	return attachRecordingClient()
}

// attachRecordingClient notes a frame stream a recording's client holds open,
// and returns the func that notes it closed.
func attachRecordingClient() (detach func()) {
	c := &recordingClients
	c.mu.Lock()
	if c.n == 0 {
		c.gone = make(chan struct{})
	}
	c.n++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.n--; c.n == 0 {
				close(c.gone)
			}
		})
	}
}

// RecordingClients is how many frame streams recordings' clients hold open.
func RecordingClients() int {
	c := &recordingClients
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// WaitForRecordingClients blocks while a recording's client holds a frame
// stream open, or until stopNow is closed. It reports whether every stream was
// closed. A client closes its streams once its stop has drained them; one that
// dies closes them as its connections drop.
func WaitForRecordingClients(stopNow <-chan struct{}) bool {
	c := &recordingClients
	c.mu.Lock()
	gone := c.gone
	c.mu.Unlock()
	select {
	case <-gone:
		return true
	case <-stopNow:
		return false
	}
}
