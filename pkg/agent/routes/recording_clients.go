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
// stream open. A client closes its streams once its stop has drained them;
// one that dies closes them as its connections drop. Nothing else ends the
// wait: a stop signal may be a copy of the one before (utils.SetSignalHold),
// and an agent stopped another way (its API's stop, its escalation signal)
// stops without it.
func WaitForRecordingClients() {
	c := &recordingClients
	c.mu.Lock()
	gone := c.gone
	c.mu.Unlock()
	<-gone
}

// recordingStreamsEnd is what each recording's frame streams wait on to end
// (streamEnding), by the recording's token (models.StreamEndTokenHeader).
// open holds, for each token with a stream open, the channel its streams
// wait on, closed when its client asks the agent to end them
// (HandleEndStreams), and how many of its streams are open: a token whose
// streams all closed without the request (its client died) is forgotten.
// ended remembers the latest tokens ended, oldest first in endedOrder, so a
// stream its client reopens after the request (a reconnect) ends at once;
// endedKept of them, a bound that holds a few bytes each, not one per
// recording the agent ever served.
var recordingStreamsEnd struct {
	mu         sync.Mutex
	open       map[string]*openStreams
	ended      map[string]bool
	endedOrder []string
}

// openStreams is one recording's open frame streams: what they wait on to end,
// and how many there are.
type openStreams struct {
	end chan struct{}
	n   int
}

// endedKept is how many ended recordings the agent remembers (recordingStreamsEnd):
// far more than record at once on one agent, each reconnecting at most for a
// short while after its end.
const endedKept = 256

// streamEnding returns what closes when the client of the frame stream r
// opened asks the agent to end its recording's streams (models.StreamEndPath),
// and the func the stream calls once it closes: nil, which never does, for a
// stream with no recording token (a client that never asks).
func streamEnding(r *http.Request) (ending <-chan struct{}, closed func()) {
	tok := r.Header.Get(models.StreamEndTokenHeader)
	if tok == "" {
		return nil, func() {}
	}
	e := &recordingStreamsEnd
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ended[tok] {
		c := make(chan struct{})
		close(c)
		return c, func() {}
	}
	if e.open == nil {
		e.open = make(map[string]*openStreams)
	}
	o, ok := e.open[tok]
	if !ok {
		o = &openStreams{end: make(chan struct{})}
		e.open[tok] = o
	}
	o.n++
	var once sync.Once
	return o.end, func() {
		once.Do(func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			if o.n--; o.n == 0 && e.open[tok] == o {
				delete(e.open, tok)
			}
		})
	}
}

// endRecordingStreams ends every frame stream of the recording whose token is
// tok (streamEnding): each puts on the stream what it still has, then its end
// marker, and ends; one it reopens later ends at once.
func endRecordingStreams(tok string) {
	e := &recordingStreamsEnd
	e.mu.Lock()
	defer e.mu.Unlock()
	if o, ok := e.open[tok]; ok {
		close(o.end)
		delete(e.open, tok)
	}
	if e.ended[tok] {
		return
	}
	if e.ended == nil {
		e.ended = make(map[string]bool)
	}
	e.ended[tok] = true
	e.endedOrder = append(e.endedOrder, tok)
	if len(e.endedOrder) > endedKept {
		delete(e.ended, e.endedOrder[0])
		e.endedOrder = e.endedOrder[1:]
	}
}
