package manager

import (
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// recording_end.go: what the agent captures once its recording has ended.
//
// The agent outlives the recordings it serves: a sidecar's app goes on serving
// after its recording's stop, through the pod's termination, and the agent
// with it. Before, nothing ended its capture but its own exit: every request
// the app served after the stop was still made a test case, every call it
// made a mock, and every resolve a mapping, for streams whose client had read
// its end markers and gone. The mappings queued for that stream and then
// were lost, one ERROR each ("mapping stream closed with mappings still
// queued"), the mocks filled the dead mock stream's queue, and the parsers'
// work went to nothing. A recording's client that read all it was owed asks
// the agent to end its streams (routes' endRecordingStreams): from that
// instant the recording has ended (EndRecording), and the agent captures
// nothing of the application until a recording starts again
// (StartRecordSession, or a stream of a new recording: ResumeRecording).
// Its parsers go on reading their connections, so what a connection's later
// exchanges depend on (a database session's prepared statements, its
// authentication) is still known when a recording starts on it again; what
// they would record is refused (AddMock, SendConfigMock, the ingress's
// capture: conn.SkipEndedRecording), counted, and said once.

// recordingEnd is a manager's ended recording: on while none is, and what the
// agent left uncaptured since.
type recordingEnd struct {
	// on: the last recording has ended and none has started since.
	on atomic.Bool
	// stopAsked: the current recording's stop has asked what the agent
	// still holds from before it (BacklogBefore): a recording whose
	// streams all close after that has ended, however its client ended
	// them (StopWasAsked).
	stopAsked atomic.Bool
	// requests and calls the application served and made while on: the
	// requests no test case was made of, the calls no mock.
	requests, calls atomic.Uint64
	// said: what was left uncaptured since the last end has been said
	// (sayNotCaptured).
	said atomic.Bool
}

// Recording reports whether a recording takes what the agent captures: false
// once the last one has ended (EndRecording) until one starts. A nil manager
// gates nothing.
func (m *SyncMockManager) Recording() bool {
	return m == nil || !m.ended.on.Load()
}

// StopWasAsked reports whether the current recording's stop has asked what
// the agent still holds from before it (BacklogBefore): its streams' closing
// after that ends it (routes' streamEnding).
func (m *SyncMockManager) StopWasAsked() bool {
	return m != nil && m.ended.stopAsked.Load()
}

// NoteNotCaptured counts a request the application served that no recording
// took (Recording false): no test case was made of it.
func (m *SyncMockManager) NoteNotCaptured() {
	if m != nil {
		m.ended.requests.Add(1)
	}
}

// refuseEnded reports whether no recording takes a mock now (Recording
// false), counting it as a call left uncaptured. Nothing is said per mock:
// the counts are said once (sayNotCaptured).
func (m *SyncMockManager) refuseEnded() bool {
	if m == nil || !m.ended.on.Load() {
		return false
	}
	m.ended.calls.Add(1)
	return true
}

// EndRecording ends the current recording, once its client has read all the
// agent owed it from before its stop and asked for its streams' end markers
// (routes' endRecordingStreams), or its streams have all closed after its
// stop asked (StopWasAsked). From here the agent captures nothing until a
// recording starts: no test case, mock or mapping (Recording). Before letting
// go of the remaining buffer, it delivers late owned / startup / reusable
// mocks to their owners (FlushOwnedWindows) and rescues genuinely unowned
// read-only mocks preceding any still-open window's openFrom into the bounded
// session read-through tier (flushUnownedReadThrough). What it still holds
// for the ended recording is then let go, and nothing of it is said per item:
//   - every request still in flight is given up (NoteStop at now, then
//     EndStoppedRecording): none is the ended recording's any more, and none
//     is a later one's;
//   - the mocks buffered for a request that may still claim them: no request
//     will;
//   - the mapping stream is let go, and the mappings queued for it: each is
//     of a request made after the stop (the client asked only once nothing
//     from before it was owed: BacklogBefore), and no one reads that stream
//     past its marker. A mapping decided later finds no stream and is let go
//     (resolveRange, sendMapping), so no drainer waits on the ended stream
//     and says it lost them.
//
// It is called before the streams are told to end, so nothing the agent
// captures meanwhile reaches them after their markers. Idempotent.
func (m *SyncMockManager) EndRecording() {
	if m == nil || m.ended.on.Swap(true) {
		return
	}
	m.ended.requests.Store(0)
	m.ended.calls.Store(0)
	m.ended.said.Store(false)
	// Deliver any late owned / startup / reusable mocks to their owners first
	// (matching CloseOutChan), then rescue any genuinely unowned read-only
	// mocks still sitting in m.buffer (e.g. from sampled-out or filtered-out
	// cache-warming calls captured within StaleHorizon of stop) before clearing
	// m.buffer and before the caller closes the mock stream with its end marker.
	m.FlushOwnedWindows()
	m.flushUnownedReadThrough()
	m.NoteStop(time.Now())
	m.EndStoppedRecording()

	m.mu.Lock()
	mocks := len(m.buffer)
	clear(m.buffer)
	m.buffer = m.buffer[:0]
	m.keepSettledLocked(0, m.settled.at[:0])
	m.mappingChan = nil
	m.mappingOverflowMu.Lock()
	queued := m.mappingOverflow
	m.mappingOverflow = nil
	m.mappingDraining = false
	m.mappingStreamCh = nil
	m.mappingStreamCtx = nil
	// A drainer still blocked on the ended stream retires on its generation,
	// and the debts of what it and the queue held are forgiven: no stop waits
	// for them.
	m.mappingGen++
	m.forgiveMappingsBefore(m.mappingGen)
	m.mappingOverflowMu.Unlock()
	m.mu.Unlock()
	// No longer added: let go (SessionDrops).
	m.totalAdded.Add(-int64(mocks))
	m.dropLogger().Info("agent: the recording's client has ended its streams; the agent captures none of the application's traffic until a recording starts",
		zap.Int("buffered_mocks_let_go", mocks),
		zap.Int("queued_mappings_let_go", len(queued)))
}

// ResumeRecording ends an ended recording's state (EndRecording) as the next
// recording starts: its first stream attached (routes' streamEnding, a new
// recording's token) or its mock stream bound (StartRecordSession). It says
// what the agent left uncaptured meanwhile. A no-op when no recording had
// ended.
func (m *SyncMockManager) ResumeRecording() {
	if m == nil {
		return
	}
	m.ended.stopAsked.Store(false)
	if m.ended.on.Swap(false) {
		m.sayNotCaptured("a recording started")
	}
}

// SayNotCaptured says what the agent left uncaptured since its last recording
// ended, when one has and that is not said yet: at its stop (the proxy's
// CloseOutChan), as no recording will start.
func (m *SyncMockManager) SayNotCaptured() {
	if m != nil && m.ended.on.Load() {
		m.sayNotCaptured("the agent stopped")
	}
}

// sayNotCaptured says, once per ended recording, what the agent left
// uncaptured after it ended, until until: the requests the application served
// and the calls it made that no recording took.
func (m *SyncMockManager) sayNotCaptured(until string) {
	if m.ended.said.Swap(true) {
		return
	}
	m.dropLogger().Info("agent: no recording took the application's traffic after the last one ended",
		zap.String("until", until),
		zap.Uint64("requests_not_captured", m.ended.requests.Load()),
		zap.Uint64("calls_not_captured", m.ended.calls.Load()))
}
