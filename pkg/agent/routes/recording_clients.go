package routes

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	syncmgr "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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

// streamClients counts open frame streams of one kind of recording client.
type streamClients struct {
	mu sync.Mutex
	n  int
	// gone is closed when n falls to zero, and replaced when it rises from it.
	gone chan struct{}
}

func newStreamClients() *streamClients {
	c := &streamClients{gone: make(chan struct{})}
	close(c.gone)
	return c
}

// attach notes a frame stream opened, and returns the func that notes it
// closed.
func (c *streamClients) attach() (detach func()) {
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

// count is how many of its streams are open.
func (c *streamClients) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// wait blocks while one of its streams is open.
func (c *streamClients) wait() {
	c.mu.Lock()
	gone := c.gone
	c.mu.Unlock()
	<-gone
}

// recordingClients are the streams whose client drains them after its stop
// and stops the agent itself (models.DrainsAfterStopHeader); noticeClients,
// those whose client drains them once the agent says a stop reached it,
// something else stopping the agent (models.DrainsOnAgentStopHeader: docker
// compose, with the app's project). noticeCarried is set once a notice
// client's stream has carried a frame.
var (
	recordingClients = newStreamClients()
	noticeClients    = newStreamClients()
	noticeCarried    atomic.Bool
)

// holdForDrain notes the frame stream r opened, when its client drains it
// after the stop, closes it before it stops the agent, and is the only thing
// that stops the agent (models.DrainsAfterStopHeader), or drains it once this
// agent says a stop reached it (models.DrainsOnAgentStopHeader), and returns
// the func that notes it closed. Any other stream (an agent kubelet stops, a
// client that does not drain) is not held for: the agent stops on a signal as
// it always has.
func holdForDrain(r *http.Request) (detach func()) {
	switch {
	case r.Header.Get(models.DrainsAfterStopHeader) == "true":
		return recordingClients.attach()
	case r.Header.Get(models.DrainsOnAgentStopHeader) == "true":
		return noticeClients.attach()
	}
	return func() {}
}

// attachRecordingClient notes a frame stream a recording's client holds open,
// draining it after its stop, and returns the func that notes it closed.
func attachRecordingClient() (detach func()) {
	return recordingClients.attach()
}

// carriedFrame notes that the frame stream r opened has taken a frame to carry
// to its client: a client that drains it once told of a stop then has
// something of this agent's to save (NoticeClientsHold). Noted as the stream
// takes the frame, before it writes it: the frame is no longer the agent's
// backlog then (BacklogBefore), and a stop that came while the stream wrote
// it found neither, and did not wait for the stream it was cut off on.
func carriedFrame(r *http.Request) {
	if r.Header.Get(models.DrainsOnAgentStopHeader) == "true" {
		noticeCarried.Store(true)
	}
}

// RecordingClients is how many frame streams recordings' clients hold open
// that they drain after their stop (models.DrainsAfterStopHeader).
func RecordingClients() int {
	return recordingClients.count()
}

// WaitForRecordingClients blocks while a recording's client holds a frame
// stream open. A client closes its streams once its stop has drained them;
// one that dies closes them as its connections drop. Nothing else ends the
// wait: a stop signal may be a copy of the one before (utils.SetSignalHold),
// and an agent stopped another way (its API's stop, its escalation signal)
// stops without it.
func WaitForRecordingClients() {
	recordingClients.wait()
}

// NoticeClientsHold reports whether a stop signal must wait for the clients
// that drain this agent's streams once told of the stop
// (models.DrainsOnAgentStopHeader): one holds a stream open, and this agent
// has something for it to save: its streams have carried a frame, or pending
// says the agent still holds one. An agent with nothing to hand over goes at
// once: under docker compose that is one whose app never ran, which keploy's
// retry of the project's `up` starts again (a recording's streams carry on on
// the agent compose starts), and which would only wait out compose's grace.
func NoticeClientsHold(pending func() bool) bool {
	if noticeClients.count() == 0 {
		return false
	}
	return noticeCarried.Load() || pending()
}

// HoldStop is what a stop signal this agent gets waits on (utils.SetSignalHold):
// the recording's clients that hold its frame streams open. One that drains
// them after its own stop (RecordingClients) is waited for until it closes
// them. One that drains them once told of the stop (NoticeClientsHold, asked
// of this agent's capture as the stop comes: whether it may still hand over
// something captured before it) is told (AnnounceStop) and waited for, but
// only when the agent has something for it to save: an agent with nothing
// goes at once, unannounced. Under docker compose that is one whose app never
// ran, which keploy's retry of the project's `up` starts again: an announced
// stop would have the recording drain, and end, before the app it records
// had started.
func HoldStop(logger *zap.Logger) func() {
	return holdStop(logger, func() bool { return syncmgr.Get().BacklogBefore(time.Now()).Pending })
}

// holdStop is HoldStop with what the agent's capture still holds asked of
// pending.
func holdStop(logger *zap.Logger, pending func() bool) func() {
	return func() {
		started, noticed := RecordingClients() > 0, NoticeClientsHold(pending)
		if !started && !noticed {
			return
		}
		if noticed {
			AnnounceStop()
		}
		logger.Info("a recording's client is still saving what this agent captured before the stop; the agent stops once it has finished")
		WaitForRecordingClients()
		if noticed {
			WaitForNoticeClients()
		}
	}
}

// WaitForNoticeClients blocks while a client that drains this agent's streams
// once told of the stop holds one open (models.DrainsOnAgentStopHeader). It
// closes them once its drain is through, or as its connections drop.
func WaitForNoticeClients() {
	noticeClients.wait()
}

// stopNoticeState is closed (announced) once a stop signal reached this agent
// and it waits for its notice clients (AnnounceStop).
type stopNoticeState struct {
	once      sync.Once
	announced chan struct{}
}

func newStopNotice() *stopNoticeState {
	return &stopNoticeState{announced: make(chan struct{})}
}

// stopNotice is this agent's: a stop reaches it at most once.
var stopNotice = newStopNotice()

// AnnounceStop tells the clients that drain this agent's streams once told
// of a stop (HandleStopNotice) that one has reached it, and that it waits for
// them to drain its streams.
func AnnounceStop() {
	s := stopNotice
	s.once.Do(func() { close(s.announced) })
}

// HandleStopNotice answers a recording's client (GET models.StopNoticePath)
// once a stop signal has reached this agent and it waits for the client to
// drain its streams (AnnounceStop): 204, at once if it has already. Until
// then the request stays open; it ends unanswered if the client goes, or the
// agent stops without waiting for it.
func (a *Agent) HandleStopNotice(w http.ResponseWriter, r *http.Request) {
	select {
	case <-stopNotice.announced:
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
	}
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
