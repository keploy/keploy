package manager

import (
	"context"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The feed of the requests the ingress did not record carries each to the
// open recording session's stream, in order; one noted while no session is
// open, or past the feed's room, is never waited for: the first is dropped,
// the second counted for the closer. A session that opens later takes the
// feed over, and the earlier session's closer leaves it be.
func TestTheNotRecordedFeedCarriesTheOpenSessionsRequests(t *testing.T) {
	m := New(zap.NewNop())
	call := func(i int) models.NotRecordedCall {
		p := "/r" + strconv.Itoa(i)
		return models.NotRecordedCall{Method: "GET", Path: p, PathTag: models.PathTag(context.Background(), "GET", p), Why: "test"}
	}
	m.NoteNotRecorded(call(-1)) // no session: no client to tell

	feed, closeFeed := m.OpenNotRecordedFeed()
	for i := range notRecordedFeedSize + 3 {
		m.NoteNotRecorded(call(i))
	}
	for i := range notRecordedFeedSize {
		if got := <-feed; got != call(i) {
			t.Fatalf("the feed's %dth is %+v, want %+v", i, got, call(i))
		}
	}
	select {
	case c := <-feed:
		t.Fatalf("the feed carried %+v past its room", c)
	default:
	}

	later, closeLater := m.OpenNotRecordedFeed()
	if n := closeFeed(); n != 0 {
		t.Fatalf("the earlier session's closer, after a later session took the feed over, counted %d", n)
	}
	m.NoteNotRecorded(call(100))
	if got := <-later; got != call(100) {
		t.Fatalf("the later session got %+v, want %+v", got, call(100))
	}
	if n := closeLater(); n != 0 {
		t.Fatalf("the later session's closer counted %d, want none", n)
	}
	m.NoteNotRecorded(call(101))
	select {
	case c := <-later:
		t.Fatalf("a closed feed carried %+v", c)
	default:
	}

	// Past the room: counted for the session's closer.
	_, closeFull := m.OpenNotRecordedFeed()
	for i := range notRecordedFeedSize + 3 {
		m.NoteNotRecorded(call(i))
	}
	if n := closeFull(); n != 3 {
		t.Fatalf("the closer counted %d past the feed's room, want 3", n)
	}

	var none *SyncMockManager
	none.NoteNotRecorded(call(0))
	if f, c := none.OpenNotRecordedFeed(); f != nil || c() != 0 {
		t.Fatal("a nil manager's feed is not empty")
	}
}
