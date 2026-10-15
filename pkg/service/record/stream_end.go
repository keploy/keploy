package record

import (
	"context"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// streamEndedLog is what a drain says (at debug) when it has read its stream
// to the agent's end marker: the CLI's own record of it, which lanes check (an
// agent docker compose runs logs through compose, which can drop its last
// lines as the stack stops).
const streamEndedLog = "read the stream to the keploy agent's end marker"

// streamEnder is an agent client whose agent ends a recording client's frame
// streams on request, each with a marker once it has put on the stream all it
// had (AgentClient.EndRecordingStreams), and that says whether a stream ended
// so (StreamEnded).
type streamEnder interface {
	EndRecordingStreams(ctx context.Context, token string) error
	StreamEnded(kind models.FrameStream) bool
}

// streamsEnd is how a recording's stop ends the agent's frame streams when
// the agent can end them itself (models.AgentBacklog.EndMarkers): each frame
// drain (test cases, mocks) joins it as it starts, and once every drain that
// joined has been told "clear" by such an agent, or has ended, it asks the
// agent once to end its streams (maybeEnd). Each drain then reads its stream to
// the agent's marker, and the mapping drain to its own (MAPPINGS): the drains
// end on those events, every frame read, not on a stream's quiet, which a
// frame still on its way could outlast. An agent that cannot end its streams
// never says it can, and its drains end as before: on quiet, and a short
// grace once they close their streams.
type streamsEnd struct {
	// token names this recording's streams to the agent
	// (models.StreamEndTokenHeader).
	token string
	mu    sync.Mutex
	// open is the drains that joined and are neither clear nor ended.
	open map[models.FrameStream]bool
	// closes is each clear drain's own close of its stream: the fallback if
	// the agent does not take the request.
	closes []func()
	// asked: the agent has been asked (or that was tried); settled is closed
	// once the request has its outcome.
	asked   bool
	settled chan struct{}
	// requested: the agent took the request, so its streams end at their
	// markers; failed: it did not, and the clear drains' streams were closed
	// here instead.
	requested atomic.Bool
	failed    atomic.Bool
}

// join notes a frame drain of kind starting.
func (e *streamsEnd) join(kind models.FrameStream) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.open == nil {
		e.open = make(map[models.FrameStream]bool)
	}
	e.open[kind] = true
}

// clear notes a frame drain of kind told "clear" by an agent that ends its
// streams, with its own close of its stream, and asks the agent to end them
// once no drain is still open.
func (e *streamsEnd) clear(r *Recorder, ender streamEnder, kind models.FrameStream, closeStream func()) {
	e.mu.Lock()
	delete(e.open, kind)
	e.closes = append(e.closes, closeStream)
	e.mu.Unlock()
	e.maybeEnd(r, ender)
}

// left notes a frame drain of kind ending, however it did.
func (e *streamsEnd) left(r *Recorder, ender streamEnder, kind models.FrameStream) {
	e.mu.Lock()
	_, wasOpen := e.open[kind]
	delete(e.open, kind)
	e.mu.Unlock()
	if wasOpen {
		e.maybeEnd(r, ender)
	}
}

// maybeEnd asks the agent, once, to end its streams, when no drain is open
// and one is clear and waiting for its marker. The request runs on its own:
// the drains go on reading meanwhile, and a second interrupt still gives them
// up. No time bounds it: the agent's answer, or its connection failing, ends
// it.
func (e *streamsEnd) maybeEnd(r *Recorder, ender streamEnder) {
	e.mu.Lock()
	if len(e.open) > 0 || e.asked || len(e.closes) == 0 {
		e.mu.Unlock()
		return
	}
	e.asked = true
	e.settled = make(chan struct{})
	closes := e.closes
	e.mu.Unlock()
	go e.request(r, ender, closes)
}

// request asks the agent to end its streams (maybeEnd), and falls back to
// closing the clear drains' streams here if it does not take it.
func (e *streamsEnd) request(r *Recorder, ender streamEnder, closes []func()) {
	defer close(e.settled)
	err := ender.EndRecordingStreams(context.Background(), e.token)
	if err == nil {
		e.requested.Store(true)
		return
	}
	// The agent said it holds nothing more from before the stop: what is
	// still on its way to this client is what a stream's close can lose,
	// as with an agent that cannot end its streams.
	r.logger.Info("could not ask the keploy agent to end its streams; ending them here", zap.Error(err))
	e.failed.Store(true)
	for _, c := range closes {
		c()
	}
}

// endsByMarker waits for the request to end the agent's streams to have its
// outcome, if one was made, and reports whether the agent took it: its
// mapping stream then ends at its marker (MAPPINGS), not on quiet.
//
// done ends the wait, the drain it serves being over.
func (e *streamsEnd) endsByMarker(done <-chan struct{}) bool {
	e.mu.Lock()
	asked, settled := e.asked, e.settled
	e.mu.Unlock()
	if !asked {
		return false
	}
	select {
	case <-settled:
		return e.requested.Load()
	case <-utils.StopNow():
		// The user's second interrupt: the drains give up, and this one
		// with them (on quiet).
		return false
	case <-done:
		return false
	}
}
