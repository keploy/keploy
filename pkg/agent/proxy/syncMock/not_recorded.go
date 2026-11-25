package manager

import (
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// notRecordedFeedSize is how many requests the ingress did not record a
// session's feed holds for its stream to take (OpenNotRecordedFeed). The
// stream takes each as it comes; one the feed has no room for is counted
// instead (the closer's count), never waited for: the ingress is on the
// application's path.
const notRecordedFeedSize = 64

// notRecordedFeed is the open recording session's feed of the requests its
// ingress did not record as test cases (NoteNotRecorded), nil while no
// session is open.
type notRecordedFeed struct {
	mu      sync.Mutex
	ch      chan models.NotRecordedCall
	dropped uint64
}

// NoteNotRecorded hands the open recording session's stream c, a request
// the ingress served and did not record as a test case, for the stream to
// tell its client (models.NotRecordedPart): the agent's log names the
// request by its shape and its path tag, and the client's console prints
// its path beside the same tag. With no session open there is no client to
// tell, and c is dropped. It never blocks: a feed with no room counts c
// (OpenNotRecordedFeed's closer). Nil-safe.
func (m *SyncMockManager) NoteNotRecorded(c models.NotRecordedCall) {
	if m == nil {
		return
	}
	f := &m.notRecordedCalls
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ch == nil {
		return
	}
	select {
	case f.ch <- c:
	default:
		f.dropped++
	}
}

// OpenNotRecordedFeed opens a recording session's feed of the requests its
// ingress does not record as test cases (NoteNotRecorded), from now until
// close, which returns how many the feed had no room for. A session that
// opens later takes the feed over: a request noted then is the later
// session's, as the stream that attached last is the recording's. The
// channel is never closed: its stream stops reading it at close. Called as
// a recording's incoming stream opens (routes/record.go). Nil-safe: a nil
// manager's feed never carries one.
func (m *SyncMockManager) OpenNotRecordedFeed() (feed <-chan models.NotRecordedCall, closeFeed func() (dropped uint64)) {
	if m == nil {
		return nil, func() uint64 { return 0 }
	}
	ch := make(chan models.NotRecordedCall, notRecordedFeedSize)
	f := &m.notRecordedCalls
	f.mu.Lock()
	f.ch, f.dropped = ch, 0
	f.mu.Unlock()
	return ch, func() uint64 {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.ch != ch {
			return 0
		}
		f.ch = nil
		return f.dropped
	}
}
